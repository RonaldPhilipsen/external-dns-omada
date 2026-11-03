package omada

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/RonaldPhilipsen/omada/cloud"
	omadalanc "github.com/RonaldPhilipsen/omada/cloud/lan"
	omadasite "github.com/RonaldPhilipsen/omada/cloud/site"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	contentTypeJSON = "application/json"

	// lanDNSPageSize is the maximum page size the LAN DNS list endpoint
	// accepts; larger values are silently capped by the controller.
	lanDNSPageSize = 1000

	// authPath is the controller's token endpoint, excluded from spans.
	authPath = "/openapi/authorize/token"
)

// omadaClient is a thin adapter over the github.com/RonaldPhilipsen/omada
// northbound client. It resolves the configured site and LAN networks at
// construction and exposes the LAN DNS operations the provider needs;
// authentication, token refresh and 401/-44112 retries are handled by the
// upstream client.
type omadaClient struct {
	cl *cloud.Client
	// omadaID is the Omada ID of the controller (OM-XXXXXXXX).
	omadaID string
	// siteID is the resolved ID of the configured site.
	siteID string
	// lanNetworkIDs are the site's LAN networks, attached to every created
	// or updated entry.
	lanNetworkIDs []string
}

// lanDNS mirrors the LAN DNS VO of the Omada controller. Type 0 is an IP
// (A/AAAA), 1 a CNAME, 2 a forward zone. Domain and Cname must not carry a
// trailing dot, and LanNetworkIDs must be non-empty on create and update.
type lanDNS struct {
	ID            string   `json:"id,omitempty"`
	Name          string   `json:"name,omitempty"`
	Domain        string   `json:"domain"`
	Type          int      `json:"type"`
	Enable        bool     `json:"enable"`
	IPAddresses   []string `json:"ipAddresses,omitempty"`
	IPv6Addresses []string `json:"ipv6Addresses,omitempty"`
	Cname         string   `json:"cname,omitempty"`
	LanNetworkIDs []string `json:"lanNetworkIds,omitempty"`
}

// lanNetwork is one entry of the site LAN network list at
// /openapi/v1/{omadacId}/sites/{siteId}/lan-networks.
type lanNetwork struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// newClient authenticates against the Omada controller, resolves the
// configured site name to its site ID, and selects the LAN networks managed
// entries are attached to.
func newClient(ctx context.Context, server, omadaID, clientID, clientSecret, siteName, networkNames string, tlsInsecureSkipVerify bool) (*omadaClient, error) {
	base, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("omada: invalid server URL %q: %w", server, err)
	}
	// otelhttp wraps the base transport so every API call produces a
	// client span; TLS behavior stays on the base transport. The auth
	// endpoint is excluded: its request carries client_secret, and span
	// attributes must never carry credentials.
	//
	// InsecureTLS is set on the base transport here rather than via
	// cloud.Options.InsecureTLS, because that option replaces a
	// caller-provided transport and would drop the otelhttp wrapper.
	baseTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: tlsInsecureSkipVerify,
		},
	}
	cl, err := cloud.New(cloud.Options{
		BaseURL:      base.String(),
		OmadacID:     omadaID,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		HTTPClient: &http.Client{
			Transport: otelhttp.NewTransport(baseTransport, otelhttp.WithFilter(
				func(r *http.Request) bool { return r.URL.Path != authPath })),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("omada: create client: %w", err)
	}
	c := &omadaClient{cl: cl, omadaID: omadaID}
	siteID, err := c.resolveSite(ctx, siteName)
	if err != nil {
		return nil, err
	}
	c.siteID = siteID
	if err := c.resolveLanNetworks(ctx, networkNames); err != nil {
		return nil, err
	}
	return c, nil
}

// envelope is the common response wrapper of every OpenAPI endpoint.
type envelope struct {
	ErrorCode int32  `json:"errorCode"`
	Msg       string `json:"msg"`
}

// decodeEnvelope reads the response body and maps a non-zero errorCode to a
// *cloud.APIError carrying the controller's code and message. The raw body
// is returned for callers that need the result payload.
func decodeEnvelope(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("omada: read response: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("omada: decode response: %w", err)
	}
	if env.ErrorCode != 0 {
		return nil, cloud.NewAPIError(int(env.ErrorCode), env.Msg)
	}
	return data, nil
}

// resolveSite looks up the site ID for the configured site name.
func (c *omadaClient) resolveSite(ctx context.Context, siteName string) (string, error) {
	resp, err := c.cl.Site().GetSiteList(ctx, c.omadaID, &omadasite.GetSiteListParams{Page: 1, PageSize: 100})
	if err != nil {
		return "", fmt.Errorf("omada: list sites: %w", err)
	}
	data, err := decodeEnvelope(resp)
	if err != nil {
		return "", err
	}
	var env omadasite.OperationResponseGridVOSiteSummaryInfo
	if err := json.Unmarshal(data, &env); err != nil {
		return "", fmt.Errorf("omada: decode sites response: %w", err)
	}
	if env.Result != nil && env.Result.Data != nil {
		for _, s := range *env.Result.Data {
			if s.SiteId != nil && s.Name != nil && *s.Name == siteName {
				return *s.SiteId, nil
			}
		}
	}
	return "", fmt.Errorf("omada: site %q not found", siteName)
}

// resolveLanNetworks loads the site's LAN networks and selects the ones
// managed entries are attached to. Every LAN DNS entry must be attached to
// at least one network, and the controller rejects creates and updates
// without them, so a site without any LAN network is not manageable.
func (c *omadaClient) resolveLanNetworks(ctx context.Context, names string) error {
	networks, err := c.listLanNetworks(ctx)
	if err != nil {
		return err
	}
	if len(networks) == 0 {
		return fmt.Errorf("omada: site %s has no LAN networks; add one at Network Config > Network Settings > LAN first", c.siteID)
	}
	ids, err := selectLanNetworks(networks, names)
	if err != nil {
		return err
	}
	c.lanNetworkIDs = ids
	return nil
}

// selectLanNetworks maps a comma-separated list of LAN network names to
// network IDs in site order. Names are matched case-insensitively, and an
// unknown name is an error listing the available networks; ignoring one
// silently would narrow the visibility of every managed entry. An empty list
// selects every network of the site.
func selectLanNetworks(networks []lanNetwork, names string) ([]string, error) {
	byName := make(map[string]string, len(networks))
	available := make([]string, 0, len(networks))
	for _, n := range networks {
		available = append(available, n.Name)
		if byName[strings.ToLower(n.Name)] == "" {
			byName[strings.ToLower(n.Name)] = n.ID
		}
	}
	wanted := make(map[string]bool)
	for raw := range strings.SplitSeq(names, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		id, ok := byName[strings.ToLower(name)]
		if !ok {
			return nil, fmt.Errorf("omada: LAN network %q not found in site; available networks: %s", name, strings.Join(available, ", "))
		}
		wanted[id] = true
	}
	ids := make([]string, 0, len(networks))
	for _, n := range networks {
		if n.ID != "" && (len(wanted) == 0 || wanted[n.ID]) {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("omada: site has no selectable LAN networks")
	}
	return ids, nil
}

// listLanNetworks returns the LAN networks of the configured site.
func (c *omadaClient) listLanNetworks(ctx context.Context) ([]lanNetwork, error) {
	resp, err := c.cl.Lan().GetLanNetworkList(ctx, c.omadaID, c.siteID, &omadalanc.GetLanNetworkListParams{Page: 1, PageSize: 1000})
	if err != nil {
		return nil, fmt.Errorf("omada: list LAN networks: %w", err)
	}
	data, err := decodeEnvelope(resp)
	if err != nil {
		return nil, err
	}
	var env omadalanc.OperationResponseLanNetworkOpenApiGridVOLanNetworkQueryOpenApiVO
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("omada: decode LAN networks: %w", err)
	}
	var out []lanNetwork
	if env.Result != nil && env.Result.Data != nil {
		for _, n := range *env.Result.Data {
			id := ""
			if n.Id != nil {
				id = *n.Id
			}
			out = append(out, lanNetwork{ID: id, Name: n.Name})
		}
	}
	return out, nil
}

// listLanDNS returns every LAN DNS entry of the configured site.
func (c *omadaClient) listLanDNS(ctx context.Context) ([]lanDNS, error) {
	entries := make([]lanDNS, 0)
	for page := 1; ; page++ {
		resp, err := c.cl.Lan().GetGridLanDns(ctx, c.omadaID, c.siteID, &omadalanc.GetGridLanDnsParams{Page: int32(page), PageSize: lanDNSPageSize})
		if err != nil {
			return nil, fmt.Errorf("omada: list LAN DNS: %w", err)
		}
		data, err := decodeEnvelope(resp)
		if err != nil {
			return nil, err
		}
		var env omadalanc.OperationResponseLanDnsGridVOLanDnsQueryOpenApiVO
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("omada: decode LAN DNS page: %w", err)
		}
		var rows []omadalanc.LanDnsQueryOpenApiVO
		total := 0
		if env.Result != nil {
			if env.Result.Data != nil {
				rows = *env.Result.Data
			}
			if env.Result.TotalRows != nil {
				total = int(*env.Result.TotalRows)
			}
		}
		for _, r := range rows {
			entries = append(entries, toLanDNS(r))
		}
		if len(rows) == 0 || len(rows) < int(lanDNSPageSize) || len(entries) >= total {
			break
		}
	}
	return entries, nil
}

func (c *omadaClient) updateLanDNS(ctx context.Context, id string, dns lanDNS) error {
	dns.LanNetworkIDs = c.lanNetworkIDs
	resp, err := c.cl.Lan().ModifyLanDns(ctx, c.omadaID, c.siteID, id, toLanDnsVO(dns))
	if err != nil {
		return fmt.Errorf("omada: update LAN DNS: %w", err)
	}
	_, err = decodeEnvelope(resp)
	return err
}

func (c *omadaClient) deleteLanDNS(ctx context.Context, id string) error {
	resp, err := c.cl.Lan().DeleteLanDns(ctx, c.omadaID, c.siteID, id)
	if err != nil {
		return fmt.Errorf("omada: delete LAN DNS: %w", err)
	}
	_, err = decodeEnvelope(resp)
	return err
}

func (c *omadaClient) createLanDNS(ctx context.Context, dns lanDNS) (string, error) {
	dns.LanNetworkIDs = c.lanNetworkIDs
	resp, err := c.cl.Lan().CreateLanDns(ctx, c.omadaID, c.siteID, toLanDnsVO(dns))
	if err != nil {
		return "", fmt.Errorf("omada: create LAN DNS: %w", err)
	}
	data, err := decodeEnvelope(resp)
	if err != nil {
		return "", err
	}
	// The controller answers successful creates without a result body, so
	// the entry ID is not always available; the provider re-discovers
	// entries from the list on the next read.
	var res struct {
		Result *struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return "", fmt.Errorf("omada: decode created LAN DNS: %w", err)
	}
	if res.Result == nil {
		return "", nil
	}
	return res.Result.ID, nil
}

// toLanDnsVO renders the internal entry shape as the generated request VO.
func toLanDnsVO(dns lanDNS) omadalanc.LanDnsOpenApiVO {
	vo := omadalanc.LanDnsOpenApiVO{
		Domain: dns.Domain,
		Name:   dns.Name,
		Type:   int32(dns.Type),
		Enable: dns.Enable,
	}
	if len(dns.IPAddresses) > 0 {
		ips := append([]string(nil), dns.IPAddresses...)
		vo.IpAddresses = &ips
	}
	if len(dns.IPv6Addresses) > 0 {
		ips := append([]string(nil), dns.IPv6Addresses...)
		vo.Ipv6Addresses = &ips
	}
	if dns.Cname != "" {
		cname := dns.Cname
		vo.Cname = &cname
	}
	if len(dns.LanNetworkIDs) > 0 {
		nets := append([]string(nil), dns.LanNetworkIDs...)
		vo.LanNetworkIds = &nets
	}
	return vo
}

// toLanDNS maps a generated grid row to the internal entry shape.
func toLanDNS(vo omadalanc.LanDnsQueryOpenApiVO) lanDNS {
	e := lanDNS{
		Name:   vo.Name,
		Domain: vo.Domain,
		Type:   int(vo.Type),
		Enable: vo.Enable,
	}
	if vo.Id != nil {
		e.ID = *vo.Id
	}
	if vo.IpAddresses != nil {
		e.IPAddresses = *vo.IpAddresses
	}
	if vo.Ipv6Addresses != nil {
		e.IPv6Addresses = *vo.Ipv6Addresses
	}
	if vo.Cname != nil {
		e.Cname = *vo.Cname
	}
	if vo.LanNetworkIds != nil {
		e.LanNetworkIDs = *vo.LanNetworkIds
	}
	return e
}
