package webhook

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	webhookclient "sigs.k8s.io/external-dns/provider/webhook"
)

// TestUpstreamClientContract drives this server with the real external-dns
// webhook provider client (provider/webhook), the same code path a
// --provider=webhook external-dns instance runs in production. It pins the
// wire contract: negotiation media type and DomainFilter payload, the
// GET /records response, and the 204 reply expected from POST /records.
func TestUpstreamClientContract(t *testing.T) {
	rec := endpoint.NewEndpointWithTTL("a.example.com", endpoint.RecordTypeA, 0, "10.0.0.1")
	rec.SetProviderSpecificProperty("Omada/entry.id", "e1")
	fake := &fakeProvider{
		records: []*endpoint.Endpoint{rec},
		filter:  endpoint.NewDomainFilter([]string{"example.com"}),
	}
	srv := httptest.NewServer(NewServer(fake, DefaultMaxBodySize).ProviderHandler())
	t.Cleanup(srv.Close)

	cfg := &externaldns.Config{
		WebhookProviderURL:          srv.URL,
		WebhookProviderReadTimeout:  5 * time.Second,
		WebhookProviderWriteTimeout: 10 * time.Second,
		WebhookProviderMaxBodySize:  DefaultMaxBodySize,
	}
	// New performs the negotiation request; it fails on a content type or
	// DomainFilter mismatch, so success already proves that part of the
	// contract.
	p, err := webhookclient.New(context.Background(), cfg, nil)
	require.NoError(t, err)

	got, err := p.Records(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "a.example.com", got[0].DNSName)
	assert.Equal(t, endpoint.RecordTypeA, got[0].RecordType)
	assert.Equal(t, endpoint.Targets{"10.0.0.1"}, got[0].Targets)
	prop, ok := got[0].GetProviderSpecificProperty("Omada/entry.id")
	require.True(t, ok)
	assert.Equal(t, "e1", prop)

	changes := &plan.Changes{
		Create: []*endpoint.Endpoint{
			endpoint.NewEndpointWithTTL("b.example.com", endpoint.RecordTypeA, 0, "10.0.0.2"),
		},
		UpdateNew: []*endpoint.Endpoint{
			endpoint.NewEndpointWithTTL("a.example.com", endpoint.RecordTypeAAAA, 0, "2001:db8::1"),
		},
		Delete: []*endpoint.Endpoint{
			endpoint.NewEndpointWithTTL("c.example.com", endpoint.RecordTypeCNAME, 0, "target.example.com"),
		},
	}
	require.NoError(t, p.ApplyChanges(context.Background(), changes))
	require.Equal(t, 1, fake.applyCalls)
	require.NotNil(t, fake.applied)
	require.Len(t, fake.applied.Create, 1)
	require.Len(t, fake.applied.UpdateNew, 1)
	require.Len(t, fake.applied.Delete, 1)
	assert.Equal(t, "b.example.com", fake.applied.Create[0].DNSName)
	assert.Equal(t, "a.example.com", fake.applied.UpdateNew[0].DNSName)
	assert.Equal(t, "c.example.com", fake.applied.Delete[0].DNSName)

	// The BaseProvider default is a pass-through; the client must get the
	// same endpoints back through POST /adjustendpoints.
	adjusted, err := p.AdjustEndpoints(got)
	require.NoError(t, err)
	require.Len(t, adjusted, 1)
	assert.Equal(t, "a.example.com", adjusted[0].DNSName)
}
