package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/RonaldPhilipsen/external-dns-omada/internal/telemetry"
)

func TestMetricsWithCustomRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	ctr := prometheus.NewCounter(prometheus.CounterOpts{Name: "omada_demo_total"})
	reg.MustRegister(ctr)
	ctr.Add(3)

	srv := httptest.NewServer(NewServer(&fakeProvider{}, 0, WithMetricsRegistry(reg)).ExposedHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/metrics", "")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "omada_demo_total 3")
}

func TestServerRequestSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	srv := httptest.NewServer(NewServer(&fakeProvider{}, DefaultMaxBodySize).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/records", "")
	resp.Body.Close()
	resp = do(t, http.MethodPost, srv.URL+"/records", changesJSON)
	resp.Body.Close()

	names := map[string]bool{}
	for _, s := range sr.Ended() {
		names[s.Name()] = true
	}
	assert.True(t, names["GET /records"], "GET %v", names)
	assert.True(t, names["POST /records"], "POST %v", names)
}

// TestMetricsBridgedToRegistry pins the spec behavior that otelhttp's HTTP
// server metrics reach the bridged registry: with the global providers set
// by telemetry.Init (OTLP disabled), a request through the otelhttp-wrapped
// handler must show up in tel.Registry().
func TestMetricsBridgedToRegistry(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	tel, err := telemetry.Init(context.Background(), "test-version")
	require.NoError(t, err)
	prevTracer := otel.GetTracerProvider()
	prevMeter := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTracer)
		otel.SetMeterProvider(prevMeter)
	})

	// Init set the global meter provider (whose reader is the Prometheus
	// bridge into tel.Registry()); NewServer must be built after Init so the
	// otelhttp instruments resolve to it.
	srv := httptest.NewServer(NewServer(&fakeProvider{}, DefaultMaxBodySize,
		WithMetricsRegistry(tel.Registry())).ExposedHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/healthz", "")
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	families, err := tel.Registry().Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range families {
		names[mf.GetName()] = true
	}
	assert.True(t, names["go_goroutines"], "runtime series must survive: %v", names)
	assert.True(t, names["http_server_request_duration_seconds"],
		"otelhttp's http.server.request.duration must be bridged: %v", names)
}
