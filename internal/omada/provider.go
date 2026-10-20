package omada

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	"github.com/RonaldPhilipsen/external-dns-omada/internal/sets"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

const (
	entryIDProperty       = "Omada/entry.id"
	entryEnableProperty   = "Omada/entry.enable"
	entryNetworksProperty = "Omada/entry.network.ids"
	maxIPTargets          = 32

	// The attribute keys recorded on the omada.dns.* instruments.
	dnsOperationAttribute = "omada.dns.operation"
	dnsErrorAttribute     = "omada.dns.error"
)

// Config carries everything the provider needs to manage an Omada site.
type Config struct {
	// Server is the base URL of the Omada controller, e.g. https://192.168.1.1:8043.
	Server string
	// ID is the Omada ID of the controller (OM-XXXXXXXX).
	ID string
	// ClientID is the ID of the custom app used for client-credentials authentication.
	ClientID string
	// ClientSecret is the secret of the custom app.
	ClientSecret string
	// SiteName is the name of the site whose LAN DNS entries are managed.
	SiteName string
	// Networks is a comma-separated list of LAN network names managed entries
	// must be attached to; an empty value attaches entries to all of the
	// site's LAN networks.
	Networks string
	// TLSInsecureSkipVerify disables verification of controller TLS certificates.
	TLSInsecureSkipVerify bool
	// DryRun reports what would change without applying it.
	DryRun bool
	// DomainFilter is the list of domain suffixes this instance manages; an
	// empty filter matches every record. The same filter is reported to
	// external-dns during webhook negotiation, so keep it in sync with what
	// this instance actually manages.
	DomainFilter []string
}

// omadaAPI is the subset of the Omada client the provider operates on, so
// tests can substitute a fake.
type omadaAPI interface {
	listLanDNS(ctx context.Context) ([]lanDNS, error)
	createLanDNS(ctx context.Context, dns lanDNS) (string, error)
	updateLanDNS(ctx context.Context, id string, dns lanDNS) error
	deleteLanDNS(ctx context.Context, id string) error
}

// Provider syncs DNS records with the LAN DNS entries of an Omada site.
type Provider struct {
	provider.BaseProvider
	api          omadaAPI
	domainFilter endpoint.DomainFilterInterface
	dryRun       bool
	siteName     string
	// lanNetworkIDs is the set of LAN networks managed entries must be
	// attached to; an entry on a different set is rebound on update.
	lanNetworkIDs sets.Set[string]
	// writeInterval spaces out write requests so a batch of changes does
	// not trip the controller's write rate limit.
	writeInterval  time.Duration
	tel            Telemetry
	entriesGauge   metric.Int64Gauge
	changesCounter metric.Int64Counter
	errorsCounter  metric.Int64Counter
}

// New creates a Provider from the given configuration and telemetry wiring.
// It authenticates against the controller and resolves the site and LAN
// networks, so a misconfiguration fails at startup instead of on the first
// reconcile.
func New(ctx context.Context, cfg Config, tel Telemetry) (*Provider, error) {
	server, err := validateConfig(cfg)
	if err != nil {
		return nil, err
	}
	c, err := newClient(ctx, server, cfg.ID, cfg.ClientID, cfg.ClientSecret, cfg.SiteName, cfg.Networks, cfg.TLSInsecureSkipVerify)
	if err != nil {
		return nil, err
	}
	networks := sets.New[string]()
	for _, id := range c.lanNetworkIDs {
		networks.Insert(id)
	}
	p := &Provider{
		api:           c,
		domainFilter:  endpoint.NewDomainFilter(cfg.DomainFilter),
		dryRun:        cfg.DryRun,
		siteName:      cfg.SiteName,
		lanNetworkIDs: networks,
		tel:           tel,
		writeInterval: 500 * time.Millisecond,
	}
	if err := p.initInstruments(); err != nil {
		return nil, fmt.Errorf("initializing omada telemetry instruments: %w", err)
	}
	return p, nil
}

// initInstruments creates the three omada.dns.* instruments on the
// provider's meter. It must run before Records or ApplyChanges is called.
func (p *Provider) initInstruments() error {
	var err error
	if p.entriesGauge, err = p.tel.Meter.Int64Gauge("omada.dns.entries",
		// The gauge counts the entries Records reports, i.e. the entries
		// this instance manages after applying its domain filter.
		metric.WithDescription("Number of Omada LAN DNS entries managed by this instance"),
		metric.WithUnit("{entry}")); err != nil {
		return err
	}
	if p.changesCounter, err = p.tel.Meter.Int64Counter("omada.dns.changes",
		metric.WithDescription("DNS changes handled by the provider, by operation outcome"),
		metric.WithUnit("{change}")); err != nil {
		return err
	}
	if p.errorsCounter, err = p.tel.Meter.Int64Counter("omada.dns.errors",
		metric.WithDescription("Errors while syncing DNS records with the Omada site, by category"),
		metric.WithUnit("{error}")); err != nil {
		return err
	}
	return nil
}

func validateConfig(cfg Config) (string, error) {
	server := strings.TrimRight(cfg.Server, " /")
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("omada: omada-server must be an http(s) URL, got %q", cfg.Server)
	}
	var missing []string
	if cfg.ID == "" {
		missing = append(missing, "omada-id")
	}
	if cfg.ClientID == "" {
		missing = append(missing, "omada-client-id")
	}
	if cfg.ClientSecret == "" {
		missing = append(missing, "omada-client-secret")
	}
	if cfg.SiteName == "" {
		missing = append(missing, "omada-site-name")
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("omada: missing required configuration: %s", strings.Join(missing, ", "))
	}
	return server, nil
}

// LanNetworkIDs returns the IDs of the LAN networks managed entries are
// attached to, in site order.
func (p *Provider) LanNetworkIDs() []string {
	return sets.Sorted(p.lanNetworkIDs)
}

// GetDomainFilter returns the domain filter the provider was created with.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface {
	return p.domainFilter
}

// Records returns the current Omada LAN DNS entries as endpoints.
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	entries, err := p.api.listLanDNS(ctx)
	if err != nil {
		p.errorsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(dnsErrorAttribute, "list")))
		return nil, provider.NewSoftErrorf("omada: listing LAN DNS entries: %v", err)
	}
	var endpoints []*endpoint.Endpoint
	for _, e := range entries {
		if !p.domainFilter.Match(e.Domain) {
			continue
		}
		switch e.Type {
		case 0:
			if len(e.IPAddresses) == 0 && len(e.IPv6Addresses) == 0 {
				log.Warnf("omada: LAN DNS entry %q (%s) has no IP addresses, skipping", e.Name, e.Domain)
				continue
			}
			if len(e.IPAddresses) > 0 {
				endpoints = append(endpoints, withEntryProperties(
					endpoint.NewEndpointWithTTL(e.Domain, endpoint.RecordTypeA, 0, e.IPAddresses...), e))
			}
			if len(e.IPv6Addresses) > 0 {
				endpoints = append(endpoints, withEntryProperties(
					endpoint.NewEndpointWithTTL(e.Domain, endpoint.RecordTypeAAAA, 0, e.IPv6Addresses...), e))
			}
		case 1:
			if e.Cname == "" {
				log.Warnf("omada: LAN DNS entry %q (%s) has no CNAME target, skipping", e.Name, e.Domain)
				continue
			}
			endpoints = append(endpoints, withEntryProperties(
				endpoint.NewEndpointWithTTL(e.Domain, endpoint.RecordTypeCNAME, 0, e.Cname), e))
		case 2:
			log.Warnf("omada: LAN DNS entry %q (%s) is a forwarder, not managed by external-dns", e.Name, e.Domain)
		}
	}
	p.entriesGauge.Record(ctx, int64(len(endpoints)))
	return endpoints, nil
}

func withEntryProperties(ep *endpoint.Endpoint, e lanDNS) *endpoint.Endpoint {
	ep.SetProviderSpecificProperty(entryIDProperty, e.ID)
	ep.SetProviderSpecificProperty(entryEnableProperty, strconv.FormatBool(e.Enable))
	ep.SetProviderSpecificProperty(entryNetworksProperty, strings.Join(e.LanNetworkIDs, ","))
	return ep
}

type lanDNSState struct {
	entryID    string
	ipv4s      sets.Set[string]
	ipv6s      sets.Set[string]
	cname      string
	enable     bool
	networkIDs sets.Set[string]
}

func newLanDNSState() *lanDNSState {
	return &lanDNSState{ipv4s: sets.New[string](), ipv6s: sets.New[string](), networkIDs: sets.New[string](), enable: true}
}

// records returns the endpoints to derive current state from, preferring the
// records the controller already fetched this cycle.
func (p *Provider) records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	if v := ctx.Value(provider.RecordsContextKey); v != nil {
		if eps, ok := v.([]*endpoint.Endpoint); ok {
			return eps, nil
		}
	}
	return p.Records(ctx)
}

func (p *Provider) currentState(ctx context.Context) (map[string]*lanDNSState, error) {
	records, err := p.records(ctx)
	if err != nil {
		return nil, err
	}
	state := make(map[string]*lanDNSState)
	for _, ep := range records {
		var s *lanDNSState
		switch ep.RecordType {
		case endpoint.RecordTypeA:
			s = stateOf(state, ep.DNSName)
			for _, t := range ep.Targets {
				s.ipv4s.Insert(t)
			}
		case endpoint.RecordTypeAAAA:
			s = stateOf(state, ep.DNSName)
			for _, t := range ep.Targets {
				s.ipv6s.Insert(t)
			}
		case endpoint.RecordTypeCNAME:
			s = stateOf(state, ep.DNSName)
			if len(ep.Targets) > 0 {
				s.cname = ep.Targets[0]
			}
		default:
			continue
		}
		if id, ok := ep.GetProviderSpecificProperty(entryIDProperty); ok {
			s.entryID = id
		}
		if enable, ok := ep.GetBoolProviderSpecificProperty(entryEnableProperty); ok {
			s.enable = enable
		}
		if networks, ok := ep.GetProviderSpecificProperty(entryNetworksProperty); ok {
			for id := range strings.SplitSeq(networks, ",") {
				if id != "" {
					s.networkIDs.Insert(id)
				}
			}
		}
	}
	return state, nil
}

func stateOf(state map[string]*lanDNSState, fqdn string) *lanDNSState {
	s := state[fqdn]
	if s == nil {
		s = newLanDNSState()
		state[fqdn] = s
	}
	return s
}

type desiredState struct {
	ipv4s  sets.Set[string]
	ipv6s  sets.Set[string]
	cnames sets.Set[string]
}

func newDesiredState() *desiredState {
	return &desiredState{ipv4s: sets.New[string](), ipv6s: sets.New[string](), cnames: sets.New[string]()}
}

func desiredFromChanges(changes *plan.Changes) map[string]*desiredState {
	desired := make(map[string]*desiredState)
	add := func(eps []*endpoint.Endpoint) {
		for _, ep := range eps {
			d := desired[ep.DNSName]
			if d == nil {
				d = newDesiredState()
				desired[ep.DNSName] = d
			}
			for _, t := range ep.Targets {
				switch ep.RecordType {
				case endpoint.RecordTypeA:
					d.ipv4s.Insert(t)
				case endpoint.RecordTypeAAAA:
					d.ipv6s.Insert(t)
				case endpoint.RecordTypeCNAME:
					d.cnames.Insert(t)
				}
			}
		}
	}
	add(changes.Create)
	add(changes.UpdateNew)
	for _, ep := range changes.Delete {
		desired[ep.DNSName] = newDesiredState()
	}
	return desired
}

func validateDesired(fqdn string, d *desiredState) []error {
	var errs []error
	if strings.Contains(fqdn, "*") {
		errs = append(errs, provider.NewSoftErrorf("omada: wildcard name %q is not supported", fqdn))
	}
	if (len(d.ipv4s) > 0 || len(d.ipv6s) > 0) && len(d.cnames) > 0 {
		errs = append(errs, provider.NewSoftErrorf("omada: %q has both IP addresses and CNAME targets", fqdn))
	}
	if len(d.ipv4s) > maxIPTargets {
		errs = append(errs, provider.NewSoftErrorf("omada: %q has %d A targets, the Omada limit is %d", fqdn, len(d.ipv4s), maxIPTargets))
	}
	if len(d.ipv6s) > maxIPTargets {
		errs = append(errs, provider.NewSoftErrorf("omada: %q has %d AAAA targets, the Omada limit is %d", fqdn, len(d.ipv6s), maxIPTargets))
	}
	if len(d.cnames) > 1 {
		errs = append(errs, provider.NewSoftErrorf("omada: %q has multiple CNAME targets %v", fqdn, sets.Sorted(d.cnames)))
	}
	return errs
}

// ApplyChanges applies the given changes to the Omada site.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	if changes == nil {
		return nil
	}
	ctx, span := p.tel.Tracer.Start(ctx, "omada.ApplyChanges")
	defer span.End()
	span.SetAttributes(
		attribute.Int("omada.dns.planned.create", len(changes.Create)),
		attribute.Int("omada.dns.planned.update", len(changes.UpdateNew)),
		attribute.Int("omada.dns.planned.delete", len(changes.Delete)),
		attribute.Bool("omada.dns.dry_run", p.dryRun),
	)
	current, err := p.currentState(ctx)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	var errs []error
	for fqdn, d := range desiredFromChanges(changes) {
		if vErrs := validateDesired(fqdn, d); len(vErrs) > 0 {
			p.errorsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(dnsErrorAttribute, "validate")))
			errs = append(errs, vErrs...)
			continue
		}
		if err := p.applyDesired(ctx, fqdn, d, current[fqdn]); err != nil {
			errs = append(errs, err)
		}
	}
	err = errors.Join(errs...)
	// The SDK leaves an unmarked span in the Unset state, so the outcome is
	// set explicitly in both directions.
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	return err
}

func (p *Provider) applyDesired(ctx context.Context, fqdn string, d *desiredState, cur *lanDNSState) error {
	empty := len(d.ipv4s) == 0 && len(d.ipv6s) == 0 && len(d.cnames) == 0
	recordChange := func(operation string) {
		p.changesCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(dnsOperationAttribute, operation)))
	}
	recordError := func(category string) {
		p.errorsCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(dnsErrorAttribute, category)))
	}

	if cur == nil {
		if empty {
			recordChange("skipped")
			return nil
		}
		entry := buildEntry(fqdn, d, true)
		if p.dryRun {
			log.Infof("DRY-RUN: would create Omada LAN DNS entry %q for %s", entry.Name, fqdn)
			recordChange("dry_run")
			return nil
		}
		id, err := p.api.createLanDNS(ctx, entry)
		if err != nil {
			recordError("create")
			return provider.NewSoftErrorf("omada: creating entry for %q: %v", fqdn, err)
		}
		if id == "" {
			log.Infof("Created Omada LAN DNS entry %q for %s", entry.Name, fqdn)
		} else {
			log.Infof("Created Omada LAN DNS entry %q for %s (id %s)", entry.Name, fqdn, id)
		}
		recordChange("created")
		p.spaceWrites()
		return nil
	}

	if empty {
		if cur.entryID == "" {
			log.Warnf("omada: cannot delete entry for %q: no entry id known", fqdn)
			recordChange("skipped")
			return nil
		}
		if p.dryRun {
			log.Infof("DRY-RUN: would delete Omada LAN DNS entry %q for %s", cur.entryID, fqdn)
			recordChange("dry_run")
			return nil
		}
		if err := p.api.deleteLanDNS(ctx, cur.entryID); err != nil {
			recordError("delete")
			return provider.NewSoftErrorf("omada: deleting entry for %q: %v", fqdn, err)
		}
		log.Infof("Deleted Omada LAN DNS entry %q for %s", cur.entryID, fqdn)
		recordChange("deleted")
		p.spaceWrites()
		return nil
	}

	if p.sameState(cur, d) {
		recordChange("skipped")
		return nil
	}
	if cur.entryID == "" {
		recordError("update")
		return provider.NewSoftErrorf("omada: cannot update entry for %q: no entry id known", fqdn)
	}
	entry := buildEntry(fqdn, d, cur.enable)
	entry.ID = cur.entryID
	if p.dryRun {
		log.Infof("DRY-RUN: would update Omada LAN DNS entry %q for %s", cur.entryID, fqdn)
		recordChange("dry_run")
		return nil
	}
	if err := p.api.updateLanDNS(ctx, cur.entryID, entry); err != nil {
		recordError("update")
		return provider.NewSoftErrorf("omada: updating entry for %q: %v", fqdn, err)
	}
	log.Infof("Updated Omada LAN DNS entry %q for %s", cur.entryID, fqdn)
	recordChange("updated")
	p.spaceWrites()
	return nil
}

// spaceWrites spaces out consecutive write requests so a batch of changes
// does not trip the controller's write rate limit.
func (p *Provider) spaceWrites() {
	if p.writeInterval > 0 {
		time.Sleep(p.writeInterval)
	}
}

// buildEntry renders the desired state as an Omada LAN DNS entry. Domain
// and Cname must not carry a trailing dot; the controller rejects dotted
// values with -1001. Endpoint names and targets already arrive without one.
func buildEntry(fqdn string, d *desiredState, enable bool) lanDNS {
	if len(d.cnames) > 0 {
		return lanDNS{
			Name:   fqdn,
			Domain: fqdn,
			Type:   1,
			Enable: enable,
			Cname:  sets.Sorted(d.cnames)[0],
		}
	}
	return lanDNS{
		Name:          fqdn,
		Domain:        fqdn,
		Type:          0,
		Enable:        enable,
		IPAddresses:   sets.Sorted(d.ipv4s),
		IPv6Addresses: sets.Sorted(d.ipv6s),
	}
}

// sameState reports whether the entry in the controller already matches the
// desired state, including its LAN network bindings.
func (p *Provider) sameState(cur *lanDNSState, d *desiredState) bool {
	if !p.networksMatch(cur.networkIDs) {
		return false
	}
	if cur.cname != "" {
		return len(d.cnames) == 1 && len(d.ipv4s) == 0 && len(d.ipv6s) == 0 && cur.cname == sets.Sorted(d.cnames)[0]
	}
	return len(d.cnames) == 0 && equalSets(cur.ipv4s, d.ipv4s) && equalSets(cur.ipv6s, d.ipv6s)
}

// networksMatch reports whether the entry is attached to the networks this
// instance manages. An unconfigured selection matches any binding.
func (p *Provider) networksMatch(cur sets.Set[string]) bool {
	if len(p.lanNetworkIDs) == 0 {
		return true
	}
	return equalSets(cur, p.lanNetworkIDs)
}

func equalSets(a, b sets.Set[string]) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b.Has(k) {
			return false
		}
	}
	return true
}
