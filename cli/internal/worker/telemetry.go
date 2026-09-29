package worker

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Telemetry starts OpenTelemetry for the worker when OTEL_EXPORTER_OTLP_ENDPOINT is set
// (docs/specs/operations.md, Telemetry): traces and metrics over OTLP/HTTP, configured by the
// standard OTEL_* variables. Without the variable it does nothing. The returned function flushes
// and stops it.
func Telemetry(ctx context.Context, version string) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName("casebox-worker"), semconv.ServiceVersion(version)))
	if err != nil {
		return nil, err
	}
	traces, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	metrics, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traces), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics)), sdkmetric.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	return func(ctx context.Context) error {
		err := tp.Shutdown(ctx)
		if err2 := mp.Shutdown(ctx); err == nil {
			err = err2
		}
		return err
	}, nil
}

// jobDuration records each job's time by kind and outcome; the global provider makes it a no-op
// until Telemetry starts one.
func recordJob(ctx context.Context, kind, outcome string, took time.Duration) {
	h, err := otel.Meter("casebox").Float64Histogram("casebox.worker.job.duration",
		metric.WithUnit("s"), metric.WithDescription("Time a worker spent on one job."))
	if err != nil {
		return
	}
	h.Record(ctx, took.Seconds(), metric.WithAttributes(attribute.String("kind", kind), attribute.String("outcome", outcome)))
}
