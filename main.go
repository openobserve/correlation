package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	serviceName = "example-service"
	o2Org       = "r1"
)

var (
	tracer  trace.Tracer
	meter   metric.Meter
	counter metric.Int64Counter
)

func initProvider() (func(context.Context) error, error) {
	ctx := context.Background()

	// Create resource with service information
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion("1.0.0"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Configure OTLP exporter
	traceExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint("localhost:5080"),
		otlptracehttp.WithURLPath("/api/"+o2Org+"/v1/traces"),
		otlptracehttp.WithInsecure(), // Explicitly use HTTP instead of HTTPS
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Basic cm9vdEBleGFtcGxlLmNvbTpDb21wbGV4cGFzcyMxMjM=",
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Configure trace provider
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tracerProvider)
	tracer = tracerProvider.Tracer(serviceName)

	// Configure metrics exporter
	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint("localhost:5080"),
		otlpmetrichttp.WithURLPath("/api/"+o2Org+"/v1/metrics"),
		otlpmetrichttp.WithInsecure(), // Explicitly use HTTP instead of HTTPS
		otlpmetrichttp.WithHeaders(map[string]string{
			"Authorization": "Basic cm9vdEBleGFtcGxlLmNvbTpDb21wbGV4cGFzcyMxMjM=",
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create metric exporter: %w", err)
	}

	// Configure metric provider with exemplar support
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithInterval(1*time.Second),
		)),
	)
	otel.SetMeterProvider(meterProvider)
	meter = meterProvider.Meter(serviceName)

	// Create a counter instrument
	var err2 error
	counter, err2 = meter.Int64Counter(
		"request_counter",
		metric.WithDescription("Counts the number of requests"),
	)
	if err2 != nil {
		return nil, fmt.Errorf("failed to create counter: %w", err2)
	}

	// Return shutdown function
	return func(ctx context.Context) error {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown trace provider: %w", err)
		}
		if err := meterProvider.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown meter provider: %w", err)
		}
		return nil
	}, nil
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Start a new span for this request
	ctx, span := tracer.Start(ctx, "handle_request")
	defer span.End()

	// Add attributes to the span
	span.SetAttributes(
		attribute.String("http.method", r.Method),
		attribute.String("http.url", r.URL.String()),
	)

	// Increment request counter with current trace context
	spanCtx := trace.SpanContextFromContext(ctx)
	counter.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("endpoint", r.URL.Path),
			attribute.String("trace_id", spanCtx.TraceID().String()),
			attribute.String("span_id", spanCtx.SpanID().String()),
		),
	)

	// Simulate some work
	time.Sleep(100 * time.Millisecond)

	fmt.Fprintf(w, "Hello, OpenTelemetry!")
}

func main() {
	// Initialize OpenTelemetry
	shutdown, err := initProvider()
	if err != nil {
		log.Fatal(err)
	}

	// Handle shutdown
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			log.Fatal(err)
		}
	}()

	// Set up HTTP server
	http.HandleFunc("/", handleRequest)

	log.Printf("Starting server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}
