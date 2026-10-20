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

	var sawSites bool
	for _, s := range sr.Ended() {
		attrs := map[string]string{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		if s.SpanKind() == trace.SpanKindClient && strings.Contains(attrs["url.full"], "/sites") {
			sawSites = true
		}
	}
	assert.True(t, sawSites, "the site resolution call must produce a client span")
}

// TestClientRefreshDoesNotLeakCredentials pins the spec non-goal that no
// span attribute carries credentials: the 401 -> refresh -> retry path must
// run, but the refresh request puts client_secret and refresh_token in the
// URL query, so no span of it may appear.
func TestClientRefreshDoesNotLeakCredentials(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	const clientSecret = "super-secret-client-credential"
	var mu sync.Mutex
	st := &authState{}
	var sitesCalls int
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == authPath {
			handleAuthorize(w, r, &mu, st)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sites") {
			mu.Lock()
			first := sitesCalls == 0
			sitesCalls++
			mu.Unlock()
			if first {
				// Expired token: force the 401 -> refresh -> retry path.
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			handleSites(w, r, &mu, st)
			return
		}
		handleLanNetworks(w, r, &mu, st)
	})

	cl, err := newClient(t.Context(), ts.URL, "test-omada-id", "test-client", clientSecret, testSite.name, "", false)
	require.NoError(t, err)
	require.NotEmpty(t, cl.lanNetworkIDs)

	mu.Lock()
	refreshCalls := st.refreshCalls
	mu.Unlock()
	require.Positive(t, refreshCalls, "the test must have exercised the refresh path")

	for _, s := range sr.Ended() {
		for _, kv := range s.Attributes() {
			v := kv.Value.AsString()
			assert.NotContains(t, v, clientSecret, "span %q must not carry the client secret", s.Name())
			assert.NotContains(t, v, "RT-test", "span %q must not carry the refresh token", s.Name())
			assert.NotContains(t, v, "AT-test", "span %q must not carry the access token", s.Name())
		}
		if s.SpanKind() == trace.SpanKindClient {
			attrs := map[string]string{}
			for _, kv := range s.Attributes() {
				attrs[string(kv.Key)] = kv.Value.AsString()
			}
			assert.NotContains(t, attrs["url.full"], authPath, "no span for the credential-carrying auth endpoint: %q", attrs["url.full"])
		}
	}
}
