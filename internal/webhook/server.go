// Package webhook implements the ExternalDNS webhook provider HTTP API
// documented in external-dns docs/tutorials/webhook-provider.md, so an
// in-process provider can be consumed by an external-dns instance running
// with --provider=webhook.
package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

const (
	// MediaType is the content type of the webhook provider API, version 1.
	// It must match the media type external-dns negotiates; a mismatch
	// makes external-dns refuse to connect.
	MediaType = "application/external.dns.webhook+json;version=1"

	contentTypeHeader = "Content-Type"

	// DefaultMaxBodySize matches external-dns'
	// --webhook-provider-max-body-size default (32 MiB, ~60k records).
	DefaultMaxBodySize = 32 << 20
)

// Server serves a provider over the ExternalDNS webhook HTTP API.
type Server struct {
	provider    provider.Provider
	maxBodySize int64
}

// NewServer returns a Server serving the given provider. A non-positive
// maxBodySize disables request body capping, mirroring
// --webhook-provider-max-body-size=0 on the external-dns side.
func NewServer(p provider.Provider, maxBodySize int64) *Server {
	return &Server{provider: p, maxBodySize: maxBodySize}
}

// ProviderHandler serves the endpoints external-dns calls on the provider:
//   - GET  /                negotiate the API and receive the DomainFilter
//   - GET  /records         list the records managed by the provider
//   - POST /records         apply a plan.Changes
//   - POST /adjustendpoints canonicalize candidate endpoints
func (s *Server) ProviderHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/", s.negotiateHandler)
	m.HandleFunc("/records", s.recordsHandler)
	m.HandleFunc("/adjustendpoints", s.adjustEndpointsHandler)
	return m
}

// ExposedHandler serves the endpoints the provider exposes to the cluster:
//   - GET /healthz liveness/readiness probe
//   - GET /metrics Prometheus metrics
func (s *Server) ExposedHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	m.HandleFunc("/metrics", promhttp.Handler().ServeHTTP)
	return m
}

func (s *Server) negotiateHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(contentTypeHeader, MediaType)
	if err := json.NewEncoder(w).Encode(s.provider.GetDomainFilter()); err != nil {
		// The header (and thus the 200 status) is already on its way.
		log.Errorf("webhook: failed to encode domain filter: %v", err)
	}
}

func (s *Server) recordsHandler(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		records, err := s.provider.Records(req.Context())
		if err != nil {
			log.Errorf("webhook: failed to get records: %v", err)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set(contentTypeHeader, MediaType)
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(records); err != nil {
			log.Errorf("webhook: failed to encode records: %v", err)
		}
	case http.MethodPost:
		var changes plan.Changes
		if err := s.decodeBody(w, req, &changes); err != nil {
			return
		}
		if err := s.provider.ApplyChanges(req.Context(), &changes); err != nil {
			log.Errorf("webhook: failed to apply changes: %v", err)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, req)
	}
}

func (s *Server) adjustEndpointsHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		methodNotAllowed(w, req)
		return
	}
	var eps []*endpoint.Endpoint
	if err := s.decodeBody(w, req, &eps); err != nil {
		return
	}
	adjusted, err := s.provider.AdjustEndpoints(eps)
	if err != nil {
		log.Errorf("webhook: failed to adjust endpoints: %v", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set(contentTypeHeader, MediaType)
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(adjusted); err != nil {
		log.Errorf("webhook: failed to encode adjusted endpoints: %v", err)
	}
}

// decodeBody reads and decodes the request body into v, capping its size at
// maxBodySize. Oversized bodies are rejected with 413, failed reads and
// malformed JSON with 400, all with a small error body external-dns can
// log. Reading the whole (already capped) body keeps the 413 decision
// deterministic instead of relying on net/http's implicit MaxBytesReader
// behavior, which does not fire the same way under every server type.
func (s *Server) decodeBody(w http.ResponseWriter, req *http.Request, v any) error {
	var body io.Reader = req.Body
	if s.maxBodySize > 0 {
		body = io.LimitReader(req.Body, s.maxBodySize+1)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		log.Errorf("webhook: failed to read request body: %v", err)
		writeError(w, http.StatusBadRequest, err)
		return err
	}
	if s.maxBodySize > 0 && int64(len(data)) > s.maxBodySize {
		err := fmt.Errorf("request body too large (limit %d bytes)", s.maxBodySize)
		log.Error(err)
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		log.Errorf("webhook: failed to decode request body: %v", err)
		writeError(w, http.StatusBadRequest, err)
		return err
	}
	return nil
}

// writeError sends a small JSON error body; the body is capped to keep it
// well under external-dns' 1 MiB drain limit so connections stay poolable.
func writeError(w http.ResponseWriter, status int, err error) {
	msg := err.Error()
	const maxMsg = 8 * 1024
	if len(msg) > maxMsg {
		msg = msg[:maxMsg]
	}
	w.Header().Set(contentTypeHeader, "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%s}`, jsonString(msg))
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"internal error"`
	}
	return string(b)
}

func methodNotAllowed(w http.ResponseWriter, req *http.Request) {
	log.Errorf("webhook: unsupported method %s on %s", req.Method, req.URL.Path)
	writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported method %s", req.Method))
}
