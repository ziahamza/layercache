package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/measurement"
)

// Reconciliation replaces one complete job's Turbo graph. Only signed OIDC job
// authority selects its identity; client JSON cannot select a run or project.
func (server *Server) reconcileTurboReport(writer http.ResponseWriter, request *http.Request) {
	claims, ok := access.ClaimsFromContext(request.Context())
	checkPrefix := "github-actions-check:" + claims.Repository + ":"
	checkID := strings.TrimPrefix(claims.WorkspaceID, checkPrefix)
	if !ok || claims.Integration != "turbo" || claims.Compatibility == "" || claims.Repository == "" || !strings.HasPrefix(claims.Subject, "github-actions:") || !strings.HasPrefix(claims.WorkspaceID, checkPrefix) || checkID == "" || !strings.HasPrefix(claims.RunID, "github-actions:"+claims.Repository+":") || !strings.HasSuffix(claims.RunID, ":check:"+checkID) {
		server.writeAuthorizationFailure(writer, request)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<20)
	defer request.Body.Close()
	var input struct {
		Summaries []json.RawMessage `json:"summaries"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || rejectTrailingJSON(decoder) != nil || len(input.Summaries) == 0 || len(input.Summaries) > 32 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid bounded Turbo summaries"})
		return
	}
	documents := make([][]byte, len(input.Summaries))
	tasks := 0
	for index, summary := range input.Summaries {
		var bounds struct {
			Execution struct {
				StartTime int64 `json:"startTime"`
				EndTime   int64 `json:"endTime"`
			} `json:"execution"`
			Tasks []struct {
				Execution struct {
					StartTime int64 `json:"startTime"`
					EndTime   int64 `json:"endTime"`
				} `json:"execution"`
			} `json:"tasks"`
		}
		if json.Unmarshal(summary, &bounds) != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid summary bounds"})
			return
		}
		tasks += len(bounds.Tasks)
		if tasks > 4096 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "too many summary tasks"})
			return
		}
		for _, task := range bounds.Tasks {
			// Unexecuted tasks may have zero timestamps. Any execution that is
			// reported must fit the bounded summary window, not an arbitrary epoch.
			if task.Execution.StartTime != 0 || task.Execution.EndTime != 0 {
				if task.Execution.StartTime < bounds.Execution.StartTime-5000 || task.Execution.EndTime > bounds.Execution.EndTime+5000 {
					writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "task falls outside summary window"})
					return
				}
			}
		}
		documents[index] = summary
	}
	repository, ok := server.measurements.(interface {
		ReconcileTurboSummaries(string, measurement.TurboReconcileOptions, [][]byte) (int, error)
	})
	if !ok {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "Turbo reporting unavailable"})
		return
	}
	now := time.Now().UTC()
	count, err := repository.ReconcileTurboSummaries(claims.RunID, measurement.TurboReconcileOptions{Project: claims.Project, WorkspaceID: claims.WorkspaceID, CompatibilityID: claims.Compatibility, RunStartedAt: now.Add(-24 * time.Hour), RunFinishedAt: now}, documents)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "Turbo summaries could not be reconciled"})
		return
	}
	if count == 0 {
		writeJSON(writer, http.StatusOK, map[string]any{"runId": claims.RunID, "tasks": 0, "report": nil})
		return
	}
	report, err := server.measurements.RunReport(claims.RunID)
	if err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "Turbo report unavailable"})
		return
	}
	// No task paths, hashes, graph or artifact keys need to leave the server.
	writeJSON(writer, http.StatusOK, map[string]any{"runId": claims.RunID, "tasks": count, "report": map[string]any{
		"eligible": report.Eligible, "hits": report.Hits, "misses": report.Misses, "hitRate": report.HitRate,
		"grossAvoidedTaskTime": report.GrossAvoidedTaskTime, "netEstimatedBuildTimeSaved": report.NetEstimatedBuildTimeSaved,
		"timing": report.Timing, "bytes": report.Bytes, "sources": report.Sources, "degraded": report.Degraded,
	}})
}
