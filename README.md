# external-dns-omada

`external-dns-omada` runs the Omada DNS provider as an
[ExternalDNS webhook provider](https://github.com/kubernetes-sigs/external-dns/blob/master/docs/tutorials/webhook-provider.md).

It is a small sidecar that talks to an ExternalDNS instance running with
`--provider=webhook`. The sidecar owns the real work — authenticating against
a TP-Link Omada controller and applying the planned changes to the site's LAN
DNS entries — while ExternalDNS owns record discovery, filtering, and planning.

Upstream ExternalDNS does not accept new in-tree providers, so the provider
logic lives here, out of tree, and is consumed through the generic webhook
contract instead of being registered in the ExternalDNS binary.

## How it fits together

```
ExternalDNS                       external-dns-omada (this image)
(--provider=webhook)   localhost  (Omada API client + provider)
  +----------------+   :8888   +--------------------------+
  | webhook client | --------> | Negotiate   GET  /       |
  |                | --------- | Records     GET  /records|
  |                | --------- | ApplyChanges POST /records|
  |                | --------- | AdjustEndpoints POST /adjustendpoints |
  +----------------+           +--------------------------+
                                          |  Omada OpenAPI
                                          v
                                   Omada controller
```

- The provider API listens on `127.0.0.1:8888` by default and is only shared
  with the ExternalDNS container in the same pod. Do not expose it.
- `/healthz` and `/metrics` are exposed on `0.0.0.0:8080` for cluster probes
  and scraping.
- The only contract the two sides must agree on is the webhook media type
  `application/external.dns.webhook+json;version=1`. That is asserted in the
  test suite by driving this server with the real upstream webhook client
  (`TestUpstreamClientContract`).

## Running it

Every flag also has an environment variable; a flag always wins over the
environment.

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--omada-server` | `OMADA_SERVER` | — (required) | Base URL of the Omada controller, e.g. `https://192.168.1.1:8043` |
| `--omada-id` | `OMADA_ID` | — (required) | Omada ID of the controller (`OM-XXXXXXXX`) |
| `--omada-client-id` | `OMADA_CLIENT_ID` | — (required) | Client ID of the custom app used for auth |
| `--omada-client-secret` | `OMADA_CLIENT_SECRET` | — (required) | Client secret of the custom app |
| `--omada-site-name` | `OMADA_SITE_NAME` | — (required) | Name of the site whose LAN DNS entries are managed |
| `--omada-networks` | `OMADA_NETWORKS` | all LAN networks | Comma-separated LAN network names entries must be attached to |
| `--omada-tls-skip-verify` | `OMADA_TLS_SKIP_VERIFY` | `false` | Disable TLS certificate verification |
| `--dry-run` | `DRY_RUN` | `false` | Report what would change without applying it |
| `--domain-filter` | `DOMAIN_FILTER` | none | Comma-separated domain suffixes this instance manages |
| `--webhook-listen` | `WEBHOOK_LISTEN` | `127.0.0.1:8888` | Listen address of the provider API; keep it on localhost |
| `--metrics-listen` | `METRICS_LISTEN` | `0.0.0.0:8080` | Listen address of `/healthz` and `/metrics` |
| `--webhook-read-timeout` | `WEBHOOK_READ_TIMEOUT` | `30s` | Max time to read a request body |
| `--webhook-write-timeout` | `WEBHOOK_WRITE_TIMEOUT` | `60s` | Max time to write a response |
| `--max-body-size` | `MAX_BODY_SIZE` | `33554432` | Max body size in bytes; larger requests get 413, `0` disables |
| `--log-level` | `LOG_LEVEL` | `info` | `panic`, `error`, `warn`, `info`, `debug`, `trace` |
| `--log-format` | `LOG_FORMAT` | `logfmt` | `logfmt` or `json` |

On startup it authenticates against the controller and resolves the site and
LAN networks, so a misconfigured secret fails immediately instead of on the
first reconcile.

### As a sidecar with ExternalDNS

See `examples/kubernetes/deployment.yaml` for a self-contained example, or use
the stock `external-dns` Helm chart, which renders the webhook sidecar
automatically when the provider is `webhook`:

```yaml
values:
  provider:
    name: webhook
    webhook:
      image:
        repository: ghcr.io/RonaldPhilipsen/external-dns-omada
        tag: v0.1.0
      env:
        - name: OMADA_SERVER
          valueFrom:
            secretKeyRef:
              name: external-dns-omada
              key: OMADA_SERVER
```

ExternalDNS is then configured with `--provider=webhook` (the default
`--webhook-provider-url=http://localhost:8888` already points at the sidecar).

### On a single host

```sh
external-dns-omada \
  --omada-server=https://192.168.1.1:8043 \
  --omada-id=OM-XXXXXXXX \
  --omada-client-id=client-id \
  --omada-client-secret=secret \
  --omada-site-name=Home
```

Then point an ExternalDNS instance at it with
`--provider=webhook --webhook-provider-url=http://localhost:8888`.

## Telemetry

Traces and metrics are exported to an OpenTelemetry collector over OTLP,
configured by the standard `OTEL_*` environment variables — no flags. A
missing or unreachable collector never prevents startup; the exporter drops
its batches in the background (SDK-internal errors go to stderr).

What is emitted:

- a server span per webhook request, and per `/healthz` and `/metrics`
  request;
- a client span per Omada API call — auth/refresh, site and network
  resolution, LAN DNS list pages, create/update/delete — with method, path,
  and response status;
- an `omada.ApplyChanges` span per reconcile batch with the planned
  create/update/delete counts and the dry-run flag, marked as an error when
  the batch fails;
- the `omada.dns.entries` gauge (entries the provider last reported), the
  `omada.dns.changes` counter (`created`/`updated`/`deleted`/`skipped`/
  `dry_run`), and the `omada.dns.errors` counter (`list`/`create`/`update`/
  `delete`/`validate`), plus the OTel HTTP server and client duration
  metrics.

| Env | Default | Effect |
|-----|---------|--------|
| `OTEL_SERVICE_NAME` | `external-dns-omada` | `service.name` resource attribute |
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `..._TRACES_ENDPOINT` / `..._METRICS_ENDPOINT` | `http://localhost:4317` | collector endpoint |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | `grpc` (4317) or `http/protobuf` (4318) |
| `OTEL_EXPORTER_OTLP_HEADERS` | — | auth headers if the collector needs them |
| `OTEL_TRACES_EXPORTER` / `OTEL_METRICS_EXPORTER` | `otlp` | set to `none` to disable one signal |
| `OTEL_METRIC_EXPORT_INTERVAL` | `60s` | OTLP metric export interval |

`/metrics` also serves the OTel-bridged metrics in Prometheus format
alongside the `go_*`/`process_*` runtime series, so existing scrapers keep
working. The only series removed from the endpoint are the Omada
HTTP-client duration summary (superseded by `http.client.request.duration`)
and `external_dns_build_info`.

```yaml
env:
  # OTLP gRPC is the default protocol.
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability.svc:4317
# For an HTTP collector on 4318, add these instead:
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability.svc:4318
  - name: OTEL_EXPORTER_OTLP_PROTOCOL
    value: http/protobuf
```

## Building

```sh
go build -o external-dns-omada .
```

`VERSION` and the commit SHA are stamped into the binary via `-ldflags` at
image build time (see `Dockerfile` and the release workflow).

## Developing

```sh
go test -race ./...
```

The layout:

| Package | Purpose |
|---------|---------|
| `internal/config` | Flag + env parsing and validation |
| `internal/omada` | Omada OpenAPI client (`client.go`) and the provider (`provider.go`) |
| `internal/telemetry` | OTel SDK setup: OTLP trace/metric export, /metrics Prometheus bridge |
| `internal/webhook` | The webhook HTTP server and its handler wiring |
| `internal/sets` | Small generic set helper |

The provider and client are a port of the in-tree `provider/omada` package and
keep the same behavior: A/AAAA and CNAME records, per-entry LAN network
binding, `entry.id`/`entry.enable`/`entry.network.ids` provider-specific
properties, and spaced-out writes.
