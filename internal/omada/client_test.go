package omada

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	extdnshttp "sigs.k8s.io/external-dns/pkg/http"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

type errorTransport struct{}

func (errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// authState holds the test server's auth bookkeeping; all access is guarded
// by mu so handler goroutines never race.
type authState struct {
	token          string
	refreshToken   string
	authorizeCalls int
	refreshCalls   int
	// lastAuth records the Authorization header of the last non-auth request.
	lastAuth string
	// lastRefreshQuery records the query of the last refresh_token request.
	lastRefreshQuery url.Values
	// networksEmpty makes handleLanNetworks return an empty list.
	networksEmpty bool
}

// handleAuthorize implements POST /openapi/authorize/token for the test
// server. A client secret of "bad-secret" is rejected with HTTP 401.
func handleAuthorize(w http.ResponseWriter, r *http.Request, mu *sync.Mutex, st *authState) {
	w.Header().Set("Content-Type", contentTypeJSON)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	grantType := r.URL.Query().Get("grant_type")

	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid client credentials", http.StatusBadRequest)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	if grantType == "client_credentials" {
		st.authorizeCalls++
		if req.ClientSecret == "bad-secret" {
			http.Error(w, "invalid client credentials", http.StatusUnauthorized)
			return
		}
		st.token = "AT-test"
		st.refreshToken = "RT-test"
		if err := json.NewEncoder(w).Encode(map[string]any{
			"errorCode": 0,
			"msg":       "success",
			"result": map[string]any{
				"accessToken":  "AT-test",
				"tokenType":    "bearer",
				"expiresIn":    7200,
				"refreshToken": "RT-test",
			},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if grantType == "refresh_token" {
		st.refreshCalls++
		st.lastRefreshQuery = r.URL.Query()
		if r.URL.Query().Get("refresh_token") != st.refreshToken {
			http.Error(w, "invalid refresh token", http.StatusUnauthorized)
			return
		}
		st.token = "AT-2"
		st.refreshToken = "RT-2"
		if err := json.NewEncoder(w).Encode(map[string]any{
			"errorCode": 0,
			"msg":       "success",
			"result": map[string]any{
				"accessToken":  "AT-2",
				"tokenType":    "bearer",
				"expiresIn":    7200,
				"refreshToken": "RT-2",
			},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	http.Error(w, "unsupported grant type", http.StatusBadRequest)
}

// handleSites implements GET /openapi/v1/{omadaID}/sites and returns the
// single testSite.
func handleSites(w http.ResponseWriter, r *http.Request, mu *sync.Mutex, st *authState) {
	mu.Lock()
	authorized := r.Header.Get("Authorization") == "AccessToken="+st.token
	mu.Unlock()
	w.Header().Set("Content-Type", contentTypeJSON)
	if !authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{
		"errorCode": 0,
		"msg":       "success",
		"result": map[string]any{
			"totalRows":   1,
			"currentPage": 1,
			"currentSize": 1,
			"data": []map[string]any{
				{"siteId": testSite.id, "name": testSite.name},
			},
		},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var testSite = struct {
	name string
	id   string
}{name: "Home", id: "site-abc"}

// testNetworks are the site's LAN networks served by handleLanNetworks.
var testNetworks = []lanNetwork{
	{ID: "net-1", Name: "Default"},
	{ID: "net-2", Name: "Guest"},
}

// handleLanNetworks implements GET .../lan-networks and returns
// testNetworks, or an empty list when st.networksEmpty is set.
func handleLanNetworks(w http.ResponseWriter, r *http.Request, mu *sync.Mutex, st *authState) {
	mu.Lock()
	authorized := r.Header.Get("Authorization") == "AccessToken="+st.token
	empty := st.networksEmpty
	mu.Unlock()
	w.Header().Set("Content-Type", contentTypeJSON)
	if !authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Query().Get("page") == "" || r.URL.Query().Get("pageSize") == "" {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"errorCode": -1001,
			"msg":       "page should not be null.\npageSize should not be null.\n",
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	data := []map[string]any{}
	if !empty {
		for _, n := range testNetworks {
			data = append(data, map[string]any{"id": n.ID, "name": n.Name})
		}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{
		"errorCode": 0,
		"msg":       "success",
		"result":    map[string]any{"data": data},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleLanDNSList implements GET .../setting/lan/dns and returns one
// static entry.
func handleLanDNSList(w http.ResponseWriter, r *http.Request, mu *sync.Mutex, st *authState) {
	mu.Lock()
	authorized := r.Header.Get("Authorization") == "AccessToken="+st.token
	mu.Unlock()
	w.Header().Set("Content-Type", contentTypeJSON)
	if !authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{
		"errorCode": 0,
		"msg":       "success",
		"result": map[string]any{
			"totalRows":   1,
			"currentPage": 1,
			"currentSize": 1,
			"data": []map[string]any{
				{
					"id":          "dns-1",
					"domain":      "a.lan",
					"type":        0,
					"enable":      true,
					"ipAddresses": []string{"192.168.1.10"},
				},
			},
		},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func TestNewClient(t *testing.T) {
	tests := []struct {
		name          string
		clientSecret  string
		siteName      string
		networkNames  string
		networksEmpty bool
		wantErr       string
		wantToken     string
		wantSiteID    string
		wantNetworks  []string
	}{
		{
			name:         "valid credentials and site",
			clientSecret: "test-secret",
			siteName:     testSite.name,
			wantToken:    "AT-test",
			wantSiteID:   testSite.id,
			wantNetworks: []string{"net-1", "net-2"},
		},
		{
			name:         "invalid client secret",
			clientSecret: "bad-secret",
			siteName:     testSite.name,
			wantErr:      "omada",
		},
		{
			name:         "unknown site name",
			clientSecret: "test-secret",
			siteName:     "Missing",
			wantErr:      `site "Missing" not found`,
		},
		{
			name:         "unknown network name fails fast",
			clientSecret: "test-secret",
			siteName:     testSite.name,
			networkNames: "Missing",
			wantErr:      `LAN network "Missing" not found in site; available networks: Default, Guest`,
		},
		{
			name:         "network filter picks a subset case-insensitively",
			clientSecret: "test-secret",
			siteName:     testSite.name,
			networkNames: "guest, default",
			wantToken:    "AT-test",
			wantSiteID:   testSite.id,
			wantNetworks: []string{"net-1", "net-2"},
		},
		{
			name:          "site without LAN networks",
			clientSecret:  "test-secret",
			siteName:      testSite.name,
			networksEmpty: true,
			wantErr:       "has no LAN networks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			st := &authState{networksEmpty: tt.networksEmpty}

			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == authPath {
					handleAuthorize(w, r, &mu, st)
					return
				}
				mu.Lock()
				st.lastAuth = r.Header.Get("Authorization")
				mu.Unlock()
				if strings.HasSuffix(r.URL.Path, "/sites") {
					handleSites(w, r, &mu, st)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/lan-networks") {
					handleLanNetworks(w, r, &mu, st)
					return
				}
				handleLanDNSList(w, r, &mu, st)
			})
			base, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}

			cl, err := newClient(
				t.Context(),
				base.String(),
				"test-omada-id",
				"test-client",
				tt.clientSecret,
				tt.siteName,
				tt.networkNames,
				false,
			)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("newClient() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("newClient() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newClient() error = %v", err)
			}
			if cl.accessToken != tt.wantToken {
				t.Fatalf("accessToken = %q, want %q", cl.accessToken, tt.wantToken)
			}
			if cl.siteID != tt.wantSiteID {
				t.Fatalf("siteID = %q, want %q", cl.siteID, tt.wantSiteID)
			}
			if !reflect.DeepEqual(cl.lanNetworkIDs, tt.wantNetworks) {
				t.Fatalf("lanNetworkIDs = %v, want %v", cl.lanNetworkIDs, tt.wantNetworks)
			}

			_, err = cl.listLanDNS(t.Context())
			if err != nil {
				t.Fatalf("listLanDNS() error = %v", err)
			}
			mu.Lock()
			gotAuth := st.lastAuth
			mu.Unlock()
			if gotAuth != "AccessToken=AT-test" {
				t.Fatalf("request Authorization = %q, want AccessToken=AT-test", gotAuth)
			}
		})
	}
}

func TestErrorCodeMapping(t *testing.T) {
	t.Run("envelope error carries code and message", func(t *testing.T) {
		var mu sync.Mutex
		st := &authState{}

		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == authPath {
				handleAuthorize(w, r, &mu, st)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/sites") {
				handleSites(w, r, &mu, st)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/lan-networks") {
				handleLanNetworks(w, r, &mu, st)
				return
			}
			w.Header().Set("Content-Type", contentTypeJSON)
			if r.Method == http.MethodPost {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errorCode": -35311,
					"msg":       "domain already exists",
					"result":    nil,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errorCode": 0,
				"msg":       "success",
				"result":    map[string]any{"id": "dns-1"},
			})
		})
		base, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := newClient(
			t.Context(),
			base.String(),
			"test-omada-id",
			"test-client",
			"test-secret",
			testSite.name,
			"",
			false,
		)
		if err != nil {
			t.Fatalf("newClient() error = %v", err)
		}
		_, err = cl.createLanDNS(t.Context(), lanDNS{
			Domain:        "test.lan",
			Type:          0,
			Enable:        true,
			IPAddresses:   []string{"192.168.1.10"},
			IPv6Addresses: []string{"fd00::10"},
		})
		if err == nil {
			t.Fatal("createLanDNS() error = nil, want envelope error")
		}
		if !strings.Contains(err.Error(), "domain already exists") {
			t.Fatalf("error = %q, want it to contain the envelope msg", err)
		}
		if !strings.Contains(err.Error(), "-35311") {
			t.Fatalf("error = %q, want it to contain the error code", err.Error())
		}
		if errors.Is(err, errNotAuthorized) {
			t.Fatal("envelope error must not be classified as not authorized")
		}
	})

	t.Run("auth failure surfaces errNotAuthorized", func(t *testing.T) {
		var mu sync.Mutex
		st := &authState{}
		// After startup, the token is invalidated and both refresh and
		// re-authorize fail, so the retry loop must give up with the
		// original 401.
		var authCalls int

		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == authPath {
				grantType := r.URL.Query().Get("grant_type")
				if grantType == "client_credentials" {
					mu.Lock()
					authCalls++
					first := authCalls == 1
					if first {
						st.token = "AT-first"
						st.refreshToken = "RT-first"
					}
					mu.Unlock()
					if !first {
						http.Error(w, "invalid client credentials", http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", contentTypeJSON)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"errorCode": 0,
						"msg":       "success",
						"result": map[string]any{
							"accessToken":  "AT-first",
							"tokenType":    "bearer",
							"expiresIn":    7200,
							"refreshToken": "RT-first",
						},
					})
					return
				}
				http.Error(w, "invalid refresh token", http.StatusUnauthorized)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/sites") {
				handleSites(w, r, &mu, st)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/lan-networks") {
				handleLanNetworks(w, r, &mu, st)
				return
			}
			mu.Lock()
			authorized := r.Header.Get("Authorization") == "AccessToken="+st.token
			mu.Unlock()
			w.Header().Set("Content-Type", contentTypeJSON)
			if !authorized {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errorCode": 0,
				"msg":       "success",
				"result": map[string]any{
					"totalRows":   0,
					"currentPage": 1,
					"currentSize": 0,
					"data":        []any{},
				},
			})
		})
		base, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := newClient(
			t.Context(),
			base.String(),
			"test-omada-id",
			"test-client",
			"test-secret",
			testSite.name,
			"",
			false,
		)
		if err != nil {
			t.Fatalf("newClient() error = %v", err)
		}
		// Invalidate the token: subsequent requests are 401, refresh and
		// re-authorize both fail.
		mu.Lock()
		st.token = "AT-stale"
		mu.Unlock()

		_, err = cl.listLanDNS(t.Context())
		if err == nil {
			t.Fatal("listLanDNS() error = nil, want not authorized")
		}
		if !errors.Is(err, errNotAuthorized) {
			t.Fatalf("error = %q, want it to wrap errNotAuthorized", err)
		}
	})
}

func TestAuthRefresh(t *testing.T) {
	var mu sync.Mutex
	st := &authState{}

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == authPath:
			handleAuthorize(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/sites"):
			handleSites(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/lan-networks"):
			handleLanNetworks(w, r, &mu, st)
		default:
			handleLanDNSList(w, r, &mu, st)
		}
	})
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newClient(
		t.Context(),
		base.String(),
		"test-omada-id",
		"test-client",
		"test-secret",
		testSite.name,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	if _, err := cl.listLanDNS(t.Context()); err != nil {
		t.Fatalf("first listLanDNS() error = %v", err)
	}

	// Expire the token server-side: the next call must 401, refresh, and
	// retry transparently.
	mu.Lock()
	st.token = "AT-stale"
	mu.Unlock()

	entries, err := cl.listLanDNS(t.Context())
	if err != nil {
		t.Fatalf("second listLanDNS() error = %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "dns-1" {
		t.Fatalf("entries = %+v, want the single static entry", entries)
	}
	if cl.accessToken != "AT-2" {
		t.Fatalf("accessToken = %q, want AT-2", cl.accessToken)
	}
	mu.Lock()
	refreshCalls := st.refreshCalls
	refreshQuery := st.lastRefreshQuery
	mu.Unlock()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if refreshQuery.Get("grant_type") != "refresh_token" {
		t.Fatalf("refresh grant_type = %q, want refresh_token", refreshQuery.Get("grant_type"))
	}
	if refreshQuery.Get("refresh_token") != "RT-test" {
		t.Fatalf("refresh_token = %q, want RT-test", refreshQuery.Get("refresh_token"))
	}
}

func TestListLanDNSPagination(t *testing.T) {
	const total = 1500

	var mu sync.Mutex
	st := &authState{}
	var requestedPages []string

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == authPath:
			handleAuthorize(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/sites"):
			handleSites(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/lan-networks"):
			handleLanNetworks(w, r, &mu, st)
		default:
			mu.Lock()
			authorized := r.Header.Get("Authorization") == "AccessToken="+st.token
			requestedPages = append(requestedPages,
				r.URL.Query().Get("page")+"/"+r.URL.Query().Get("pageSize"))
			mu.Unlock()
			w.Header().Set("Content-Type", contentTypeJSON)
			if !authorized {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if err != nil || page < 1 {
				http.Error(w, "invalid page", http.StatusBadRequest)
				return
			}
			start := (page - 1) * 1000
			end := min(start+1000, total)
			data := make([]map[string]any, 0, end-start)
			for i := start; i < end; i++ {
				data = append(data, map[string]any{
					"id":     fmt.Sprintf("dns-%04d", i),
					"domain": fmt.Sprintf("e%04d.lan", i),
					"type":   0,
					"enable": true,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errorCode": 0,
				"msg":       "success",
				"result": map[string]any{
					"totalRows":   total,
					"currentPage": page,
					"currentSize": len(data),
					"data":        data,
				},
			})
		}
	})
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newClient(
		t.Context(),
		base.String(),
		"test-omada-id",
		"test-client",
		"test-secret",
		testSite.name,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	entries, err := cl.listLanDNS(t.Context())
	if err != nil {
		t.Fatalf("listLanDNS() error = %v", err)
	}
	if len(entries) != total {
		t.Fatalf("got %d entries, want %d", len(entries), total)
	}
	if entries[0].ID != "dns-0000" {
		t.Fatalf("first entry ID = %q, want dns-0000", entries[0].ID)
	}
	if entries[total-1].ID != fmt.Sprintf("dns-%04d", total-1) {
		t.Fatalf("last entry ID = %q, want dns-%04d", entries[total-1].ID, total-1)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requestedPages) != 2 {
		t.Fatalf("requested pages = %v, want exactly [1/1000 2/1000]", requestedPages)
	}
	if requestedPages[0] != "1/1000" || requestedPages[1] != "2/1000" {
		t.Fatalf("requested pages = %v, want [1/1000 2/1000]", requestedPages)
	}
}

func TestCreateLanDNS(t *testing.T) {
	var mu sync.Mutex
	st := &authState{}
	var captured struct {
		method string
		path   string
		body   []byte
	}

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == authPath:
			handleAuthorize(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/sites"):
			handleSites(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/lan-networks"):
			handleLanNetworks(w, r, &mu, st)
		default:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			captured.method = r.Method
			captured.path = r.URL.Path
			captured.body = body
			mu.Unlock()
			w.Header().Set("Content-Type", contentTypeJSON)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errorCode": 0,
				"msg":       "success",
				"result":    map[string]any{"id": "dns-new-1"},
			})
		}
	})
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newClient(
		t.Context(),
		base.String(),
		"test-omada-id",
		"test-client",
		"test-secret",
		testSite.name,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	id, err := cl.createLanDNS(t.Context(), lanDNS{
		Domain:        "test.lan",
		Type:          0,
		Enable:        true,
		IPAddresses:   []string{"192.168.1.10"},
		IPv6Addresses: []string{"fd00::10"},
	})
	if err != nil {
		t.Fatalf("createLanDNS() error = %v", err)
	}
	if id != "dns-new-1" {
		t.Fatalf("createLanDNS() ID = %q, want dns-new-1", id)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", captured.method)
	}
	if want := "/openapi/v1/test-omada-id/sites/site-abc/setting/lan/dns"; captured.path != want {
		t.Fatalf("path = %q, want %q", captured.path, want)
	}
	var payload map[string]any
	if err := json.Unmarshal(captured.body, &payload); err != nil {
		t.Fatalf("invalid JSON body %q: %v", captured.body, err)
	}
	if payload["domain"] != "test.lan" {
		t.Fatalf("body domain = %v, want test.lan", payload["domain"])
	}
	if payload["type"] != float64(0) {
		t.Fatalf("body type = %v, want 0", payload["type"])
	}
	if payload["enable"] != true {
		t.Fatalf("body enable = %v, want true", payload["enable"])
	}
	ips, _ := payload["ipAddresses"].([]any)
	if len(ips) != 1 || ips[0] != "192.168.1.10" {
		t.Fatalf("body ipAddresses = %v, want [192.168.1.10]", payload["ipAddresses"])
	}
	ips6, _ := payload["ipv6Addresses"].([]any)
	if len(ips6) != 1 || ips6[0] != "fd00::10" {
		t.Fatalf("body ipv6Addresses = %v, want [fd00::10]", payload["ipv6Addresses"])
	}
	nets, _ := payload["lanNetworkIds"].([]any)
	if len(nets) != 2 || nets[0] != "net-1" || nets[1] != "net-2" {
		t.Fatalf("body lanNetworkIds = %v, want [net-1 net-2]", payload["lanNetworkIds"])
	}
}

// TestCreateLanDNSWithoutResult covers controllers that answer successful
// creates with an envelope that has no result body, so createLanDNS must
// accept the missing entry ID instead of failing the create.
func TestCreateLanDNSWithoutResult(t *testing.T) {
	var mu sync.Mutex
	st := &authState{}

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == authPath:
			handleAuthorize(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/sites"):
			handleSites(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/lan-networks"):
			handleLanNetworks(w, r, &mu, st)
		default:
			w.Header().Set("Content-Type", contentTypeJSON)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errorCode": 0,
				"msg":       "Success.",
			})
		}
	})
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newClient(
		t.Context(),
		base.String(),
		"test-omada-id",
		"test-client",
		"test-secret",
		testSite.name,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	id, err := cl.createLanDNS(t.Context(), lanDNS{
		Domain:      "test.lan",
		Type:        0,
		Enable:      true,
		IPAddresses: []string{"192.168.1.10"},
	})
	if err != nil {
		t.Fatalf("createLanDNS() error = %v", err)
	}
	if id != "" {
		t.Fatalf("createLanDNS() ID = %q, want empty", id)
	}
}

func TestUpdateLanDNS(t *testing.T) {
	var mu sync.Mutex
	st := &authState{}
	var captured struct {
		method string
		path   string
		body   []byte
	}

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == authPath:
			handleAuthorize(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/sites"):
			handleSites(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/lan-networks"):
			handleLanNetworks(w, r, &mu, st)
		default:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			captured.method = r.Method
			captured.path = r.URL.Path
			captured.body = body
			mu.Unlock()
			w.Header().Set("Content-Type", contentTypeJSON)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errorCode": 0,
				"msg":       "success",
				"result":    map[string]any{"id": "dns-1"},
			})
		}
	})
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newClient(
		t.Context(),
		base.String(),
		"test-omada-id",
		"test-client",
		"test-secret",
		testSite.name,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	err = cl.updateLanDNS(t.Context(), "dns-1", lanDNS{
		Domain:      "test.lan",
		Type:        0,
		Enable:      false,
		IPAddresses: []string{"192.168.1.11"},
	})
	if err != nil {
		t.Fatalf("updateLanDNS() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if captured.method != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", captured.method)
	}
	if want := "/openapi/v1/test-omada-id/sites/site-abc/setting/lan/dns/dns-1"; captured.path != want {
		t.Fatalf("path = %q, want %q", captured.path, want)
	}
	var payload map[string]any
	if err := json.Unmarshal(captured.body, &payload); err != nil {
		t.Fatalf("invalid JSON body %q: %v", captured.body, err)
	}
	nets, _ := payload["lanNetworkIds"].([]any)
	if len(nets) != 2 || nets[0] != "net-1" || nets[1] != "net-2" {
		t.Fatalf("body lanNetworkIds = %v, want [net-1 net-2]", payload["lanNetworkIds"])
	}
}

func TestDeleteLanDNS(t *testing.T) {
	var mu sync.Mutex
	st := &authState{}
	var captured struct {
		method string
		path   string
	}

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == authPath:
			handleAuthorize(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/sites"):
			handleSites(w, r, &mu, st)
		case strings.HasSuffix(r.URL.Path, "/lan-networks"):
			handleLanNetworks(w, r, &mu, st)
		default:
			mu.Lock()
			captured.method = r.Method
			captured.path = r.URL.Path
			mu.Unlock()
			w.Header().Set("Content-Type", contentTypeJSON)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errorCode": 0,
				"msg":       "success",
				"result":    nil,
			})
		}
	})
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := newClient(
		t.Context(),
		base.String(),
		"test-omada-id",
		"test-client",
		"test-secret",
		testSite.name,
		"",
		false,
	)
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	if err := cl.deleteLanDNS(t.Context(), "dns-1"); err != nil {
		t.Fatalf("deleteLanDNS() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if captured.method != http.MethodDelete {
		t.Fatalf("method = %q, want DELETE", captured.method)
	}
	if want := "/openapi/v1/test-omada-id/sites/site-abc/setting/lan/dns/dns-1"; captured.path != want {
		t.Fatalf("path = %q, want %q", captured.path, want)
	}
}

func TestNetworkError(t *testing.T) {
	cl := &omadaClient{
		baseURL:    "https://controller.example.test",
		omadaID:    "test-omada-id",
		siteID:     "site-abc",
		httpClient: extdnshttp.NewInstrumentedClient(&http.Client{Transport: errorTransport{}}),
	}
	_, err := cl.listLanDNS(t.Context())
	if err == nil {
		t.Fatal("listLanDNS() error = nil, want transport error")
	}
	if errors.Is(err, errNotAuthorized) {
		t.Fatal("transport error must not be classified as not authorized")
	}
}

func TestSelectLanNetworks(t *testing.T) {
	networks := []lanNetwork{
		{ID: "net-1", Name: "Default"},
		{ID: "net-2", Name: "IOT"},
		{ID: "net-3", Name: "Guest"},
	}
	tests := []struct {
		name    string
		input   string
		wantIDs []string
		wantErr string
	}{
		{name: "empty selects all", input: "", wantIDs: []string{"net-1", "net-2", "net-3"}},
		{name: "only separators selects all", input: " , ", wantIDs: []string{"net-1", "net-2", "net-3"}},
		{name: "subset in site order", input: "Guest,Default", wantIDs: []string{"net-1", "net-3"}},
		{name: "case insensitive with spaces", input: " guest , iot ", wantIDs: []string{"net-2", "net-3"}},
		{name: "duplicate names collapse", input: "guest,guest,Guest", wantIDs: []string{"net-3"}},
		{name: "unknown name", input: "Missing", wantErr: `LAN network "Missing" not found in site; available networks: Default, IOT, Guest`},
		{name: "unknown name among known ones", input: "Default,Missing", wantErr: `LAN network "Missing" not found in site`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, err := selectLanNetworks(networks, tt.input)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("selectLanNetworks() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectLanNetworks() error = %v", err)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Fatalf("selectLanNetworks() = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}
