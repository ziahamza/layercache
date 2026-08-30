package buildkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type ProgressMetrics struct {
	Vertices          int     `json:"vertices"`
	CompletedVertices int     `json:"completedVertices"`
	CachedVertices    int     `json:"cachedVertices"`
	CacheHitRate      float64 `json:"cacheHitRate"`
}

type progressVertex struct {
	ID        string          `json:"id"`
	Digest    string          `json:"digest"`
	Completed json.RawMessage `json:"completed"`
	Cached    bool            `json:"cached"`
}

type progressEvent struct {
	progressVertex
	Vertexes []progressVertex `json:"vertexes"`
}

type vertexState struct {
	completed bool
	cached    bool
}

func ParseProgress(reader io.Reader) (ProgressMetrics, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	vertices := make(map[string]vertexState)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var event progressEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return ProgressMetrics{}, fmt.Errorf("decode BuildKit progress line %d: %w", line, err)
		}
		if len(event.Vertexes) == 0 {
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
			state := vertices[identity]
			if len(vertex.Completed) > 0 && string(vertex.Completed) != "null" {
				state.completed = true
			}
			state.cached = state.cached || vertex.Cached
			vertices[identity] = state
		}
	}
	if err := scanner.Err(); err != nil {
		return ProgressMetrics{}, fmt.Errorf("read BuildKit progress: %w", err)
	}

	metrics := ProgressMetrics{Vertices: len(vertices)}
	for _, state := range vertices {
		if state.completed {
			metrics.CompletedVertices++
		}
		if state.cached {
			metrics.CachedVertices++
		}
	}
	if metrics.CompletedVertices > 0 {
		metrics.CacheHitRate = float64(metrics.CachedVertices) / float64(metrics.CompletedVertices)
	}
	return metrics, nil
}
