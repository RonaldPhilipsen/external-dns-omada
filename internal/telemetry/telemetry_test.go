package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shutdown tolerates flush failures against an unreachable collector; the
// assertion that matters for these tests is that Init succeeded.
func shutdown(t *testing.T, tel *Telemetry) {
	t.Helper()
	assert.NotPanics(t, func() {
		_ = tel.Shutdown(context.Background())
	})
}

func TestInitDisabledSignals(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")

	tel, err := Init(context.Background(), "test-version")
	require.NoError(t, err)

	assert.NotNil(t, tel.Tracer("x"))
	assert.NotNil(t, tel.Meter("x"))

	families, err := tel.Registry().Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range families {
		names[mf.GetName()] = true
	}
	assert.True(t, names["go_goroutines"], "Go collector must serve runtime metrics at /metrics")

	require.NoError(t, tel.Shutdown(context.Background()))
	assert.NoError(t, tel.Shutdown(context.Background()), "Shutdown must be idempotent")
}

func TestInitTracesDisabledMetricsOn(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	tel, err := Init(context.Background(), "test-version")
	require.NoError(t, err)
	assert.NotNil(t, tel.Meter("x"))
	families, gerr := tel.Registry().Gather()
	require.NoError(t, gerr)
	names := map[string]bool{}
	for _, mf := range families {
		names[mf.GetName()] = true
	}
	assert.True(t, names["go_goroutines"], "the /metrics registry must be populated even with traces disabled")
	shutdown(t, tel)
}

func TestInitDefaultsUnreachableCollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	tel, err := Init(context.Background(), "test-version")
	require.NoError(t, err, "an unreachable collector must not prevent startup")
	shutdown(t, tel)
}

func TestInitBadProtocolNamesTheSuppliedVariable(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "carrier-pigeon")

	_, err := Init(context.Background(), "test-version")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP_PROTOCOL")
	assert.NotContains(t, err.Error(), "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
}

func TestInitHTTPProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	tel, err := Init(context.Background(), "test-version")
	require.NoError(t, err)
	shutdown(t, tel)
}
