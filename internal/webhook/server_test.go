package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// fakeProvider is a provider.Provider test double that records the calls it
// receives.
type fakeProvider struct {
	records     []*endpoint.Endpoint
	recordsErr  error
	applyErr    error
	applied     *plan.Changes
	applyCalls  int
	adjustInput []*endpoint.Endpoint
	adjusted    []*endpoint.Endpoint
	adjustErr   error
	filter      endpoint.DomainFilterInterface
}

func (f *fakeProvider) Records(context.Context) ([]*endpoint.Endpoint, error) {
	return f.records, f.recordsErr
}

func (f *fakeProvider) ApplyChanges(_ context.Context, c *plan.Changes) error {
	f.applyCalls++
	f.applied = c
	return f.applyErr
}

func (f *fakeProvider) AdjustEndpoints(e []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	f.adjustInput = e
	if f.adjustErr != nil {
		return nil, f.adjustErr
	}
	if f.adjusted != nil {
		return f.adjusted, nil
	}
	return e, nil
}

func (f *fakeProvider) GetDomainFilter() endpoint.DomainFilterInterface {
	if f.filter == nil {
		return &endpoint.DomainFilter{}
	}
	return f.filter
}

const changesJSON = `{"create":[{"dnsName":"a.example.com","recordType":"A","targets":["10.0.0.1"]}]}`

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	resp, err := (&http.Client{}).Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestNegotiate(t *testing.T) {
	fake := &fakeProvider{filter: endpoint.NewDomainFilter([]string{"example.com"})}
	srv := httptest.NewServer(NewServer(fake, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, MediaType, resp.Header.Get("Content-Type"))

	var df endpoint.DomainFilter
	require.NoError(t, json.Unmarshal(body, &df))
	assert.Equal(t, []string{"example.com"}, df.Filters)
}

func TestNegotiateEmptyFilter(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeProvider{}, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	// An empty filter marshals to an empty object: every serde field is
	// omitempty.
	assert.JSONEq(t, `{}`, string(body))
}

func TestRecords(t *testing.T) {
	rec := endpoint.NewEndpointWithTTL("a.example.com", endpoint.RecordTypeA, 0, "10.0.0.1")
	rec.SetProviderSpecificProperty("Omada/entry.id", "e1")
	fake := &fakeProvider{records: []*endpoint.Endpoint{rec}}
	srv := httptest.NewServer(NewServer(fake, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/records", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, MediaType, resp.Header.Get("Content-Type"))

	var got []*endpoint.Endpoint
	require.NoError(t, json.Unmarshal(body, &got))
	require.Len(t, got, 1)
	require.Equal(t, "a.example.com", got[0].DNSName)
	require.Equal(t, endpoint.RecordTypeA, got[0].RecordType)
	assert.Equal(t, rec.Targets, got[0].Targets)
	prop, ok := got[0].GetProviderSpecificProperty("Omada/entry.id")
	require.True(t, ok)
	assert.Equal(t, "e1", prop)
}

func TestRecordsProviderError(t *testing.T) {
	fake := &fakeProvider{recordsErr: fmt.Errorf("controller unreachable")}
	srv := httptest.NewServer(NewServer(fake, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/records", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(body), "controller unreachable")
}

func TestApplyChanges(t *testing.T) {
	fake := &fakeProvider{}
	srv := httptest.NewServer(NewServer(fake, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodPost, srv.URL+"/records", changesJSON)
	defer resp.Body.Close()

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, 1, fake.applyCalls)
	require.NotNil(t, fake.applied)
	require.Len(t, fake.applied.Create, 1)
	require.Equal(t, "a.example.com", fake.applied.Create[0].DNSName)
	require.Equal(t, endpoint.Targets{"10.0.0.1"}, fake.applied.Create[0].Targets)
}

func TestApplyChangesProviderError(t *testing.T) {
	fake := &fakeProvider{applyErr: fmt.Errorf("omada error -35311: domain already exists")}
	srv := httptest.NewServer(NewServer(fake, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodPost, srv.URL+"/records", changesJSON)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(body), "domain already exists")
}

func TestApplyChangesBadJSON(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeProvider{}, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodPost, srv.URL+"/records", `{not json`)
	defer resp.Body.Close()

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestApplyChangesBodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeProvider{}, 64).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodPost, srv.URL+"/records", strings.Repeat("x", 256))
	defer resp.Body.Close()

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestAdjustEndpoints(t *testing.T) {
	ep := endpoint.NewEndpointWithTTL("a.example.com", endpoint.RecordTypeA, 0, "10.0.0.1")
	adjusted := endpoint.NewEndpointWithTTL("a.example.com", endpoint.RecordTypeA, 60, "10.0.0.1")
	fake := &fakeProvider{adjusted: []*endpoint.Endpoint{adjusted}}
	srv := httptest.NewServer(NewServer(fake, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	body, err := json.Marshal([]*endpoint.Endpoint{ep})
	require.NoError(t, err)
	resp := do(t, http.MethodPost, srv.URL+"/adjustendpoints", string(body))
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, MediaType, resp.Header.Get("Content-Type"))
	require.Len(t, fake.adjustInput, 1)
	require.Equal(t, "a.example.com", fake.adjustInput[0].DNSName)

	var got []*endpoint.Endpoint
	require.NoError(t, json.Unmarshal(respBody, &got))
	require.Len(t, got, 1)
	require.Equal(t, endpoint.TTL(60), got[0].RecordTTL)
}

func TestAdjustEndpointsProviderError(t *testing.T) {
	fake := &fakeProvider{adjustErr: fmt.Errorf("provider unavailable")}
	srv := httptest.NewServer(NewServer(fake, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodPost, srv.URL+"/adjustendpoints", `[]`)
	defer resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestUnsupportedMethods(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeProvider{}, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		method string
		url    string
	}{
		{http.MethodDelete, srv.URL + "/records"},
		{http.MethodGet, srv.URL + "/adjustendpoints"},
		{http.MethodPut, srv.URL + "/records"},
	} {
		resp := do(t, tc.method, tc.url, "")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s %s", tc.method, tc.url)
	}
}

func TestExposedEndpoints(t *testing.T) {
	srv := httptest.NewServer(NewServer(&fakeProvider{}, 0).ExposedHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/healthz", "")
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(respBody))

	resp = do(t, http.MethodGet, srv.URL+"/metrics", "")
	metricsBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(metricsBody), "go_goroutines")
	assert.NotContains(t, string(metricsBody), "external_dns_build_info")
}

func TestUnknownPathFallsBackToNegotiate(t *testing.T) {
	// The mux catch-all pattern is "/" (same as the in-tree reference
	// implementation), so an unknown path answers the negotiation request.
	srv := httptest.NewServer(NewServer(&fakeProvider{}, 0).ProviderHandler())
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodGet, srv.URL+"/anything/else", "")
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, MediaType, resp.Header.Get("Content-Type"))
}
