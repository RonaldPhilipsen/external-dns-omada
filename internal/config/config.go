/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package config parses the webhook provider's runtime configuration from
// command-line flags and environment variables, with the precedence:
// flag > environment variable > default.
package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/RonaldPhilipsen/external-dns-omada/internal/omada"
)

// Config is the runtime configuration of the webhook provider server.
type Config struct {
	Omada omada.Config

	// WebhookListen is the address the provider API listens on. It should
	// stay on localhost, shared only with the external-dns container of
	// the same pod.
	WebhookListen string
	// MetricsListen is the address the healthz and metrics endpoints are
	// exposed on; it is meant for cluster-wide probes and scraping.
	MetricsListen string
	// ReadTimeout bounds how long the server waits to read a request body.
	ReadTimeout time.Duration
	// WriteTimeout bounds how long the server takes to write a response.
	WriteTimeout time.Duration
	// MaxBodySize caps request and response bodies in bytes; 0 disables
	// the cap.
	MaxBodySize int64

	LogLevel  string
	LogFormat string
}

// Parse reads the configuration from args. args must not contain the
// program name; pass os.Args[1:].
func Parse(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("external-dns-omada", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&c.Omada.Server, "omada-server", envOr("OMADA_SERVER", ""),
		"Base URL of the Omada controller, e.g. https://192.168.1.1:8043 (required)")
	fs.StringVar(&c.Omada.ID, "omada-id", envOr("OMADA_ID", ""),
		"Omada ID of the controller (OM-XXXXXXXX) (required)")
	fs.StringVar(&c.Omada.ClientID, "omada-client-id", envOr("OMADA_CLIENT_ID", ""),
		"Client ID of the custom app used for client-credentials authentication (required)")
	fs.StringVar(&c.Omada.ClientSecret, "omada-client-secret", envOr("OMADA_CLIENT_SECRET", ""),
		"Client secret of the custom app (required)")
	fs.StringVar(&c.Omada.SiteName, "omada-site-name", envOr("OMADA_SITE_NAME", ""),
		"Name of the site whose LAN DNS entries are managed (required)")
	fs.StringVar(&c.Omada.Networks, "omada-networks", envOr("OMADA_NETWORKS", ""),
		"Comma-separated list of LAN network names managed entries must be attached to; empty attaches entries to all of the site's LAN networks")
	fs.BoolVar(&c.Omada.TLSInsecureSkipVerify, "omada-tls-skip-verify", envBoolOr("OMADA_TLS_SKIP_VERIFY", false),
		"Disable verification of any TLS certificates")
	fs.BoolVar(&c.Omada.DryRun, "dry-run", envBoolOr("DRY_RUN", false),
		"Report what would change without applying it")
	domainFilter := fs.String("domain-filter", envOr("DOMAIN_FILTER", ""),
		"Comma-separated list of domain suffixes this instance manages; empty manages everything external-dns reports")

	fs.StringVar(&c.WebhookListen, "webhook-listen", envOr("WEBHOOK_LISTEN", "127.0.0.1:8888"),
		"Listen address of the provider API endpoints; keep it on localhost next to external-dns")
	fs.StringVar(&c.MetricsListen, "metrics-listen", envOr("METRICS_LISTEN", "0.0.0.0:8080"),
		"Listen address of the /healthz and /metrics endpoints")
	fs.DurationVar(&c.ReadTimeout, "webhook-read-timeout", durationOrEnv("WEBHOOK_READ_TIMEOUT", 30*time.Second),
		"Maximum time to read a request body; a body still being sent after this deadline fails the request")
	fs.DurationVar(&c.WriteTimeout, "webhook-write-timeout", durationOrEnv("WEBHOOK_WRITE_TIMEOUT", 60*time.Second),
		"Maximum time to write a response; long batches of Omada writes need their own budget on top")
	fs.Int64Var(&c.MaxBodySize, "max-body-size", int64Or("MAX_BODY_SIZE", int64(32<<20)),
		"Maximum size in bytes of a webhook request or response body; larger requests are rejected with 413, 0 disables")

	fs.StringVar(&c.LogLevel, "log-level", envOr("LOG_LEVEL", "info"),
		"Log level: panic, error, warn, info, debug or trace")
	fs.StringVar(&c.LogFormat, "log-format", envOr("LOG_FORMAT", "logfmt"),
		"Log format: logfmt or json")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unknown arguments: %s", strings.Join(fs.Args(), " "))
	}

	var missing []string
	if c.Omada.Server == "" {
		missing = append(missing, "omada-server")
	}
	if c.Omada.ID == "" {
		missing = append(missing, "omada-id")
	}
	if c.Omada.ClientID == "" {
		missing = append(missing, "omada-client-id")
	}
	if c.Omada.ClientSecret == "" {
		missing = append(missing, "omada-client-secret")
	}
	if c.Omada.SiteName == "" {
		missing = append(missing, "omada-site-name")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}

	c.Omada.DomainFilter = splitList(*domainFilter)
	return c, nil
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBoolOr(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func durationOrEnv(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func int64Or(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	}
	return def
}

// splitList splits a comma-separated list, trimming spaces and dropping
// empty items, so "lan, .home.lan" behaves like "lan,.home.lan".
func splitList(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
