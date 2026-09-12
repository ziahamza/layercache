package telemetry

import (
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Handler instruments a server without ever exporting request targets, query
// strings, headers, or bodies. Go's ServeMux fills Request.Pattern with the
// static registered route, which becomes available after the wrapped handler
// has run and is safe to use as a bounded-cardinality attribute.
func (runtime *Runtime) Handler(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if runtime == nil || (!runtime.state.Traces && !runtime.state.Metrics) {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		runtime.serveHTTP(writer, request, next)
	})
}

func (runtime *Runtime) serveHTTP(writer http.ResponseWriter, request *http.Request, next http.Handler) {
	started := time.Now()
	method := normalizedMethod(request.Method)
	startAttributes := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("layercache.role", runtime.role),
	}

	requestContext := request.Context()
	var span trace.Span
	if runtime.state.Traces {
		requestContext = runtime.propagator.Extract(requestContext, propagation.HeaderCarrier(request.Header))
		requestContext, span = runtime.tracer.Start(
			requestContext,
			"HTTP request",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(startAttributes...),
		)
		defer span.End()
		request = request.WithContext(requestContext)
	}

	activeAttributes := metric.WithAttributes(startAttributes...)
	if runtime.state.Metrics {
		runtime.active.Add(requestContext, 1, activeAttributes)
		defer runtime.active.Add(requestContext, -1, activeAttributes)
	}

	response := &responseRecorder{ResponseWriter: writer}
	panicked := true
	defer func() {
		status := response.statusCode()
		if panicked {
			status = http.StatusInternalServerError
		}
		route := normalizedRoute(request.Pattern)
		integration := routeIntegration(route)
		finishedAttributes := []attribute.KeyValue{
			attribute.String("http.request.method", method),
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", status),
			attribute.String("layercache.integration", integration),
			attribute.String("layercache.role", runtime.role),
		}
		if runtime.state.Traces {
			span.SetName(method + " " + route)
			span.SetAttributes(finishedAttributes...)
			if status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
		}
		if runtime.state.Metrics {
			measurementOptions := metric.WithAttributes(finishedAttributes...)
			runtime.requests.Add(requestContext, 1, measurementOptions)
			runtime.duration.Record(requestContext, time.Since(started).Seconds(), measurementOptions)
		}
	}()

	next.ServeHTTP(response, request)
	panicked = false
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *responseRecorder) WriteHeader(status int) {
	if recorder.status != 0 {
		return
	}
	// Informational responses other than protocol switching are not final.
	// Forward them while allowing the handler to send its eventual status.
	if status >= 100 && status <= 199 && status != http.StatusSwitchingProtocols {
		recorder.ResponseWriter.WriteHeader(status)
		return
	}
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *responseRecorder) Write(body []byte) (int, error) {
	if recorder.status == 0 {
		recorder.WriteHeader(http.StatusOK)
	}
	return recorder.ResponseWriter.Write(body)
}

// Unwrap lets http.ResponseController retain Flush, Hijack, deadline, and
// full-duplex support from the underlying server writer.
func (recorder *responseRecorder) Unwrap() http.ResponseWriter {
	return recorder.ResponseWriter
}

func (recorder *responseRecorder) statusCode() int {
	if recorder.status == 0 {
		return http.StatusOK
	}
	return recorder.status
}

func normalizedMethod(method string) string {
	switch method {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func normalizedRoute(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || len(pattern) > 256 {
		return "unmatched"
	}
	return pattern
}

func routeIntegration(route string) string {
	switch {
	case strings.Contains(route, "/v8/artifacts"):
		return "turbo"
	case strings.Contains(route, "/_apis/artifactcache/") || strings.Contains(route, "/_layercache/compatibility/"):
		return "actions"
	case strings.Contains(route, "/v1/buildkit/"):
		return "buildkit"
	case strings.Contains(route, "public-build"):
		return "publicBuild"
	case strings.Contains(route, "/v1/public"):
		return "publicCache"
	default:
		return "control"
	}
}
