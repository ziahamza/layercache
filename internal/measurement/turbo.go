package measurement

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// TurboReconcileOptions binds a summary to one Layer Cache namespace and the
// command window that was allowed to produce it.
type TurboReconcileOptions struct {
	Project         string
	WorkspaceID     string
	CompatibilityID string
	RunStartedAt    time.Time
	RunFinishedAt   time.Time
}

// TurboObservation is bounded transport evidence from the Turbo cache
// gateway. It cannot express task identity, dependencies, cache eligibility,
// or execution duration; those facts come only from a completed Run Summary.
type TurboObservation struct {
	RunID      string
	ArtifactID string
	Result     Result
	Source     Source
	StartedAt  time.Time
	FinishedAt time.Time
	Timing     Timing
	Bytes      Bytes
	Degraded   bool
}

// ObserveTurbo records transport evidence from the Turbo cache gateway. An
// observation is deliberately not a final outcome: one Turbo task can make
// several cache probes before its Run Summary declares the eventual result.
func (repository *SQLiteRepository) ObserveTurbo(observation TurboObservation) error {
	if err := validateTurboObservation(observation); err != nil {
		return err
	}
	if observation.ArtifactID == "" {
		return errors.New("Turbo observation artifact ID is required")
	}
	startedAt, err := observation.StartedAt.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode Turbo observation start: %w", err)
	}
	finishedAt, err := observation.FinishedAt.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode Turbo observation finish: %w", err)
	}
	_, err = repository.database.Exec(`
		INSERT INTO measurement_turbo_observations_v1 (
			run_id, artifact_id, result, source, started_at, finished_at,
			lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
			downloaded_bytes, uploaded_bytes, degraded
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		observation.RunID,
		observation.ArtifactID,
		observation.Result,
		observation.Source,
		startedAt,
		finishedAt,
		int64(observation.Timing.Lookup),
		int64(observation.Timing.Download),
		int64(observation.Timing.Verification),
		int64(observation.Timing.Restore),
		int64(observation.Timing.Upload),
		observation.Bytes.Downloaded,
		observation.Bytes.Uploaded,
		observation.Degraded,
	)
	if err != nil {
		return fmt.Errorf("record Turbo transport observation: %w", err)
	}
	return nil
}

func validateTurboObservation(observation TurboObservation) error {
	return validateOutcome(FinalOutcome{
		RunID: observation.RunID, WorkID: observation.ArtifactID, ArtifactID: observation.ArtifactID,
		Result: observation.Result, Source: observation.Source,
		StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt,
		Timing: observation.Timing, Bytes: observation.Bytes, Degraded: observation.Degraded,
	})
}

// ReconcileTurboSummaries atomically replaces provisional cache traffic for a
// Layer Cache run with one final outcome per successful, cache-eligible Turbo
// task. The completed Run Summary is authoritative for task membership, graph,
// hit or miss result, and task timing. Gateway observations only contribute
// source, byte, and transport-overhead evidence.
func (repository *SQLiteRepository) ReconcileTurboSummaries(
	runID string,
	options TurboReconcileOptions,
	documents [][]byte,
) (int, error) {
	summaries, err := parseTurboSummaries(runID, options, documents)
	if err != nil {
		return 0, err
	}

	const reconciliationTimeout = 5 * time.Second
	deadline := time.Now().Add(reconciliationTimeout)
	for {
		count, err := repository.reconcileParsedTurboSummaries(runID, options, summaries)
		if err == nil || !sqliteLockContention(err) || !time.Now().Before(deadline) {
			return count, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func parseTurboSummaries(
	runID string,
	options TurboReconcileOptions,
	documents [][]byte,
) ([]turboRunSummary, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("measurement run ID is required")
	}
	if strings.TrimSpace(options.Project) == "" {
		return nil, errors.New("Turbo reconciliation project is required")
	}
	if strings.TrimSpace(options.CompatibilityID) == "" {
		return nil, errors.New("Turbo reconciliation compatibility is required")
	}
	if len(documents) == 0 {
		return nil, errors.New("at least one Turbo Run Summary is required")
	}

	summaries := make([]turboRunSummary, 0, len(documents))
	seenSummaryIDs := make(map[string]struct{}, len(documents))
	for index, document := range documents {
		summary, err := decodeTurboRunSummary(document)
		if err != nil {
			return nil, fmt.Errorf("decode Turbo Run Summary %d: %w", index+1, err)
		}
		if _, exists := seenSummaryIDs[summary.ID]; exists {
			return nil, fmt.Errorf("Turbo Run Summary ID %q appears more than once", summary.ID)
		}
		if err := summary.withinRun(options.RunStartedAt, options.RunFinishedAt); err != nil {
			return nil, err
		}
		seenSummaryIDs[summary.ID] = struct{}{}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Execution.StartTime == summaries[j].Execution.StartTime {
			return summaries[i].ID < summaries[j].ID
		}
		return summaries[i].Execution.StartTime < summaries[j].Execution.StartTime
	})
	return summaries, nil
}

func (repository *SQLiteRepository) reconcileParsedTurboSummaries(
	runID string,
	options TurboReconcileOptions,
	summaries []turboRunSummary,
) (int, error) {
	transaction, err := repository.database.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin Turbo measurement reconciliation: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	observations, err := loadTurboObservations(transaction, runID)
	if err != nil {
		return 0, err
	}
	outcomes, err := reconcileTurboOutcomes(runID, options, summaries, observations)
	if err != nil {
		return 0, err
	}
	if _, err := transaction.Exec(
		`DELETE FROM measurement_final_outcomes_v1 WHERE run_id = ? AND integration IN ('', 'turbo')`, runID,
	); err != nil {
		return 0, fmt.Errorf("clear provisional Turbo outcomes: %w", err)
	}
	for _, outcome := range outcomes {
		if err := insertFinalOutcome(transaction, outcome, false); err != nil {
			return 0, err
		}
	}
	if _, err := transaction.Exec(`DELETE FROM measurement_turbo_observations_v1 WHERE run_id = ?`, runID); err != nil {
		return 0, fmt.Errorf("clear reconciled Turbo observations: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, fmt.Errorf("commit Turbo measurement reconciliation: %w", err)
	}
	return len(outcomes), nil
}

type sqliteErrorCoder interface {
	Code() int
}

func sqliteLockContention(err error) bool {
	var coded sqliteErrorCoder
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY or SQLITE_LOCKED, including extended codes.
			return true
		}
	}
	return false
}

func (summary turboRunSummary) withinRun(startedAt, finishedAt time.Time) error {
	if startedAt.IsZero() && finishedAt.IsZero() {
		return nil
	}
	if startedAt.IsZero() || finishedAt.IsZero() || finishedAt.Before(startedAt) {
		return errors.New("Turbo reconciliation needs ordered run timestamps")
	}
	const clockTolerance = 5 * time.Second
	summaryStart := time.UnixMilli(summary.Execution.StartTime)
	summaryFinish := time.UnixMilli(summary.Execution.EndTime)
	if summaryStart.Before(startedAt.Add(-clockTolerance)) || summaryFinish.After(finishedAt.Add(clockTolerance)) {
		return fmt.Errorf("Turbo Run Summary %q falls outside the Layer Cache run", summary.ID)
	}
	return nil
}

// TurboArtifactIdentity produces the exact opaque artifact coordinate shared
// by the gateway observation and Run Summary reconciliation paths.
func TurboArtifactIdentity(project, compatibilityID, turboHash string) string {
	digest := sha256.Sum256([]byte("turbo\x00" + project + "\x00" + compatibilityID + "\x00" + turboHash + "\x00\x00"))
	return fmt.Sprintf("sha256:%x", digest[:])
}

type turboRunSummary struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Execution struct {
		StartTime int64 `json:"startTime"`
		EndTime   int64 `json:"endTime"`
	} `json:"execution"`
	Tasks []turboSummaryTask `json:"tasks"`
}

type turboSummaryTask struct {
	TaskID       string   `json:"taskId"`
	Hash         string   `json:"hash"`
	Dependencies []string `json:"dependencies"`
	Cache        struct {
		Local     bool        `json:"local"`
		Remote    bool        `json:"remote"`
		Status    string      `json:"status"`
		Source    string      `json:"source"`
		TimeSaved json.Number `json:"timeSaved"`
	} `json:"cache"`
	Execution struct {
		StartTime int64 `json:"startTime"`
		EndTime   int64 `json:"endTime"`
		ExitCode  *int  `json:"exitCode"`
	} `json:"execution"`
	ResolvedTaskDefinition struct {
		Cache *bool `json:"cache"`
	} `json:"resolvedTaskDefinition"`
}

func decodeTurboRunSummary(document []byte) (turboRunSummary, error) {
	var summary turboRunSummary
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&summary); err != nil {
		return turboRunSummary{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return turboRunSummary{}, err
	}
	if summary.ID == "" {
		return turboRunSummary{}, errors.New("summary ID is required")
	}
	if summary.Version != "1" {
		return turboRunSummary{}, fmt.Errorf("unsupported summary schema version %q", summary.Version)
	}
	if summary.Execution.StartTime <= 0 || summary.Execution.EndTime < summary.Execution.StartTime {
		return turboRunSummary{}, errors.New("summary has invalid execution timestamps")
	}
	seenTasks := make(map[string]struct{}, len(summary.Tasks))
	for _, task := range summary.Tasks {
		if task.TaskID == "" || task.Hash == "" {
			return turboRunSummary{}, errors.New("summary task identity and hash are required")
		}
		if _, exists := seenTasks[task.TaskID]; exists {
			return turboRunSummary{}, fmt.Errorf("summary repeats task %q", task.TaskID)
		}
		seenTasks[task.TaskID] = struct{}{}
		if task.Execution.StartTime < 0 || task.Execution.EndTime < task.Execution.StartTime {
			return turboRunSummary{}, fmt.Errorf("summary task %q has invalid execution timestamps", task.TaskID)
		}
		for _, dependency := range task.Dependencies {
			if dependency == task.TaskID {
				return turboRunSummary{}, fmt.Errorf("summary task %q depends on itself", task.TaskID)
			}
		}
	}
	for _, task := range summary.Tasks {
		for _, dependency := range task.Dependencies {
			if _, exists := seenTasks[dependency]; !exists {
				return turboRunSummary{}, fmt.Errorf("summary task %q names unknown dependency %q", task.TaskID, dependency)
			}
		}
	}
	return summary, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("summary contains more than one JSON value")
	}
	return err
}

type turboObservation struct {
	FinalOutcome
	used bool
}

func loadTurboObservations(transaction *sql.Tx, runID string) ([]turboObservation, error) {
	rows, err := transaction.Query(`
		SELECT
			artifact_id, result, source, started_at, finished_at,
			lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
			downloaded_bytes, uploaded_bytes, degraded
		FROM measurement_turbo_observations_v1
		WHERE run_id = ?
		ORDER BY sequence`, runID)
	if err != nil {
		return nil, fmt.Errorf("load Turbo transport observations: %w", err)
	}
	observations := make([]turboObservation, 0)
	for rows.Next() {
		observation, err := scanTurboObservation(rows, runID)
		if err != nil {
			rows.Close()
			return nil, err
		}
		observations = append(observations, turboObservation{FinalOutcome: observation})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("load Turbo transport observations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close Turbo transport observations: %w", err)
	}

	if len(observations) > 0 {
		return observations, nil
	}

	// Releases before the observation table existed wrote cache traffic into
	// the final table. Use those rows as migration evidence only when no
	// dedicated observations exist; the same transaction replaces them with
	// summary-derived outcomes below.
	legacyRows, err := transaction.Query(
		selectOutcomes+` WHERE run_id = ? AND integration IN ('', 'turbo') ORDER BY started_at, work_id`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("load legacy Turbo observations: %w", err)
	}
	for legacyRows.Next() {
		outcome, err := scanFinalOutcome(legacyRows)
		if err != nil {
			legacyRows.Close()
			return nil, err
		}
		observations = append(observations, turboObservation{FinalOutcome: outcome})
	}
	if err := legacyRows.Err(); err != nil {
		legacyRows.Close()
		return nil, fmt.Errorf("load legacy Turbo observations: %w", err)
	}
	if err := legacyRows.Close(); err != nil {
		return nil, fmt.Errorf("close legacy Turbo observations: %w", err)
	}
	sort.SliceStable(observations, func(i, j int) bool {
		return observations[i].StartedAt.Before(observations[j].StartedAt)
	})
	return observations, nil
}

type observationScanner interface {
	Scan(destinations ...any) error
}

func scanTurboObservation(row observationScanner, runID string) (FinalOutcome, error) {
	var outcome FinalOutcome
	var startedAt []byte
	var finishedAt []byte
	var lookup int64
	var download int64
	var verification int64
	var restore int64
	var upload int64
	var degraded bool
	if err := row.Scan(
		&outcome.ArtifactID,
		&outcome.Result,
		&outcome.Source,
		&startedAt,
		&finishedAt,
		&lookup,
		&download,
		&verification,
		&restore,
		&upload,
		&outcome.Bytes.Downloaded,
		&outcome.Bytes.Uploaded,
		&degraded,
	); err != nil {
		return FinalOutcome{}, fmt.Errorf("decode Turbo transport observation: %w", err)
	}
	outcome.RunID = runID
	if err := outcome.StartedAt.UnmarshalBinary(startedAt); err != nil {
		return FinalOutcome{}, fmt.Errorf("decode Turbo observation start: %w", err)
	}
	if err := outcome.FinishedAt.UnmarshalBinary(finishedAt); err != nil {
		return FinalOutcome{}, fmt.Errorf("decode Turbo observation finish: %w", err)
	}
	outcome.Timing = Timing{
		Lookup:       time.Duration(lookup),
		Download:     time.Duration(download),
		Verification: time.Duration(verification),
		Restore:      time.Duration(restore),
		Upload:       time.Duration(upload),
	}
	outcome.Degraded = degraded
	return outcome, nil
}

func reconcileTurboOutcomes(
	runID string,
	options TurboReconcileOptions,
	summaries []turboRunSummary,
	observations []turboObservation,
) ([]FinalOutcome, error) {
	outcomes := make([]FinalOutcome, 0)
	for _, summary := range summaries {
		eligible := make(map[string]bool, len(summary.Tasks))
		tasks := make(map[string]turboSummaryTask, len(summary.Tasks))
		workIDs := make(map[string]string, len(summary.Tasks))
		for _, task := range summary.Tasks {
			tasks[task.TaskID] = task
			if turboTaskEligible(task) {
				eligible[task.TaskID] = true
				workIDs[task.TaskID] = privateTurboIdentity(
					"task", options.Project, options.CompatibilityID, summary.ID, task.TaskID, task.Hash,
				)
			}
		}
		sort.SliceStable(summary.Tasks, func(i, j int) bool {
			if summary.Tasks[i].Execution.StartTime == summary.Tasks[j].Execution.StartTime {
				return summary.Tasks[i].TaskID < summary.Tasks[j].TaskID
			}
			return summary.Tasks[i].Execution.StartTime < summary.Tasks[j].Execution.StartTime
		})
		for _, task := range summary.Tasks {
			if !eligible[task.TaskID] {
				continue
			}
			outcome, err := reconcileTurboTask(runID, options, summary.ID, task, observations)
			if err != nil {
				return nil, err
			}
			outcome.WorkID = workIDs[task.TaskID]
			dependencyIDs := eligibleTurboDependencies(task.TaskID, tasks, eligible)
			for _, dependency := range dependencyIDs {
				outcome.Dependencies = append(outcome.Dependencies, workIDs[dependency])
			}
			sort.Strings(outcome.Dependencies)
			outcomes = append(outcomes, outcome)
		}
	}
	if _, err := buildRunReport(runID, append([]FinalOutcome(nil), outcomes...)); err != nil && len(outcomes) > 0 {
		return nil, fmt.Errorf("validate reconciled Turbo graph: %w", err)
	}
	return outcomes, nil
}

func turboTaskEligible(task turboSummaryTask) bool {
	if task.ResolvedTaskDefinition.Cache == nil || !*task.ResolvedTaskDefinition.Cache {
		return false
	}
	if task.Execution.ExitCode == nil || *task.Execution.ExitCode != 0 {
		return false
	}
	if task.Execution.StartTime <= 0 || task.Execution.EndTime < task.Execution.StartTime {
		return false
	}
	status := strings.ToUpper(task.Cache.Status)
	return status == "HIT" || status == "MISS"
}

func reconcileTurboTask(
	runID string,
	options TurboReconcileOptions,
	summaryID string,
	task turboSummaryTask,
	observations []turboObservation,
) (FinalOutcome, error) {
	startedAt := time.UnixMilli(task.Execution.StartTime).UTC()
	finishedAt := time.UnixMilli(task.Execution.EndTime).UTC()
	artifactID := TurboArtifactIdentity(options.Project, options.CompatibilityID, task.Hash)
	outcome := FinalOutcome{
		RunID:           runID,
		WorkspaceID:     options.WorkspaceID,
		Integration:     IntegrationTurbo,
		WorkID:          privateTurboIdentity("task", options.Project, options.CompatibilityID, summaryID, task.TaskID, task.Hash),
		ArtifactID:      artifactID,
		CompatibilityID: options.CompatibilityID,
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
	}

	if strings.EqualFold(task.Cache.Status, "MISS") {
		outcome.Result = ResultMiss
		outcome.Source = SourceNone
		duration := finishedAt.Sub(startedAt)
		outcome.ExecutionDuration = &duration
		if observation := combinedTurboMissObservation(observations, artifactID, startedAt, finishedAt); observation != nil {
			outcome.Timing = observation.Timing
			outcome.Bytes = observation.Bytes
			outcome.Degraded = observation.Degraded
		}
		return outcome, validateOutcome(outcome)
	}

	// Turborepo's Workspace-local cache can satisfy a task before the Layer
	// Cache gateway receives any request. Preserve that final task in the run
	// graph, but do not credit its hit or time saving to Layer Cache.
	if task.Cache.Local || strings.EqualFold(task.Cache.Source, "LOCAL") {
		outcome.Result = ResultUnknown
		outcome.Source = SourceUnattributed
		outcome.Timing.Restore = finishedAt.Sub(startedAt)
		return outcome, validateOutcome(outcome)
	}

	outcome.Result = ResultHit
	producerDuration, err := positiveTurboMilliseconds(task.Cache.TimeSaved)
	if err != nil {
		return FinalOutcome{}, fmt.Errorf("summary task %q has invalid timeSaved: %w", task.TaskID, err)
	}
	outcome.ProducerDuration = producerDuration
	taskDuration := finishedAt.Sub(startedAt)

	observation := closestTurboObservation(observations, artifactID, ResultHit, startedAt, finishedAt)
	if observation == nil {
		outcome.Source = SourceUnattributed
		outcome.Timing.Restore = taskDuration
		return outcome, validateOutcome(outcome)
	}
	outcome.Source = observation.Source
	if outcome.Source == SourceNone || !validSource(outcome.Source) {
		outcome.Source = SourceUnattributed
	}
	outcome.Timing = observation.Timing
	outcome.Bytes = observation.Bytes
	outcome.Degraded = observation.Degraded
	if observed := totalTiming(outcome.Timing); observed < taskDuration {
		outcome.Timing.Restore += taskDuration - observed
	}
	return outcome, validateOutcome(outcome)
}

func closestTurboObservation(
	observations []turboObservation,
	artifactID string,
	result Result,
	startedAt time.Time,
	finishedAt time.Time,
) *FinalOutcome {
	return closestTurboObservationWhere(
		observations, artifactID, result, startedAt, finishedAt, func(FinalOutcome) bool { return true },
	)
}

func closestTurboObservationWhere(
	observations []turboObservation,
	artifactID string,
	result Result,
	startedAt time.Time,
	finishedAt time.Time,
	accept func(FinalOutcome) bool,
) *FinalOutcome {
	best := -1
	var bestDistance time.Duration
	target := startedAt.Add(finishedAt.Sub(startedAt) / 2)
	for index := range observations {
		observation := &observations[index]
		if observation.used || observation.ArtifactID != artifactID || observation.Result != result ||
			!accept(observation.FinalOutcome) {
			continue
		}
		midpoint := observation.StartedAt.Add(observation.FinishedAt.Sub(observation.StartedAt) / 2)
		distance := midpoint.Sub(target)
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < bestDistance {
			best = index
			bestDistance = distance
		}
	}
	if best < 0 {
		return nil
	}
	observations[best].used = true
	return &observations[best].FinalOutcome
}

func combinedTurboMissObservation(
	observations []turboObservation,
	artifactID string,
	startedAt time.Time,
	finishedAt time.Time,
) *FinalOutcome {
	lookup := closestTurboObservationWhere(
		observations, artifactID, ResultMiss, startedAt, finishedAt,
		func(outcome FinalOutcome) bool {
			return outcome.Timing.Upload == 0 && outcome.Bytes.Uploaded == 0
		},
	)
	upload := closestTurboObservationWhere(
		observations, artifactID, ResultMiss, startedAt, finishedAt,
		func(outcome FinalOutcome) bool {
			return outcome.Timing.Upload > 0 || outcome.Bytes.Uploaded > 0
		},
	)
	if lookup == nil && upload == nil {
		return closestTurboObservation(observations, artifactID, ResultMiss, startedAt, finishedAt)
	}
	if lookup == nil {
		combined := *upload
		return &combined
	}
	combined := *lookup
	if upload == nil {
		return &combined
	}
	if upload.StartedAt.Before(combined.StartedAt) {
		combined.StartedAt = upload.StartedAt
	}
	if upload.FinishedAt.After(combined.FinishedAt) {
		combined.FinishedAt = upload.FinishedAt
	}
	combined.Timing.Lookup = max(combined.Timing.Lookup, upload.Timing.Lookup)
	combined.Timing.Download = max(combined.Timing.Download, upload.Timing.Download)
	combined.Timing.Verification = max(combined.Timing.Verification, upload.Timing.Verification)
	combined.Timing.Restore = max(combined.Timing.Restore, upload.Timing.Restore)
	combined.Timing.Upload = max(combined.Timing.Upload, upload.Timing.Upload)
	combined.Bytes.Downloaded = max(combined.Bytes.Downloaded, upload.Bytes.Downloaded)
	combined.Bytes.Uploaded = max(combined.Bytes.Uploaded, upload.Bytes.Uploaded)
	combined.Degraded = combined.Degraded || upload.Degraded
	return &combined
}

func eligibleTurboDependencies(taskID string, tasks map[string]turboSummaryTask, eligible map[string]bool) []string {
	result := make(map[string]struct{})
	visiting := make(map[string]bool)
	var visit func(string)
	visit = func(candidate string) {
		if visiting[candidate] {
			return
		}
		if eligible[candidate] {
			result[candidate] = struct{}{}
			return
		}
		visiting[candidate] = true
		for _, dependency := range tasks[candidate].Dependencies {
			visit(dependency)
		}
		delete(visiting, candidate)
	}
	for _, dependency := range tasks[taskID].Dependencies {
		visit(dependency)
	}
	dependencies := make([]string, 0, len(result))
	for dependency := range result {
		dependencies = append(dependencies, dependency)
	}
	sort.Strings(dependencies)
	return dependencies
}

func positiveTurboMilliseconds(number json.Number) (*time.Duration, error) {
	if number == "" {
		return nil, nil
	}
	milliseconds, err := number.Int64()
	if err != nil {
		return nil, err
	}
	if milliseconds <= 0 {
		return nil, nil
	}
	if milliseconds > int64((1<<63-1)/time.Millisecond) {
		return nil, errors.New("duration overflows")
	}
	duration := time.Duration(milliseconds) * time.Millisecond
	return &duration, nil
}

func privateTurboIdentity(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		_, _ = digest.Write([]byte(part))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil))
}

func totalTiming(timing Timing) time.Duration {
	return timing.Lookup + timing.Download + timing.Verification + timing.Restore + timing.Upload
}
