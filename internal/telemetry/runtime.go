// Package telemetry exports bounded, non-sensitive operational telemetry.
// Product-owned cache outcomes remain in the measurement package; this package
// deliberately records only static route templates, integration names, status
// codes, and request timing.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/layercache/layercache/internal/telemetry"

// Options names the process without adding project identities, paths, cache
// keys, or other user-controlled values to exported telemetry.
type Options struct {
	ServiceName    string
	ServiceVersion string
	Role           string
}

// State is safe to expose through status and diagnostics.
type State struct {
	Configured bool `json:"configured"`
	Traces     bool `json:"traces"`
	Metrics    bool `json:"metrics"`
}

// Runtime owns providers rather than replacing OpenTelemetry's process-global
// providers. Multiple installed-system servers can therefore coexist in tests
// without changing each other's telemetry destination.
type Runtime struct {
	state          State
	role           string
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	tracer         trace.Tracer
	requests       metric.Int64Counter
	duration       metric.Float64Histogram
	active         metric.Int64UpDownCounter
	propagator     propagation.TraceContext
}

// NewFromEnvironment enables OTLP/HTTP only when a standard OTLP endpoint is
// explicitly configured. A signal-specific endpoint enables only that signal;
// OTEL_EXPORTER_OTLP_ENDPOINT enables both. Exporter options such as TLS,
// headers, compression, timeout, and protocol continue to come from the
// standard OpenTelemetry environment variables.
//
// A partially initialized Runtime may be returned with an error. The caller
// should report the error and keep serving with the signal that initialized.
func NewFromEnvironment(ctx context.Context, options Options) (*Runtime, error) {
	if strings.TrimSpace(options.ServiceName) == "" {
		return Disabled(), errors.New("OpenTelemetry service name is required")
	}
	runtime := Disabled()
	runtime.role = normalizeRole(options.Role)
	if sdkDisabled() {
		return runtime, nil
	}

	tracesEnabled := signalEnabled("TRACES")
	metricsEnabled := signalEnabled("METRICS")
	if !tracesEnabled && !metricsEnabled {
		return runtime, nil
	}
	runtime.state.Configured = true

	attributes := []attribute.KeyValue{
		attribute.String("service.name", strings.TrimSpace(options.ServiceName)),
		attribute.String("service.version", normalizedVersion(options.ServiceVersion)),
		attribute.String("layercache.role", runtime.role),
	}
	processResource := resource.NewSchemaless(attributes...)

	var initializationErrors []error
	if tracesEnabled {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			initializationErrors = append(initializationErrors, fmt.Errorf("configure OTLP trace exporter: %w", err))
		} else {
			runtime.tracerProvider = sdktrace.NewTracerProvider(
				sdktrace.WithResource(processResource),
				sdktrace.WithBatcher(exporter),
			)
			runtime.tracer = runtime.tracerProvider.Tracer(instrumentationName)
			runtime.state.Traces = true
		}
	}

	if metricsEnabled {
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			initializationErrors = append(initializationErrors, fmt.Errorf("configure OTLP metric exporter: %w", err))
		} else {
			reader := sdkmetric.NewPeriodicReader(exporter)
			runtime.meterProvider = sdkmetric.NewMeterProvider(
				sdkmetric.WithResource(processResource),
				sdkmetric.WithReader(reader),
			)
			meter := runtime.meterProvider.Meter(instrumentationName)
			runtime.requests, err = meter.Int64Counter(
				"layercache.http.server.requests",
				metric.WithDescription("Completed Layer Cache HTTP requests"),
				metric.WithUnit("{request}"),
			)
			if err == nil {
				runtime.duration, err = meter.Float64Histogram(
					"layercache.http.server.duration",
					metric.WithDescription("Layer Cache HTTP request duration"),
					metric.WithUnit("s"),
				)
			}
			if err == nil {
				runtime.active, err = meter.Int64UpDownCounter(
					"layercache.http.server.active_requests",
					metric.WithDescription("Layer Cache HTTP requests currently being served"),
					metric.WithUnit("{request}"),
				)
			}
			if err != nil {
				initializationErrors = append(initializationErrors, fmt.Errorf("configure OpenTelemetry HTTP instruments: %w", err))
				_ = runtime.meterProvider.Shutdown(ctx)
				runtime.meterProvider = nil
			} else {
				runtime.state.Metrics = true
			}
		}
	}

	if !runtime.state.Traces && !runtime.state.Metrics {
		runtime.state.Configured = false
	}
	return runtime, errors.Join(initializationErrors...)
}

// Disabled returns a no-op Runtime suitable for fail-open initialization.
func Disabled() *Runtime {
	return &Runtime{role: "unknown"}
}

func (runtime *Runtime) State() State {
	if runtime == nil {
		return State{}
	}
	return runtime.state
}

// Shutdown flushes pending spans and metrics and releases both exporters.
func (runtime *Runtime) Shutdown(ctx context.Context) error {
	if runtime == nil {
		return nil
	}
	var shutdownErrors []error
	if runtime.tracerProvider != nil {
		if err := runtime.tracerProvider.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("shut down OpenTelemetry traces: %w", err))
		}
	}
	if runtime.meterProvider != nil {
		if err := runtime.meterProvider.Shutdown(ctx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("shut down OpenTelemetry metrics: %w", err))
		}
	}
	return errors.Join(shutdownErrors...)
}

func sdkDisabled() bool {
	value := strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED"))
	return strings.EqualFold(value, "true") || value == "1"
}

func signalEnabled(signal string) bool {
	exporter := strings.TrimSpace(os.Getenv("OTEL_" + signal + "_EXPORTER"))
	if strings.EqualFold(exporter, "none") {
		return false
	}
	return strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT")) != "" ||
		strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

func normalizeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "local", "team", "public":
		return strings.ToLower(strings.TrimSpace(role))
	default:
		return "unknown"
	}
}

func normalizedVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "development"
	}
	return version
}
