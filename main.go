package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"log"

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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var requestCount atomic.Int64

// Add constants before the function:
const (
	EnvOtlpEndpoint  = "OTLP_ENDPOINT"
	EnvOtlpAuthToken = "OTLP_AUTH_TOKEN"
	DefaultEndpoint  = "localhost:5080"
	DefaultAuthToken = "cm9vdEBleGFtcGxlLmNvbTpDb21wbGV4cGFzcyMxMjM=" // base64("root@example.com:Complexpass#123")
)

// Add helper function:
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// otlpWriter implements zapcore.WriteSyncer interface
type otlpWriter struct {
	endpoint string
	headers  map[string]string
}

func (w *otlpWriter) Write(p []byte) (n int, err error) {
	req, err := http.NewRequest("POST", w.endpoint, bytes.NewBuffer(p))
	if err != nil {
		return 0, err
	}

	for k, v := range w.headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return len(p), nil
}

func (w *otlpWriter) Sync() error {
	return nil
}

const (
	serviceName = "example-service"
)

var (
	tracer  trace.Tracer
	meter   metric.Meter
	counter metric.Int64Counter
	logger  *zap.Logger
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
		otlptracehttp.WithEndpoint(getEnvOrDefault(EnvOtlpEndpoint, DefaultEndpoint)),
		otlptracehttp.WithURLPath("/api/default/v1/traces"),
		otlptracehttp.WithInsecure(), // Explicitly use HTTP instead of HTTPS
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + getEnvOrDefault(EnvOtlpAuthToken, DefaultAuthToken),
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Configure trace provider
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(0.1)), // Changed to 10% sampling
	)
	otel.SetTracerProvider(tracerProvider)
	tracer = tracerProvider.Tracer(serviceName)

	// Configure metrics exporter
	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(getEnvOrDefault(EnvOtlpEndpoint, DefaultEndpoint)),
		otlpmetrichttp.WithURLPath("/api/default/v1/metrics"),
		otlpmetrichttp.WithInsecure(), // Explicitly use HTTP instead of HTTPS
		otlpmetrichttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + getEnvOrDefault(EnvOtlpAuthToken, DefaultAuthToken),
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

	// Configure Zap logger
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "message",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// Create core for OTLP HTTP endpoint
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(&otlpWriter{
			endpoint: "http://" + getEnvOrDefault(EnvOtlpEndpoint, DefaultEndpoint) + "/api/default/default/_json",
			headers: map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Basic  " + getEnvOrDefault(EnvOtlpAuthToken, DefaultAuthToken),
			},
		}),
		zapcore.InfoLevel,
	)

	// Create logger
	logger = zap.New(core,
		zap.WithCaller(true),
		zap.Fields(
			zap.String("service.name", serviceName),
			zap.String("service.version", "1.0.0"),
		),
	)
	defer logger.Sync()

	// Return shutdown function
	return func(ctx context.Context) error {
		if err := tracerProvider.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown trace provider: %w", err)
		}
		if err := meterProvider.Shutdown(ctx); err != nil {
			return fmt.Errorf("failed to shutdown meter provider: %w", err)
		}
		// No logsProvider to shutdown
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

	// Add log with trace context
	logger.Info("Handling request",
		zap.String("http.method", r.Method),
		zap.String("http.url", r.URL.String()),
		zap.String("trace_id", span.SpanContext().TraceID().String()),
		zap.String("span_id", span.SpanContext().SpanID().String()),
	)

	// Increment request counter with current trace context
	// spanCtx := trace.SpanContextFromContext(ctx)
	currentCount := requestCount.Add(1)
	measurementOption := metric.WithAttributes(
		attribute.String("endpoint", r.URL.Path),
	)
	counter.Add(ctx, currentCount, measurementOption)

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
