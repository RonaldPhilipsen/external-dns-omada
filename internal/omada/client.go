package omada

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	contentTypeJSON = "application/json"

	// lanDNSPageSize is the maximum page size the LAN DNS list endpoint
	// accepts; larger values are silently capped by the controller.
	lanDNSPageSize = 1000

	authPath = "/openapi/authorize/token"

	clientIDKey     = "client_id"
	clientSecretKey = "client_secret"

	pageKey     = "page"
	pageSizeKey = "pageSize"
)

// errNotAuthorized marks a 401 response from the Omada controller. The
// retry loop in do treats it as "token expired": refresh, re-authorize,
// and try the original request once more.
var errNotAuthorized = errors.New("omada: not authorized")

// omadaClient is a minimal client for the Omada controller OpenAPI. Token
// state is guarded by mu because the reconcile loop may call the client
// concurrently with a refresh triggered by a 401.
type omadaClient struct {
	mu           sync.Mutex
	baseURL      string
	omadaID      string
	siteID       string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	accessToken  string
	refreshToken string
	// lanNetworkIDs are the site's LAN networks, attached to every created
	// or updated entry.
	lanNetworkIDs []string
}

type tokenResult struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// omadaEnvelope is the common response wrapper of every OpenAPI endpoint.
type omadaEnvelope struct {
	ErrorCode int             `json:"errorCode"`
	Msg       string          `json:"msg"`
	Result    json.RawMessage `json:"result"`
}

type siteInfo struct {
	SiteID string `json:"siteId"`
	Name   string `json:"name"`
}

type sitesPage struct {
	Data []siteInfo `json:"data"`
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

type lanDNSPage struct {
	TotalRows   int      `json:"totalRows"`
	CurrentPage int      `json:"currentPage"`
	CurrentSize int      `json:"currentSize"`
	Data        []lanDNS `json:"data"`
}

type createLanDNSResult struct {
	ID string `json:"id"`
}

// lanNetwork is one entry of the site LAN network list at
// /openapi/v1/{omadacId}/sites/{siteId}/lan-networks.
type lanNetwork struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type lanNetworksResult struct {
	Data []lanNetwork `json:"data"`
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
	// client span; TLS behavior stays on the base transport.
	baseTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: tlsInsecureSkipVerify,
		},
	}
	cl := &omadaClient{
		baseURL:      base.String(),
		omadaID:      omadaID,
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient: &http.Client{
			Transport: otelhttp.NewTransport(baseTransport),
		},
	}
	if err := cl.authorize(ctx); err != nil {
		return nil, fmt.Errorf("omada: authorize: %w", err)
	}
	siteID, err := cl.resolveSite(ctx, siteName)
	if err != nil {
		return nil, err
	}
	cl.siteID = siteID
	if err := cl.resolveLanNetworks(ctx, networkNames); err != nil {
		return nil, err
	}
	return cl, nil
}

func (c *omadaClient) authorize(ctx context.Context) error {
	query := url.Values{"grant_type": {"client_credentials"}}
	payload := map[string]string{
		"omadacId":      c.omadaID,
		clientIDKey:     c.clientID,
		clientSecretKey: c.clientSecret,
	}
	req, err := c.newRequest(ctx, http.MethodPost, authPath, query, payload)
	if err != nil {
		return err
	}
	var env omadaEnvelope
	if err := c.send(req, &env); err != nil {
		return err
	}
	return c.storeTokenResult(env)
}

func (c *omadaClient) refresh(ctx context.Context) error {
	c.mu.Lock()
	rt := c.refreshToken
	c.mu.Unlock()
	if rt == "" {
		return errNotAuthorized
	}
	query := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		// The controller requires the client credentials both as query
		// parameters and in the body on refresh.
		clientIDKey:     {c.clientID},
		clientSecretKey: {c.clientSecret},
	}
	payload := map[string]string{
		clientIDKey:     c.clientID,
		clientSecretKey: c.clientSecret,
	}
	req, err := c.newRequest(ctx, http.MethodPost, authPath, query, payload)
	if err != nil {
		return err
	}
	var env omadaEnvelope
	if err := c.send(req, &env); err != nil {
		return err
	}
	return c.storeTokenResult(env)
}

func (c *omadaClient) storeTokenResult(env omadaEnvelope) error {
	var tr tokenResult
	if err := json.Unmarshal(env.Result, &tr); err != nil {
		return fmt.Errorf("omada: decode token response: %w", err)
	}
	c.mu.Lock()
	c.accessToken = tr.AccessToken
	c.refreshToken = tr.RefreshToken
	c.mu.Unlock()
	return nil
}

// resolveSite looks up the site ID for the configured site name.
func (c *omadaClient) resolveSite(ctx context.Context, siteName string) (string, error) {
	path := fmt.Sprintf("/openapi/v1/%s/sites", c.omadaID)
	query := url.Values{pageKey: {"1"}, pageSizeKey: {"100"}}
	var env omadaEnvelope
	if err := c.do(ctx, http.MethodGet, path, query, nil, &env); err != nil {
		return "", err
	}
	var page sitesPage
	if err := json.Unmarshal(env.Result, &page); err != nil {
		return "", fmt.Errorf("omada: decode sites response: %w", err)
	}
	for _, site := range page.Data {
		if site.Name == siteName {
			return site.SiteID, nil
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
	path := fmt.Sprintf("/openapi/v1/%s/sites/%s/lan-networks", c.omadaID, c.siteID)
	// The controller rejects list requests without pagination query
	// parameters (-1001).
	query := url.Values{pageKey: {"1"}, pageSizeKey: {"1000"}}
	var env omadaEnvelope
	if err := c.do(ctx, http.MethodGet, path, query, nil, &env); err != nil {
		return nil, err
	}
	var result lanNetworksResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, fmt.Errorf("omada: decode LAN networks: %w", err)
	}
	return result.Data, nil
}

// listLanDNS returns every LAN DNS entry of the configured site.
func (c *omadaClient) listLanDNS(ctx context.Context) ([]lanDNS, error) {
	path := c.lanDNSPath()
	entries := make([]lanDNS, 0)
	for page := 1; ; page++ {
		query := url.Values{
			pageKey:     {strconv.Itoa(page)},
			pageSizeKey: {strconv.Itoa(lanDNSPageSize)},
		}
		var env omadaEnvelope
		if err := c.do(ctx, http.MethodGet, path, query, nil, &env); err != nil {
			return nil, err
		}
		var pageResp lanDNSPage
		if err := json.Unmarshal(env.Result, &pageResp); err != nil {
			return nil, fmt.Errorf("omada: decode LAN DNS page: %w", err)
		}
		entries = append(entries, pageResp.Data...)
		if len(pageResp.Data) == 0 || len(pageResp.Data) < lanDNSPageSize || len(entries) >= pageResp.TotalRows {
			break
		}
	}
	return entries, nil
}

func (c *omadaClient) updateLanDNS(ctx context.Context, id string, dns lanDNS) error {
	dns.LanNetworkIDs = c.lanNetworkIDs
	path := fmt.Sprintf("%s/%s", c.lanDNSPath(), id)
	var env omadaEnvelope
	return c.do(ctx, http.MethodPatch, path, nil, dns, &env)
}

func (c *omadaClient) deleteLanDNS(ctx context.Context, id string) error {
	path := fmt.Sprintf("%s/%s", c.lanDNSPath(), id)
	var env omadaEnvelope
	return c.do(ctx, http.MethodDelete, path, nil, nil, &env)
}

func (c *omadaClient) createLanDNS(ctx context.Context, dns lanDNS) (string, error) {
	dns.LanNetworkIDs = c.lanNetworkIDs
	var env omadaEnvelope
	if err := c.do(ctx, http.MethodPost, c.lanDNSPath(), nil, dns, &env); err != nil {
		return "", err
	}
	// The controller answers successful creates without a result body, so
	// the entry ID is not always available; the provider re-discovers
	// entries from the list on the next read.
	if len(env.Result) == 0 {
		return "", nil
	}
	var res createLanDNSResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return "", fmt.Errorf("omada: decode created LAN DNS: %w", err)
	}
	return res.ID, nil
}

// do performs a request, and on a 401 refreshes the token (falling back to
// a full re-authorize) and retries the request once. If both re-auth
// attempts fail, the original 401 error is returned so callers can still
// match it with errors.Is(err, errNotAuthorized).
func (c *omadaClient) do(ctx context.Context, method, path string, query url.Values, payload any, result *omadaEnvelope) error {
	err := c.doOnce(ctx, method, path, query, payload, result)
	if err == nil || !errors.Is(err, errNotAuthorized) {
		return err
	}
	if err := c.refresh(ctx); err != nil {
		if authErr := c.authorize(ctx); authErr != nil {
			return err
		}
	}
	return c.doOnce(ctx, method, path, query, payload, result)
}

func (c *omadaClient) doOnce(ctx context.Context, method, path string, query url.Values, payload any, result *omadaEnvelope) error {
	req, err := c.newRequest(ctx, method, path, query, payload)
	if err != nil {
		return err
	}
	return c.send(req, result)
}

func (c *omadaClient) newRequest(ctx context.Context, method, path string, query url.Values, payload any) (*http.Request, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("omada: encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", contentTypeJSON)
	}
	return req, nil
}

// send executes the request and decodes the envelope. A non-zero
// errorCode is mapped to a plain error carrying the code and message; a
// 401 is mapped to errNotAuthorized regardless of the body.
func (c *omadaClient) send(req *http.Request, result *omadaEnvelope) error {
	c.mu.Lock()
	token := c.accessToken
	c.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "AccessToken="+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("omada: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errNotAuthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("omada: %s %s: unexpected status %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("omada: read response: %w", err)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("omada: decode response: %w", err)
	}
	if result.ErrorCode != 0 {
		return fmt.Errorf("omada error %d: %s", result.ErrorCode, result.Msg)
	}
	return nil
}

func (c *omadaClient) lanDNSPath() string {
	return fmt.Sprintf("/openapi/v1/%s/sites/%s/setting/lan/dns", c.omadaID, c.siteID)
}
