package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"google.golang.org/grpc"
)

type Config struct {
	MetricsAddr     string
	TracingEndpoint string
}

// NewServerHandler returns the OTel gRPC stats handler as a ServerOption.
func NewServerHandler() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler())
}

// Setup initializes telemetry (Metrics and Tracing).
// It returns the Prometheus registry (to be served via HTTP) and a shutdown function.
func Setup(ctx context.Context, cfg Config) (*prometheus.Registry, func(context.Context) error, error) {
	slog.Info("Initializing telemetry", "config", cfg)

	// 1. Metrics (Prometheus)
	reg := prometheus.NewRegistry()
	// Register standard Go metrics
	// reg.MustRegister(collectors.NewGoCollector())
	// reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	// Note: We need to import "github.com/prometheus/client_golang/prometheus/collectors" to do this.
	// For now, let's skip default collectors to keep deps simple, or add them if needed.

	// 2. Tracing (OpenTelemetry)
	var tp *sdktrace.TracerProvider
	if cfg.TracingEndpoint != "" {
		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithInsecure(), // TODO: Support TLS
			otlptracegrpc.WithEndpoint(cfg.TracingEndpoint),
			otlptracegrpc.WithDialOption(), // Needed?
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
		}

		res, err := resource.Merge(
			resource.Default(),
			resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceName("gorego"),
				semconv.ServiceVersion("0.0.1"),
			),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create resource: %w", err)
		}

		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		)

		// Set global providers
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
	} else {
		// No-op provider
		tp = sdktrace.NewTracerProvider()
		otel.SetTracerProvider(tp)
	}

	shutdown := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			return fmt.Errorf("tracer provider shutdown failed: %w", err)
		}
		return nil
	}

	return reg, shutdown, nil
}
