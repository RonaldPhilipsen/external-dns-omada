// Package telemetry wires the OpenTelemetry SDK for external-dns-omada:
// OTLP export for traces and metrics, selected by the standard OTEL_*
// environment variables, plus a Prometheus bridge for the /metrics
// endpoint. A missing or unreachable collector never prevents startup.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	promexport "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const defaultServiceName = "external-dns-omada"

const (
	protocolGRPC = "grpc"
	protocolHTTP = "http/protobuf"
)

// Telemetry holds the initialized OpenTelemetry providers together with the
// Prometheus registry that backs the /metrics endpoint. Create it with Init
// and call Shutdown when the process exits.
type Telemetry struct {
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	registry       *prometheus.Registry

	shutdownOnce sync.Once
}

// Init starts the OpenTelemetry SDK. Traces and metrics are exported over
// OTLP unless OTEL_TRACES_EXPORTER or OTEL_METRICS_EXPORTER contains "none";
// the export protocol comes from OTEL_EXPORTER_OTLP_TRACES_PROTOCOL and
// OTEL_EXPORTER_OTLP_METRICS_PROTOCOL, falling back to
// OTEL_EXPORTER_OTLP_PROTOCOL (default grpc). OTel metrics are additionally
// bridged into a Prometheus registry carrying the Go runtime and process
// collectors. The service is named by OTEL_SERVICE_NAME (default
// external-dns-omada) and carries version as its service version.
func Init(ctx context.Context, version string) (*Telemetry, error) {
	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName()),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, err
	}

	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if signalEnabled("OTEL_TRACES_EXPORTER") {
		exp, err := newTraceExporter(ctx)
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	promExpo, err := promexport.New(promexport.WithRegisterer(reg))
	if err != nil {
		return nil, err
	}

	mpOpts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExpo),
	}
	if signalEnabled("OTEL_METRICS_EXPORTER") {
		exp, err := newMetricExporter(ctx)
		if err != nil {
			return nil, err
		}
		interval, err := metricExportInterval()
		if err != nil {
			return nil, err
		}
		mpOpts = append(mpOpts,
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	mp := sdkmetric.NewMeterProvider(mpOpts...)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Telemetry{
		tracerProvider: tp,
		meterProvider:  mp,
		registry:       reg,
	}, nil
}

// Tracer returns a named tracer from the initialized provider. It never
// returns nil, even when trace export is disabled.
func (t *Telemetry) Tracer(name string) trace.Tracer {
	return t.tracerProvider.Tracer(name)
}

// Meter returns a named meter from the initialized provider. It never
// returns nil, even when metric export is disabled.
func (t *Telemetry) Meter(name string) metric.Meter {
	return t.meterProvider.Meter(name)
}

// Registry returns the Prometheus registry backing the /metrics endpoint;
// it carries the OTel metrics bridge plus the Go runtime and process
// collectors.
func (t *Telemetry) Registry() *prometheus.Registry {
	return t.registry
}

// Shutdown flushes and stops both providers, joining their errors. It is
// idempotent: only the first call does anything, later calls return the same
// error.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var err error
	t.shutdownOnce.Do(func() {
		var errs []error
		if e := t.tracerProvider.Shutdown(ctx); e != nil {
			errs = append(errs, fmt.Errorf("shutting down tracer provider: %w", e))
		}
		if e := t.meterProvider.Shutdown(ctx); e != nil {
			errs = append(errs, fmt.Errorf("shutting down meter provider: %w", e))
		}
		err = errors.Join(errs...)
	})
	return err
}

// newTraceExporter builds the OTLP trace exporter for the protocol selected
// by OTEL_EXPORTER_OTLP_TRACES_PROTOCOL, falling back to
// OTEL_EXPORTER_OTLP_PROTOCOL.
func newTraceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	p, err := protocol("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")
	if err != nil {
		return nil, err
	}
	switch p {
	case protocolGRPC:
		return otlptracegrpc.New(ctx)
	default:
		return otlptracehttp.New(ctx)
	}
}

// newMetricExporter builds the OTLP metric exporter for the protocol
// selected by OTEL_EXPORTER_OTLP_METRICS_PROTOCOL, falling back to
// OTEL_EXPORTER_OTLP_PROTOCOL.
func newMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	p, err := protocol("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")
	if err != nil {
		return nil, err
	}
	switch p {
	case protocolGRPC:
		return otlpmetricgrpc.New(ctx)
	default:
		return otlpmetrichttp.New(ctx)
	}
}

// protocol resolves the OTLP protocol for one signal: the signal-specific
// environment variable wins over the shared fallback, the default is grpc,
// and only grpc and http/protobuf are supported.
func protocol(envKey, fallbackKey string) (string, error) {
	p := os.Getenv(envKey)
	if p == "" {
		p = os.Getenv(fallbackKey)
	}
	if p == "" {
		p = protocolGRPC
	}
	if p == protocolGRPC || p == protocolHTTP {
		return p, nil
	}
	return "", fmt.Errorf("unsupported OTLP protocol %q for %s (supported: %q, %q)",
		p, envKey, protocolGRPC, protocolHTTP)
}

// signalEnabled reports whether the signal behind envKey is exported. The
// unset variable follows the OTel spec default (export over OTLP); a "none"
// entry in the comma-separated value disables the signal.
func signalEnabled(envKey string) bool {
	for _, e := range strings.Split(os.Getenv(envKey), ",") {
		if strings.TrimSpace(e) == "none" {
			return false
		}
	}
	return true
}

// metricExportInterval returns OTEL_METRIC_EXPORT_INTERVAL as a Go duration,
// defaulting to one minute.
func metricExportInterval() (time.Duration, error) {
	v := os.Getenv("OTEL_METRIC_EXPORT_INTERVAL")
	if v == "" {
		return time.Minute, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse OTEL_METRIC_EXPORT_INTERVAL %q as a Go duration: %w", v, err)
	}
	return d, nil
}

// serviceName returns OTEL_SERVICE_NAME if set, else the package default.
func serviceName() string {
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		return v
	}
	return defaultServiceName
}
