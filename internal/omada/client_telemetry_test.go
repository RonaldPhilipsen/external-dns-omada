package omada

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestClientHTTPIsInstrumented(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var mu sync.Mutex
	st := &authState{}
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == authPath {
			handleAuthorize(w, r, &mu, st)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sites") {
			handleSites(w, r, &mu, st)
			return
		}
		handleLanNetworks(w, r, &mu, st)
	})

	cl, err := newClient(t.Context(), ts.URL, "test-omada-id", "test-client", "test-secret", testSite.name, "", false)
	require.NoError(t, err)
	require.NotEmpty(t, cl.lanNetworkIDs)

	var sawAuth bool
	for _, s := range sr.Ended() {
		attrs := map[string]string{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		if s.SpanKind() == trace.SpanKindClient && strings.Contains(attrs["url.full"], "/openapi/authorize/token") {
			sawAuth = true
		}
	}
	assert.True(t, sawAuth, "the authorize call must produce a client span")
}
