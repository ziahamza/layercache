package actionscache

import (
	"fmt"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

func TestActionsMissCorrelationIsBoundedAndExpires(t *testing.T) {
	t.Parallel()

	handler := &Handler{
		missCorrelations: make(map[[32]byte]actionsMissCorrelation),
		saveCorrelations: make(map[int64]actionsSaveCorrelation),
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	scope := Scope{
		Project: "github.com/acme/widgets", Repository: "acme/widgets",
		Ref: "refs/heads/main", Compatibility: "linux-amd64-node24",
		RunID: "run-bounded", WorkspaceID: "workspace-bounded",
	}
	for index := 0; index < maxActionsCorrelations+128; index++ {
		outcome := measurement.FinalOutcome{
			RunID: scope.RunID, WorkID: fmt.Sprintf("work-%d", index), FinishedAt: now,
		}
		handler.rememberActionsMiss(scope, fmt.Sprintf("key-%d", index), "v1", outcome, now)
	}
	if got := len(handler.missCorrelations) + len(handler.saveCorrelations); got != maxActionsCorrelations {
		t.Fatalf("tracked Actions correlations = %d, want %d", got, maxActionsCorrelations)
	}

	handler.measurementMu.Lock()
	handler.pruneActionsCorrelationsLocked(now.Add(actionsCorrelationTTL))
	remaining := len(handler.missCorrelations) + len(handler.saveCorrelations)
	handler.measurementMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired Actions correlations = %d, want 0", remaining)
	}
}

func TestActionsMissCorrelationCarriesOnlyObservedDuration(t *testing.T) {
	t.Parallel()

	var completed measurement.ActionsMissCompletion
	handler := &Handler{
		config: Config{EnrichActionsMiss: func(value measurement.ActionsMissCompletion) error {
			completed = value
			return nil
		}},
		missCorrelations: make(map[[32]byte]actionsMissCorrelation),
		saveCorrelations: make(map[int64]actionsSaveCorrelation),
	}
	lookupFinished := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	scope := Scope{
		Project: "github.com/acme/widgets", Repository: "acme/widgets",
		Ref: "refs/heads/main", Compatibility: "linux-amd64-node24",
		RunID: "run-observed", WorkspaceID: "workspace-observed",
	}
	outcome := measurement.FinalOutcome{
		RunID: scope.RunID, WorkID: "work-observed", FinishedAt: lookupFinished,
	}
	handler.rememberActionsMiss(scope, "deps-key", "v1", outcome, lookupFinished)
	reserveStarted := lookupFinished.Add(7 * time.Second)
	handler.correlateActionsSave(41, scope, "deps-key", "v1", reserveStarted)
	producerDuration := handler.actionsSaveProducerDuration(41, scope, reserveStarted)
	if producerDuration == nil || *producerDuration != 7*time.Second {
		t.Fatalf("correlated producer duration = %v, want 7s", producerDuration)
	}

	wrongRun := scope
	wrongRun.RunID = "run-other"
	if got := handler.actionsSaveProducerDuration(41, wrongRun, reserveStarted); got != nil {
		t.Fatalf("cross-run producer duration = %v, want unknown", got)
	}
	commitFinished := reserveStarted.Add(2 * time.Second)
	handler.completeActionsSave(41, scope, 1234, commitFinished)
	if completed.RunID != scope.RunID || completed.WorkID != outcome.WorkID ||
		completed.ExecutionDuration != 7*time.Second || completed.UploadDuration != 2*time.Second ||
		completed.UploadedBytes != 1234 || !completed.FinishedAt.Equal(commitFinished) {
		t.Fatalf("Actions miss completion = %#v", completed)
	}
	if got := handler.actionsSaveProducerDuration(41, scope, commitFinished); got != nil {
		t.Fatalf("completed correlation remained tracked: %v", got)
	}

	emptyRun := scope
	emptyRun.RunID = ""
	handler.rememberActionsMiss(emptyRun, "untracked", "v1", outcome, lookupFinished)
	if got := len(handler.missCorrelations) + len(handler.saveCorrelations); got != 0 {
		t.Fatalf("correlations without authenticated run ID = %d, want 0", got)
	}
}
