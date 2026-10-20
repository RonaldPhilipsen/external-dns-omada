package omada

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/RonaldPhilipsen/external-dns-omada/internal/sets"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

type fakeUpdate struct {
	id  string
	dns lanDNS
}

type fakeAPI struct {
	mu           sync.Mutex
	entries      []lanDNS
	listCalls    int
	created      []lanDNS
	updated      []fakeUpdate
	deleted      []string
	nextCreateID int
	listErr      error
	createErr    error
	updateErr    error
	deleteErr    error
}

func (f *fakeAPI) listLanDNS(_ context.Context) ([]lanDNS, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]lanDNS, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

func (f *fakeAPI) createLanDNS(_ context.Context, dns lanDNS) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextCreateID++
	id := fmt.Sprintf("created-%d", f.nextCreateID)
	dns.ID = id
	f.created = append(f.created, dns)
	return id, nil
}

func (f *fakeAPI) updateLanDNS(_ context.Context, id string, dns lanDNS) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, fakeUpdate{id: id, dns: dns})
	return nil
}

func (f *fakeAPI) deleteLanDNS(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func newTestProvider(t *testing.T, api omadaAPI) *Provider {
	t.Helper()
	return newTestProviderWithTelemetry(t, api, NoopTelemetry())
}

func newTestProviderWithTelemetry(t *testing.T, api omadaAPI, tel Telemetry) *Provider {
	t.Helper()
	p := &Provider{
		BaseProvider: provider.BaseProvider{},
		api:          api,
		domainFilter: endpoint.MatchAllDomainFilters{},
		tel:          tel,
	}
	require.NoError(t, p.initInstruments())
	return p
}

func epA(fqdn string, ips ...string) *endpoint.Endpoint {
	return endpoint.NewEndpointWithTTL(fqdn, endpoint.RecordTypeA, 0, ips...)
}

func epAAAA(fqdn string, ips ...string) *endpoint.Endpoint {
	return endpoint.NewEndpointWithTTL(fqdn, endpoint.RecordTypeAAAA, 0, ips...)
}

func epCNAME(fqdn, target string) *endpoint.Endpoint {
	return endpoint.NewEndpointWithTTL(fqdn, endpoint.RecordTypeCNAME, 0, target)
}

func withEntryID(ep *endpoint.Endpoint, id string) *endpoint.Endpoint {
	ep.SetProviderSpecificProperty("Omada/entry.id", id)
	ep.SetProviderSpecificProperty("Omada/entry.enable", "true")
	ep.SetProviderSpecificProperty("Omada/entry.network.ids", "")
	return ep
}

func sortEndpoints(eps []*endpoint.Endpoint) {
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].DNSName != eps[j].DNSName {
			return eps[i].DNSName < eps[j].DNSName
		}
		return eps[i].RecordType < eps[j].RecordType
	})
}

func TestRecordsMapping(t *testing.T) {
	fake := &fakeAPI{entries: []lanDNS{
		{ID: "e1", Name: "a", Domain: "a.example.com", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}},
		{ID: "e2", Name: "dual", Domain: "dual.example.com", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.2"}, IPv6Addresses: []string{"fd00::2"}},
		{ID: "e3", Name: "c", Domain: "c.example.com", Type: 1, Enable: true, Cname: "target.example.com"},
		{ID: "e4", Name: "fwd", Domain: "fwd.example.com", Type: 2, Enable: true},
		{ID: "e5", Name: "empty", Domain: "empty.example.com", Type: 0, Enable: true},
		{ID: "e6", Name: "out", Domain: "out.other.net", Type: 0, Enable: true, IPAddresses: []string{"10.9.9.9"}},
	}}
	p := newTestProvider(t, fake)
	p.domainFilter = endpoint.NewDomainFilter([]string{"example.com"})

	got, err := p.Records(t.Context())
	require.NoError(t, err)

	sortEndpoints(got)
	want := []*endpoint.Endpoint{
		withEntryID(epA("a.example.com", "10.0.0.1"), "e1"),
		withEntryID(epCNAME("c.example.com", "target.example.com"), "e3"),
		withEntryID(epA("dual.example.com", "10.0.0.2"), "e2"),
		withEntryID(epAAAA("dual.example.com", "fd00::2"), "e2"),
	}
	require.Equal(t, want, got)
}

func TestApplyChangesCreateA(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{Create: []*endpoint.Endpoint{epA("new.example.com", "10.0.0.5")}}

	err := p.ApplyChanges(t.Context(), changes)

	require.NoError(t, err)
	require.Len(t, fake.created, 1)
	require.Equal(t, 0, fake.created[0].Type)
	require.Equal(t, "new.example.com", fake.created[0].Domain)
	require.True(t, fake.created[0].Enable)
	assert.Equal(t, []string{"10.0.0.5"}, fake.created[0].IPAddresses)
	assert.Empty(t, fake.deleted)
	assert.Empty(t, fake.updated)
}

func TestApplyChangesCreateCNAME(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{Create: []*endpoint.Endpoint{epCNAME("cname.example.com", "target.example.com")}}

	err := p.ApplyChanges(t.Context(), changes)

	require.NoError(t, err)
	require.Len(t, fake.created, 1)
	require.Equal(t, 1, fake.created[0].Type)
	require.Equal(t, "cname.example.com", fake.created[0].Domain)
	assert.Equal(t, "target.example.com", fake.created[0].Cname)
}

func TestApplyChangesDelete(t *testing.T) {
	fake := &fakeAPI{entries: []lanDNS{
		{ID: "e1", Name: "a", Domain: "a.example.com.", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}},
	}}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{Delete: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")}}

	err := p.ApplyChanges(t.Context(), changes)

	require.NoError(t, err)
	assert.Equal(t, []string{"e1"}, fake.deleted)
	assert.Empty(t, fake.created)
	assert.Empty(t, fake.updated)
}

func TestApplyChangesUpdate(t *testing.T) {
	fake := &fakeAPI{entries: []lanDNS{
		{ID: "e1", Name: "a", Domain: "a.example.com.", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}},
	}}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.2")}}

	err := p.ApplyChanges(t.Context(), changes)

	require.NoError(t, err)
	require.Len(t, fake.updated, 1)
	assert.Equal(t, "e1", fake.updated[0].id)
	assert.Equal(t, []string{"10.0.0.2"}, fake.updated[0].dns.IPAddresses)
	assert.True(t, fake.updated[0].dns.Enable)
	assert.Empty(t, fake.created)
	assert.Empty(t, fake.deleted)
}

func TestApplyChangesNoOp(t *testing.T) {
	fake := &fakeAPI{entries: []lanDNS{
		{ID: "e1", Name: "a", Domain: "a.example.com.", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}},
	}}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")}}

	err := p.ApplyChanges(t.Context(), changes)

	require.NoError(t, err)
	assert.Empty(t, fake.created)
	assert.Empty(t, fake.updated)
	assert.Empty(t, fake.deleted)
}

func TestApplyChangesRebindsNetworks(t *testing.T) {
	t.Run("differing networks are rebound", func(t *testing.T) {
		fake := &fakeAPI{entries: []lanDNS{
			{ID: "e1", Name: "a", Domain: "a.example.com", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}, LanNetworkIDs: []string{"net-old"}},
		}}
		p := newTestProvider(t, fake)
		p.lanNetworkIDs = sets.New[string]("net-new")
		changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{epA("a.example.com", "10.0.0.1")}}

		err := p.ApplyChanges(t.Context(), changes)

		require.NoError(t, err)
		require.Len(t, fake.updated, 1)
		assert.Equal(t, "e1", fake.updated[0].id)
		assert.Empty(t, fake.created)
		assert.Empty(t, fake.deleted)
	})
	t.Run("matching networks are left alone", func(t *testing.T) {
		fake := &fakeAPI{entries: []lanDNS{
			{ID: "e1", Name: "a", Domain: "a.example.com", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}, LanNetworkIDs: []string{"net-a", "net-b"}},
		}}
		p := newTestProvider(t, fake)
		p.lanNetworkIDs = sets.New[string]("net-b", "net-a")
		changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{epA("a.example.com", "10.0.0.1")}}

		err := p.ApplyChanges(t.Context(), changes)

		require.NoError(t, err)
		assert.Empty(t, fake.created)
		assert.Empty(t, fake.updated)
		assert.Empty(t, fake.deleted)
	})
	t.Run("unconfigured selection matches any binding", func(t *testing.T) {
		fake := &fakeAPI{entries: []lanDNS{
			{ID: "e1", Name: "a", Domain: "a.example.com", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}, LanNetworkIDs: []string{"net-old"}},
		}}
		p := newTestProvider(t, fake)
		changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{epA("a.example.com", "10.0.0.1")}}

		err := p.ApplyChanges(t.Context(), changes)

		require.NoError(t, err)
		assert.Empty(t, fake.created)
		assert.Empty(t, fake.updated)
		assert.Empty(t, fake.deleted)
	})
}

func recordsCtx(eps ...*endpoint.Endpoint) context.Context {
	return context.WithValue(context.Background(), provider.RecordsContextKey, eps)
}

func TestApplyChangesUsesRecordsContext(t *testing.T) {
	t.Run("with context records no list call", func(t *testing.T) {
		fake := &fakeAPI{}
		p := newTestProvider(t, fake)
		changes := &plan.Changes{Delete: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")}}
		ctx := recordsCtx(withEntryID(epA("a.example.com.", "10.0.0.1"), "e1"))

		err := p.ApplyChanges(ctx, changes)

		require.NoError(t, err)
		assert.Equal(t, []string{"e1"}, fake.deleted)
		assert.Zero(t, fake.listCalls)
	})
	t.Run("without context falls back to list", func(t *testing.T) {
		fake := &fakeAPI{entries: []lanDNS{
			{ID: "e1", Name: "a", Domain: "a.example.com.", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}},
		}}
		p := newTestProvider(t, fake)
		changes := &plan.Changes{Delete: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")}}

		err := p.ApplyChanges(t.Context(), changes)

		require.NoError(t, err)
		assert.Equal(t, []string{"e1"}, fake.deleted)
		assert.Equal(t, 1, fake.listCalls)
	})
}

func TestApplyChangesDryRun(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	p.dryRun = true
	ctx := recordsCtx(
		withEntryID(epA("a.example.com.", "10.0.0.1"), "e1"),
		withEntryID(epA("b.example.com.", "10.0.0.2"), "e2"),
	)
	changes := &plan.Changes{
		Create:    []*endpoint.Endpoint{epA("c.example.com.", "10.0.0.3")},
		Delete:    []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")},
		UpdateNew: []*endpoint.Endpoint{epA("b.example.com.", "10.0.0.9")},
	}

	err := p.ApplyChanges(ctx, changes)

	require.NoError(t, err)
	assert.Zero(t, fake.listCalls)
	assert.Empty(t, fake.created)
	assert.Empty(t, fake.updated)
	assert.Empty(t, fake.deleted)
}

func TestApplyChangesRejectsWildcard(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{Create: []*endpoint.Endpoint{epA("*.example.com.", "10.0.0.1")}}

	err := p.ApplyChanges(t.Context(), changes)

	require.Error(t, err)
	require.ErrorIs(t, err, provider.SoftError)
	assert.Empty(t, fake.created)
}

func TestApplyChangesRejectsIPAndCNAMEConflict(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{Create: []*endpoint.Endpoint{
		epA("conflict.example.com.", "10.0.0.1"),
		epCNAME("conflict.example.com.", "target.example.com."),
	}}

	err := p.ApplyChanges(t.Context(), changes)

	require.Error(t, err)
	require.ErrorIs(t, err, provider.SoftError)
	assert.Empty(t, fake.created)
}

func TestApplyChangesRejectsTooManyTargets(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	ips := make([]string, 33)
	for i := range ips {
		ips[i] = fmt.Sprintf("10.1.0.%d", i)
	}
	changes := &plan.Changes{Create: []*endpoint.Endpoint{epA("many.example.com.", ips...)}}

	err := p.ApplyChanges(t.Context(), changes)

	require.Error(t, err)
	require.ErrorIs(t, err, provider.SoftError)
	assert.Empty(t, fake.created)
}

func TestApplyChangesRejectsMultipleCNAMETargets(t *testing.T) {
	fake := &fakeAPI{entries: []lanDNS{
		{ID: "e1", Name: "c", Domain: "c.example.com.", Type: 1, Enable: true, Cname: "one.example.com."},
	}}
	p := newTestProvider(t, fake)
	changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{
		epCNAME("c.example.com.", "one.example.com."),
		epCNAME("c.example.com.", "two.example.com."),
	}}

	err := p.ApplyChanges(t.Context(), changes)

	require.Error(t, err)
	require.ErrorIs(t, err, provider.SoftError)
	assert.Empty(t, fake.updated)
}

func TestApplyChangesDeleteUnknownEntrySkipped(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	ctx := recordsCtx(epA("a.example.com.", "10.0.0.1"))
	changes := &plan.Changes{Delete: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")}}

	err := p.ApplyChanges(ctx, changes)

	require.NoError(t, err)
	assert.Empty(t, fake.deleted)
}

func TestApplyChangesUpdateUnknownEntryFails(t *testing.T) {
	fake := &fakeAPI{}
	p := newTestProvider(t, fake)
	ctx := recordsCtx(epA("a.example.com.", "10.0.0.1"))
	changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.2")}}

	err := p.ApplyChanges(ctx, changes)

	require.Error(t, err)
	require.ErrorIs(t, err, provider.SoftError)
	assert.Empty(t, fake.updated)
}

func TestRecordsListErrorIsSoft(t *testing.T) {
	fake := &fakeAPI{listErr: errors.New("boom")}
	p := newTestProvider(t, fake)

	_, err := p.Records(t.Context())

	require.Error(t, err)
	require.ErrorIs(t, err, provider.SoftError)
}

func TestNewValidation(t *testing.T) {
	base := Config{
		Server:       "https://omada.example.com:8043",
		ID:           "omada-id",
		ClientID:     "client-id",
		ClientSecret: "secret",
		SiteName:     "site",
	}
	tests := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"missing server", func(c *Config) { c.Server = "" }},
		{"unparseable server", func(c *Config) { c.Server = "://bad" }},
		{"non http server", func(c *Config) { c.Server = "ftp://omada.example.com" }},
		{"missing omada id", func(c *Config) { c.ID = "" }},
		{"missing client id", func(c *Config) { c.ClientID = "" }},
		{"missing client secret", func(c *Config) { c.ClientSecret = "" }},
		{"missing site name", func(c *Config) { c.SiteName = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			_, err := New(t.Context(), cfg, NoopTelemetry())
			require.Error(t, err)
		})
	}
}
