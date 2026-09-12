package server

import (
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	boundedRequestBodyTimeout = 5 * time.Second
	streamRequestIdleTimeout  = 30 * time.Second
	streamResponseIdleTimeout = 30 * time.Second
)

func requestReadDeadlineHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		response := newProgressDeadlineResponseWriter(writer)
		defer response.clear()
		if request.Body == nil || request.Body == http.NoBody || request.ContentLength == 0 {
			next.ServeHTTP(response, request)
			return
		}
		controller := http.NewResponseController(response)
		if boundedRequestBody(request) {
			_ = controller.SetReadDeadline(time.Now().Add(boundedRequestBodyTimeout))
		} else {
			reset := func() { _ = controller.SetReadDeadline(time.Now().Add(streamRequestIdleTimeout)) }
			reset()
			request.Body = &progressDeadlineBody{body: request.Body, progress: reset}
		}
		defer controller.SetReadDeadline(time.Time{})
		next.ServeHTTP(response, request)
	})
}

type progressDeadlineResponseWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
}

func newProgressDeadlineResponseWriter(writer http.ResponseWriter) *progressDeadlineResponseWriter {
	return &progressDeadlineResponseWriter{
		ResponseWriter: writer,
		controller:     http.NewResponseController(writer),
	}
}

func (writer *progressDeadlineResponseWriter) WriteHeader(status int) {
	writer.reset()
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *progressDeadlineResponseWriter) Write(body []byte) (int, error) {
	writer.reset()
	return writer.ResponseWriter.Write(body)
}

func (writer *progressDeadlineResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *progressDeadlineResponseWriter) reset() {
	_ = writer.controller.SetWriteDeadline(time.Now().Add(streamResponseIdleTimeout))
}

func (writer *progressDeadlineResponseWriter) clear() {
	_ = writer.controller.SetWriteDeadline(time.Time{})
}

func boundedRequestBody(request *http.Request) bool {
	path := request.URL.Path
	if strings.HasPrefix(path, "/v1/auth/") || strings.HasPrefix(path, "/v1/public-build") ||
		strings.HasPrefix(path, "/v1/buildkit/promotion-leases/") || strings.HasPrefix(path, "/v1/members/") ||
		path == "/v1/public/revoke" || path == "/v8/artifacts/events" {
		return true
	}
	if strings.HasPrefix(path, "/_apis/artifactcache/") || strings.HasPrefix(path, "/_layercache/compatibility/") {
		return request.Method != http.MethodPatch
	}
	return request.Method == http.MethodPost && path != "/v1/public/publish"
}

type progressDeadlineBody struct {
	body     io.ReadCloser
	progress func()
}

func (body *progressDeadlineBody) Read(buffer []byte) (int, error) {
	read, err := body.body.Read(buffer)
	if read > 0 {
		body.progress()
	}
	return read, err
}

func (body *progressDeadlineBody) Close() error { return body.body.Close() }
