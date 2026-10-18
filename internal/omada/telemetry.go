package omada

import (
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Telemetry carries the tracer and meter the provider uses for the
// omada.ApplyChanges span and the omada.dns.* business instruments.
type Telemetry struct {
	Tracer trace.Tracer
	Meter  metric.Meter
}

// NoopTelemetry returns a Telemetry backed by no-op providers, for call
// sites and tests that do not want to record telemetry.
func NoopTelemetry() Telemetry {
	return Telemetry{
		Tracer: tracenoop.NewTracerProvider().Tracer("omada"),
		Meter:  noopmetric.NewMeterProvider().Meter("omada"),
	}
}
