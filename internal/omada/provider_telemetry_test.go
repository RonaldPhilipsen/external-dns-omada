package omada

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracetest "go.opentelemetry.io/otel/sdk/trace/tracetest"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

func newInstrumentedProvider(t *testing.T, api omadaAPI) (*Provider, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	p := newTestProviderWithTelemetry(t, api, Telemetry{Tracer: tp.Tracer("omada"), Meter: mp.Meter("omada")})
	return p, sr, reader
}

// metricValue collects the manual reader and returns the data points of the
// instrument named `name`, keyed by their attribute set
// ("omada.dns.operation=created") for counters or "" for an attributeless
// gauge. Walk reader.Collect -> rm.ScopeMetrics -> sm.Metrics, match on
// m.Name, and switch on m.Data (metricdata.Sum[int64] / Gauge[int64]),
// summing each DataPoints[i].Value and building the key by visiting
// DataPoints[i].Attributes.
func metricValue(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	out := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					out[metricKey(dp.Attributes)] += dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range data.DataPoints {
					out[metricKey(dp.Attributes)] += dp.Value
				}
			}
		}
	}
	return out
}

// metricKey renders an attribute set as "key=value" pairs in sorted order,
// or "" for an attributeless data point.
func metricKey(attrs attribute.Set) string {
	kvs := attrs.ToSlice()
	parts := make([]string, len(kvs))
	for i, kv := range kvs {
		parts[i] = string(kv.Key) + "=" + kv.Value.AsString()
	}
	return strings.Join(parts, ",")
}

// spanAttributes returns a span's attributes as a map for assertions.
func spanAttributes(t *testing.T, s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	t.Helper()
	attrs := make(map[string]attribute.Value, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	return attrs
}

func TestApplyChangesSpanAndChangeCounters(t *testing.T) {
	p, sr, reader := newInstrumentedProvider(t, &fakeAPI{})
	require.NoError(t, p.ApplyChanges(t.Context(), &plan.Changes{
		Create: []*endpoint.Endpoint{epA("new.example.com", "10.0.0.5")},
	}))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "omada.ApplyChanges", span.Name())
	attrs := spanAttributes(t, span)
	assert.Equal(t, int64(1), attrs["omada.dns.planned.create"].AsInt64())
	assert.Equal(t, int64(0), attrs["omada.dns.planned.update"].AsInt64())
	assert.Equal(t, int64(0), attrs["omada.dns.planned.delete"].AsInt64())
	assert.False(t, attrs["omada.dns.dry_run"].AsBool())
	assert.Equal(t, codes.Ok, span.Status().Code)

	assert.Equal(t, map[string]int64{"omada.dns.operation=created": 1},
		metricValue(t, reader, "omada.dns.changes"))
}

func TestRecordsRecordsEntriesGauge(t *testing.T) {
	fake := &fakeAPI{entries: []lanDNS{
		{ID: "e1", Name: "a", Domain: "a.example.com", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}},
		{ID: "e2", Name: "c", Domain: "c.example.com", Type: 1, Enable: true, Cname: "t.example.com"},
	}}
	p, _, reader := newInstrumentedProvider(t, fake)
	_, err := p.Records(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"": 2}, metricValue(t, reader, "omada.dns.entries"))
}

func TestApplyChangesDryRunCounters(t *testing.T) {
	fake := &fakeAPI{}
	p, _, reader := newInstrumentedProvider(t, fake)
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
	require.NoError(t, p.ApplyChanges(ctx, changes))
	assert.Empty(t, fake.created)
	assert.Equal(t, map[string]int64{"omada.dns.operation=dry_run": 3},
		metricValue(t, reader, "omada.dns.changes"))
}

func TestApplyChangesSkipCounter(t *testing.T) {
	fake := &fakeAPI{entries: []lanDNS{
		{ID: "e1", Name: "a", Domain: "a.example.com.", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}},
	}}
	p, _, reader := newInstrumentedProvider(t, fake)
	changes := &plan.Changes{UpdateNew: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")}}
	require.NoError(t, p.ApplyChanges(t.Context(), changes))
	assert.Equal(t, map[string]int64{"omada.dns.operation=skipped": 1},
		metricValue(t, reader, "omada.dns.changes"))
}

func TestApplyChangesMixedSuccessAndFailure(t *testing.T) {
	fake := &fakeAPI{
		entries:   []lanDNS{{ID: "e1", Name: "a", Domain: "a.example.com.", Type: 0, Enable: true, IPAddresses: []string{"10.0.0.1"}}},
		deleteErr: errors.New("omada error -35311: domain already exists"),
	}
	p, sr, reader := newInstrumentedProvider(t, fake)
	changes := &plan.Changes{
		Create: []*endpoint.Endpoint{epA("b.example.com.", "10.0.0.2")},
		Delete: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")},
	}
	err := p.ApplyChanges(t.Context(), changes)
	require.Error(t, err)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.Equal(t, map[string]int64{"omada.dns.operation=created": 1},
		metricValue(t, reader, "omada.dns.changes"))
	assert.Equal(t, map[string]int64{"omada.dns.error=delete": 1},
		metricValue(t, reader, "omada.dns.errors"))
}

func TestApplyChangesListFailureMarksSpanError(t *testing.T) {
	fake := &fakeAPI{listErr: errors.New("boom")}
	p, sr, _ := newInstrumentedProvider(t, fake)
	changes := &plan.Changes{Create: []*endpoint.Endpoint{epA("a.example.com.", "10.0.0.1")}}
	require.Error(t, p.ApplyChanges(t.Context(), changes))
	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestApplyChangesValidateErrorCounter(t *testing.T) {
	fake := &fakeAPI{}
	p, _, reader := newInstrumentedProvider(t, fake)
	changes := &plan.Changes{Create: []*endpoint.Endpoint{epA("*.example.com.", "10.0.0.1")}}
	require.Error(t, p.ApplyChanges(t.Context(), changes))
	assert.Equal(t, map[string]int64{"omada.dns.error=validate": 1},
		metricValue(t, reader, "omada.dns.errors"))
}

func TestRecordsListErrorCounter(t *testing.T) {
	fake := &fakeAPI{listErr: errors.New("boom")}
	p, _, reader := newInstrumentedProvider(t, fake)
	_, err := p.Records(t.Context())
	require.Error(t, err)
	assert.Equal(t, map[string]int64{"omada.dns.error=list": 1},
		metricValue(t, reader, "omada.dns.errors"))
}
