package buildkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const (
	maxProgressLineBytes = 4 << 20
	maxProgressVertices  = 100_000
	maxProgressStatuses  = 200_000
)

type ProgressMetrics struct {
	Vertices               int          `json:"vertices"`
	CompletedVertices      int          `json:"completedVertices"`
	CachedVertices         int          `json:"cachedVertices"`
	CacheHitRate           float64      `json:"cacheHitRate"`
	BuildDurationMS        int64        `json:"buildDurationMs"`
	RequestedCacheScopes   []CacheScope `json:"requestedCacheScopes,omitempty"`
	CacheSource            string       `json:"cacheSource,omitempty"`
	RemoteBytes            int64        `json:"remoteBytes"`
	RemoteBytesMeasured    bool         `json:"remoteBytesMeasured"`
	RemoteDownloadedBytes  int64        `json:"remoteDownloadedBytes"`
	RemoteUploadedBytes    int64        `json:"remoteUploadedBytes"`
	GraphDigest            string       `json:"graphDigest,omitempty"`
	TeamPromotionAttempted bool         `json:"teamPromotionAttempted"`
	TeamPromotionSucceeded bool         `json:"teamPromotionSucceeded"`
}

type progressVertex struct {
	ID        string          `json:"id"`
	Digest    string          `json:"digest"`
	Name      string          `json:"name"`
	Inputs    []string        `json:"inputs"`
	Completed json.RawMessage `json:"completed"`
	Cached    bool            `json:"cached"`
	Error     string          `json:"error"`
}

type progressStatus struct {
	ID      string `json:"id"`
	Vertex  string `json:"vertex"`
	Name    string `json:"name"`
	Current int64  `json:"current"`
}

type progressEvent struct {
	progressVertex
	Vertexes []progressVertex `json:"vertexes"`
	Statuses []progressStatus `json:"statuses"`
}

type vertexState struct {
	name      string
	inputs    []string
	completed bool
	cached    bool
	failed    bool
}

type statusState struct {
	vertex  string
	name    string
	current int64
}

type progressAccumulator struct {
	vertices map[string]vertexState
	statuses map[string]statusState
	warnings []error
}

func newProgressAccumulator() *progressAccumulator {
	return &progressAccumulator{
		vertices: make(map[string]vertexState),
		statuses: make(map[string]statusState),
	}
}

func (accumulator *progressAccumulator) addLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var event progressEvent
	if err := json.Unmarshal(line, &event); err != nil {
		accumulator.warn(fmt.Errorf("decode BuildKit progress: %w", err))
		return
	}
	if len(event.Vertexes) == 0 && (event.ID != "" || event.Digest != "") {
		event.Vertexes = []progressVertex{event.progressVertex}
	}
	for _, vertex := range event.Vertexes {
		identity := vertex.Digest
		if identity == "" {
			identity = vertex.ID
		}
		if identity == "" {
			continue
		}
		state, found := accumulator.vertices[identity]
		if !found && len(accumulator.vertices) >= maxProgressVertices {
			accumulator.warn(fmt.Errorf("BuildKit progress exceeded %d unique vertices", maxProgressVertices))
			continue
		}
		if vertex.Name != "" {
			state.name = vertex.Name
		}
		if len(vertex.Inputs) > 0 {
			state.inputs = append(state.inputs[:0], vertex.Inputs...)
		}
		if len(vertex.Completed) > 0 && string(vertex.Completed) != "null" {
			state.completed = true
		}
		state.cached = state.cached || vertex.Cached
		if vertex.Error != "" && !state.failed {
			state.failed = true
			accumulator.warn(errors.New("BuildKit reported a vertex failure while continuing the build"))
		}
		accumulator.vertices[identity] = state
	}
	for _, status := range event.Statuses {
		if status.ID == "" || status.Current < 0 {
			continue
		}
		identity := status.Vertex + "\x00" + status.ID
		state, found := accumulator.statuses[identity]
		if !found && len(accumulator.statuses) >= maxProgressStatuses {
			accumulator.warn(fmt.Errorf("BuildKit progress exceeded %d transfer statuses", maxProgressStatuses))
			continue
		}
		if status.Vertex != "" {
			state.vertex = status.Vertex
		}
		if status.Name != "" {
			state.name = status.Name
		}
		if status.Current > state.current {
			state.current = status.Current
		}
		accumulator.statuses[identity] = state
	}
}

func (accumulator *progressAccumulator) warn(err error) {
	if len(accumulator.warnings) < 8 {
		accumulator.warnings = append(accumulator.warnings, err)
	}
}

func (accumulator *progressAccumulator) metrics(remoteReferences []string) ProgressMetrics {
	scopes := make([]CacheScope, 0, len(remoteReferences))
	for _, reference := range remoteReferences {
		scopes = append(scopes, CacheScope{Reference: reference})
	}
	return accumulator.metricsForScopes(scopes)
}

func (accumulator *progressAccumulator) metricsForScopes(remoteScopes []CacheScope) ProgressMetrics {
	metrics := ProgressMetrics{Vertices: len(accumulator.vertices)}
	graphVertices := make([]string, 0, len(accumulator.vertices))
	for identity, state := range accumulator.vertices {
		if !state.completed {
			continue
		}
		metrics.CompletedVertices++
		if graphIdentityVertex(state.name) {
			graphVertices = append(graphVertices, identity)
		}
		if state.cached {
			metrics.CachedVertices++
		}
	}
	if len(graphVertices) > 0 {
		signatures := make([]string, 0, len(graphVertices))
		memo := make(map[string]string, len(graphVertices))
		visiting := make(map[string]bool, len(graphVertices))
		for _, identity := range graphVertices {
			signatures = append(signatures, accumulator.graphVertexSignature(identity, memo, visiting))
		}
		sort.Strings(signatures)
		digest := sha256.New()
		for _, signature := range signatures {
			_, _ = digest.Write([]byte(signature))
			_, _ = digest.Write([]byte{0})
		}
		metrics.GraphDigest = fmt.Sprintf("sha256:%x", digest.Sum(nil))
	}
	if metrics.CompletedVertices > 0 {
		metrics.CacheHitRate = float64(metrics.CachedVertices) / float64(metrics.CompletedVertices)
	}
	for _, status := range accumulator.statuses {
		vertexName := accumulator.vertices[status.vertex].name
		if status.current <= 0 {
			continue
		}
		direction, found := matchingRemoteDirection(status.name+"\n"+vertexName, remoteScopes)
		if found {
			metrics.RemoteBytesMeasured = true
			metrics.RemoteBytes += status.current
			switch direction {
			case "import":
				metrics.RemoteDownloadedBytes += status.current
			case "export":
				metrics.RemoteUploadedBytes += status.current
			}
		}
	}
	return metrics
}

// graphVertexSignature canonicalizes the observed vertex name and dependency
// structure. BuildKit's raw vertex digests include solve-session identities and
// can change between an otherwise identical cold and warm build, so they are
// used only to join progress events and never persisted as graph identity.
func (accumulator *progressAccumulator) graphVertexSignature(
	identity string,
	memo map[string]string,
	visiting map[string]bool,
) string {
	if signature, found := memo[identity]; found {
		return signature
	}
	if visiting[identity] {
		return "cycle"
	}
	visiting[identity] = true
	state := accumulator.vertices[identity]
	name := strings.TrimSpace(state.name)
	if name == "" {
		name = "unnamed"
	}
	dependencies := make([]string, 0, len(state.inputs))
	for _, input := range state.inputs {
		dependency, found := accumulator.vertices[input]
		if !found || !dependency.completed || !graphIdentityVertex(dependency.name) {
			dependencies = append(dependencies, "external")
			continue
		}
		dependencies = append(dependencies, accumulator.graphVertexSignature(input, memo, visiting))
	}
	sort.Strings(dependencies)
	digest := sha256.New()
	_, _ = digest.Write([]byte(name))
	_, _ = digest.Write([]byte{0})
	for _, dependency := range dependencies {
		_, _ = digest.Write([]byte(dependency))
		_, _ = digest.Write([]byte{0})
	}
	signature := fmt.Sprintf("sha256:%x", digest.Sum(nil))
	visiting[identity] = false
	memo[identity] = signature
	return signature
}

func graphIdentityVertex(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return !strings.HasPrefix(name, "exporting ") && !strings.HasPrefix(name, "importing to ") &&
		!strings.HasPrefix(name, "importing cache manifest from ")
}

func matchingRemoteDirection(value string, scopes []CacheScope) (string, bool) {
	direction := ""
	found := false
	for _, scope := range scopes {
		if scope.Reference == "" || !strings.Contains(value, scope.Reference) {
			continue
		}
		found = true
		if direction == "" {
			direction = scope.Direction
		} else if direction != scope.Direction {
			return "", true
		}
	}
	return direction, found
}

func (accumulator *progressAccumulator) err() error {
	return errors.Join(accumulator.warnings...)
}

// progressStream forwards Buildx's native stderr while parsing raw JSON one
// line at a time. It caps the pending line so a noisy build cannot retain its
// complete progress history in the Layer Cache process.
type progressStream struct {
	mu           sync.Mutex
	destination  io.Writer
	accumulator  *progressAccumulator
	remoteScopes []CacheScope
	pending      []byte
	discardLine  bool
	finished     bool
}

func newProgressStream(destination io.Writer, remoteReferences []string) *progressStream {
	scopes := make([]CacheScope, 0, len(remoteReferences))
	for _, reference := range remoteReferences {
		scopes = append(scopes, CacheScope{Reference: reference})
	}
	return newProgressStreamWithScopes(destination, scopes)
}

func newProgressStreamWithScopes(destination io.Writer, remoteScopes []CacheScope) *progressStream {
	return &progressStream{
		destination:  writerOrDiscard(destination),
		accumulator:  newProgressAccumulator(),
		remoteScopes: append([]CacheScope(nil), remoteScopes...),
		pending:      make([]byte, 0, 64*1024),
	}
}

func (stream *progressStream) Write(data []byte) (int, error) {
	written, err := stream.destination.Write(data)
	if written == 0 {
		return written, err
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if !stream.finished {
		stream.consume(data[:written])
	}
	return written, err
}

func (stream *progressStream) consume(data []byte) {
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		chunk := data
		complete := false
		if newline >= 0 {
			chunk = data[:newline]
			complete = true
		}
		if !stream.discardLine {
			if len(stream.pending)+len(chunk) > maxProgressLineBytes {
				stream.accumulator.warn(fmt.Errorf("BuildKit progress line exceeded %d bytes", maxProgressLineBytes))
				stream.pending = stream.pending[:0]
				stream.discardLine = true
			} else {
				stream.pending = append(stream.pending, chunk...)
			}
		}
		if complete {
			if !stream.discardLine {
				stream.accumulator.addLine(stream.pending)
			}
			stream.pending = stream.pending[:0]
			stream.discardLine = false
			data = data[newline+1:]
			continue
		}
		break
	}
}

func (stream *progressStream) finish() (ProgressMetrics, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if !stream.finished {
		if !stream.discardLine && len(stream.pending) > 0 {
			stream.accumulator.addLine(stream.pending)
		}
		stream.pending = nil
		stream.finished = true
	}
	return stream.accumulator.metricsForScopes(stream.remoteScopes), stream.accumulator.err()
}

func ParseProgress(reader io.Reader) (ProgressMetrics, error) {
	stream := newProgressStream(io.Discard, nil)
	_, copyErr := io.Copy(stream, reader)
	metrics, progressErr := stream.finish()
	return metrics, errors.Join(copyErr, progressErr)
}
