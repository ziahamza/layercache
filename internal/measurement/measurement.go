package measurement

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const SchemaVersion = "1"

var ErrRunNotFound = errors.New("measurement run not found")

type Result string

const (
	ResultHit  Result = "hit"
	ResultMiss Result = "miss"
)

type Source string

const (
	SourceNone         Source = "none"
	SourceLocalCache   Source = "localCache"
	SourceTeamCache    Source = "teamCache"
	SourcePublicCache  Source = "publicCache"
	SourceUnattributed Source = "unattributed"
)

type Confidence string

const (
	ConfidenceUnknown Confidence = "unknown"
	ConfidenceLow     Confidence = "low"
	ConfidenceMedium  Confidence = "medium"
	ConfidenceHigh    Confidence = "high"
)

type Timing struct {
	Lookup       time.Duration
	Download     time.Duration
	Verification time.Duration
	Restore      time.Duration
	Upload       time.Duration
}

type Bytes struct {
	Downloaded int64 `json:"downloaded"`
	Uploaded   int64 `json:"uploaded"`
}

type FinalOutcome struct {
	RunID             string
	WorkID            string
	Dependencies      []string
	ArtifactID        string
	CompatibilityID   string
	Result            Result
	Source            Source
	StartedAt         time.Time
	FinishedAt        time.Time
	ProducerDuration  *time.Duration
	ExecutionDuration *time.Duration
	Timing            Timing
	Bytes             Bytes
	Degraded          bool
}

type Estimate struct {
	Milliseconds *int64     `json:"milliseconds"`
	Method       string     `json:"method"`
	Confidence   Confidence `json:"confidence"`
	Known        int        `json:"known"`
	Total        int        `json:"total"`
}

type TimingReport struct {
	LookupMS       int64 `json:"lookupMs"`
	DownloadMS     int64 `json:"downloadMs"`
	VerificationMS int64 `json:"verificationMs"`
	RestoreMS      int64 `json:"restoreMs"`
	UploadMS       int64 `json:"uploadMs"`
}

type OutcomeReport struct {
	WorkID              string       `json:"workId"`
	Dependencies        []string     `json:"dependencies,omitempty"`
	ArtifactID          string       `json:"artifactId,omitempty"`
	CompatibilityID     string       `json:"compatibilityId,omitempty"`
	Result              Result       `json:"result"`
	Source              Source       `json:"source"`
	StartedAt           time.Time    `json:"startedAt"`
	FinishedAt          time.Time    `json:"finishedAt"`
	ProducerDurationMS  *int64       `json:"producerDurationMs"`
	ExecutionDurationMS *int64       `json:"executionDurationMs"`
	Timing              TimingReport `json:"timing"`
	Bytes               Bytes        `json:"bytes"`
	Degraded            bool         `json:"degraded"`
}

type SourceReport struct {
	Source     Source `json:"source"`
	Hits       int    `json:"hits"`
	Downloaded int64  `json:"downloadedBytes"`
	Uploaded   int64  `json:"uploadedBytes"`
}

type RunReport struct {
	SchemaVersion              string          `json:"schemaVersion"`
	RunID                      string          `json:"runId"`
	StartedAt                  time.Time       `json:"startedAt"`
	FinishedAt                 time.Time       `json:"finishedAt"`
	Outcomes                   []OutcomeReport `json:"outcomes"`
	Eligible                   int             `json:"eligible"`
	Hits                       int             `json:"hits"`
	Misses                     int             `json:"misses"`
	HitRate                    float64         `json:"hitRate"`
	GrossAvoidedTaskTime       Estimate        `json:"grossAvoidedTaskTime"`
	NetEstimatedBuildTimeSaved Estimate        `json:"netEstimatedBuildTimeSaved"`
	Timing                     TimingReport    `json:"timing"`
	Bytes                      Bytes           `json:"bytes"`
	Sources                    []SourceReport  `json:"sources"`
	Degraded                   bool            `json:"degraded"`
}

type PeriodReport struct {
	SchemaVersion              string         `json:"schemaVersion"`
	From                       time.Time      `json:"from"`
	To                         time.Time      `json:"to"`
	Runs                       int            `json:"runs"`
	Eligible                   int            `json:"eligible"`
	Hits                       int            `json:"hits"`
	Misses                     int            `json:"misses"`
	HitRate                    float64        `json:"hitRate"`
	GrossAvoidedTaskTime       Estimate       `json:"grossAvoidedTaskTime"`
	NetEstimatedBuildTimeSaved Estimate       `json:"netEstimatedBuildTimeSaved"`
	Timing                     TimingReport   `json:"timing"`
	Bytes                      Bytes          `json:"bytes"`
	Sources                    []SourceReport `json:"sources"`
	Degraded                   bool           `json:"degraded"`
}

type HistoricalWork struct {
	WorkID            string
	Dependencies      []string
	ArtifactID        string
	CompatibilityID   string
	ExecutionDuration *time.Duration
	ArtifactBytes     int64
	HitTiming         Timing
	MissTiming        Timing
}

type HistoricalRun struct {
	RunID      string
	StartedAt  time.Time
	FinishedAt time.Time
	Work       []HistoricalWork
}

type BacktestPolicy struct {
	Retention time.Duration
	HitSource Source
}

type BacktestReport struct {
	SchemaVersion string       `json:"schemaVersion"`
	Method        string       `json:"method"`
	RetentionMS   int64        `json:"retentionMs"`
	Runs          []RunReport  `json:"runs"`
	Period        PeriodReport `json:"period"`
}

type Recorder struct {
	mu   sync.RWMutex
	runs map[string]map[string]FinalOutcome
}

func NewRecorder() *Recorder {
	return &Recorder{runs: make(map[string]map[string]FinalOutcome)}
}

func (recorder *Recorder) Record(outcome FinalOutcome) error {
	if err := validateOutcome(outcome); err != nil {
		return err
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	work := recorder.runs[outcome.RunID]
	if work == nil {
		work = make(map[string]FinalOutcome)
		recorder.runs[outcome.RunID] = work
	}
	if _, exists := work[outcome.WorkID]; exists {
		return fmt.Errorf("run %q already has final outcome %q", outcome.RunID, outcome.WorkID)
	}
	work[outcome.WorkID] = cloneOutcome(outcome)
	return nil
}

func (recorder *Recorder) RunReport(runID string) (RunReport, error) {
	recorder.mu.RLock()
	work, found := recorder.runs[runID]
	if !found {
		recorder.mu.RUnlock()
		return RunReport{}, ErrRunNotFound
	}
	outcomes := make([]FinalOutcome, 0, len(work))
	for _, outcome := range work {
		outcomes = append(outcomes, cloneOutcome(outcome))
	}
	recorder.mu.RUnlock()
	return buildRunReport(runID, outcomes)
}

// PeriodReport includes runs whose start is in the half-open interval [from, to).
func (recorder *Recorder) PeriodReport(from, to time.Time) (PeriodReport, error) {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return PeriodReport{}, errors.New("measurement period needs an ordered start and finish")
	}
	recorder.mu.RLock()
	runs := make(map[string][]FinalOutcome, len(recorder.runs))
	for runID, work := range recorder.runs {
		for _, outcome := range work {
			runs[runID] = append(runs[runID], cloneOutcome(outcome))
		}
	}
	recorder.mu.RUnlock()

	reports := make([]RunReport, 0, len(runs))
	for runID, outcomes := range runs {
		report, err := buildRunReport(runID, outcomes)
		if err != nil {
			return PeriodReport{}, err
		}
		if !report.StartedAt.Before(from) && report.StartedAt.Before(to) {
			reports = append(reports, report)
		}
	}
	return aggregatePeriod(from, to, reports), nil
}

func Backtest(history []HistoricalRun, policy BacktestPolicy) (BacktestReport, error) {
	if policy.Retention < 0 {
		return BacktestReport{}, errors.New("backtest retention cannot be negative")
	}
	if policy.HitSource == "" {
		policy.HitSource = SourceUnattributed
	}
	if policy.HitSource == SourceNone || !validSource(policy.HitSource) {
		return BacktestReport{}, errors.New("backtest hit source must name a cache")
	}
	runs := cloneHistory(history)
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].StartedAt.Before(runs[j].StartedAt)
	})
	if err := validateHistory(runs); err != nil {
		return BacktestReport{}, err
	}
	result := BacktestReport{
		SchemaVersion: SchemaVersion,
		Method:        "causalReplay",
		RetentionMS:   policy.Retention.Milliseconds(),
		Runs:          make([]RunReport, 0, len(runs)),
	}
	if len(runs) == 0 {
		result.Period = aggregatePeriod(time.Time{}, time.Time{}, nil)
		return result, nil
	}

	cache := make(map[historicalKey]historicalArtifact)
	pending := make([]pendingHistoricalArtifact, 0)
	for _, historicalRun := range runs {
		cache, pending = activateHistoricalArtifacts(cache, pending, historicalRun.StartedAt, policy.Retention)
		outcomes := make([]FinalOutcome, 0, len(historicalRun.Work))
		for _, work := range historicalRun.Work {
			key := historicalKey{ArtifactID: work.ArtifactID, CompatibilityID: work.CompatibilityID}
			artifact, hit := cache[key]
			if hit && expired(artifact.LastAccess, historicalRun.StartedAt, policy.Retention) {
				delete(cache, key)
				hit = false
			}
			outcome := FinalOutcome{
				RunID:           historicalRun.RunID,
				WorkID:          work.WorkID,
				Dependencies:    append([]string(nil), work.Dependencies...),
				ArtifactID:      work.ArtifactID,
				CompatibilityID: work.CompatibilityID,
				StartedAt:       historicalRun.StartedAt,
				FinishedAt:      historicalRun.FinishedAt,
			}
			if hit {
				outcome.Result = ResultHit
				outcome.Source = policy.HitSource
				outcome.ProducerDuration = cloneDuration(artifact.ProducerDuration)
				outcome.Timing = work.HitTiming
				outcome.Bytes.Downloaded = work.ArtifactBytes
				artifact.LastAccess = historicalRun.StartedAt
				cache[key] = artifact
			} else {
				outcome.Result = ResultMiss
				outcome.Source = SourceNone
				outcome.ExecutionDuration = cloneDuration(work.ExecutionDuration)
				outcome.Timing = work.MissTiming
				outcome.Bytes.Uploaded = work.ArtifactBytes
				pending = append(pending, pendingHistoricalArtifact{
					Key: key,
					Artifact: historicalArtifact{
						ProducerDuration: cloneDuration(work.ExecutionDuration),
						LastAccess:       historicalRun.FinishedAt,
					},
					AvailableAt: historicalRun.FinishedAt,
				})
			}
			outcomes = append(outcomes, outcome)
		}
		report, err := buildRunReport(historicalRun.RunID, outcomes)
		if err != nil {
			return BacktestReport{}, err
		}
		result.Runs = append(result.Runs, report)
	}
	result.Period = aggregatePeriod(runs[0].StartedAt, latestHistoryFinish(runs), result.Runs)
	return result, nil
}

type historicalArtifact struct {
	ProducerDuration *time.Duration
	LastAccess       time.Time
}

type pendingHistoricalArtifact struct {
	Key         historicalKey
	Artifact    historicalArtifact
	AvailableAt time.Time
}

type historicalKey struct {
	ArtifactID      string
	CompatibilityID string
}

func activateHistoricalArtifacts(cache map[historicalKey]historicalArtifact, pending []pendingHistoricalArtifact, now time.Time, retention time.Duration) (map[historicalKey]historicalArtifact, []pendingHistoricalArtifact) {
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].AvailableAt.Before(pending[j].AvailableAt) })
	remaining := pending[:0]
	for _, candidate := range pending {
		if candidate.AvailableAt.After(now) {
			remaining = append(remaining, candidate)
			continue
		}
		existing, found := cache[candidate.Key]
		if found && !expired(existing.LastAccess, candidate.AvailableAt, retention) {
			continue
		}
		cache[candidate.Key] = candidate.Artifact
	}
	return cache, remaining
}

func expired(lastAccess, now time.Time, retention time.Duration) bool {
	return retention > 0 && !now.Before(lastAccess.Add(retention))
}

func latestHistoryFinish(runs []HistoricalRun) time.Time {
	latest := runs[0].FinishedAt
	for _, run := range runs[1:] {
		if run.FinishedAt.After(latest) {
			latest = run.FinishedAt
		}
	}
	return latest
}

func validateHistory(runs []HistoricalRun) error {
	seenRuns := make(map[string]struct{}, len(runs))
	for _, run := range runs {
		if run.RunID == "" {
			return errors.New("historical run ID is required")
		}
		if _, found := seenRuns[run.RunID]; found {
			return fmt.Errorf("historical run %q appears more than once", run.RunID)
		}
		seenRuns[run.RunID] = struct{}{}
		if run.StartedAt.IsZero() || run.FinishedAt.IsZero() || run.FinishedAt.Before(run.StartedAt) {
			return fmt.Errorf("historical run %q needs an ordered start and finish", run.RunID)
		}
		seenWork := make(map[string]struct{}, len(run.Work))
		for _, work := range run.Work {
			if work.WorkID == "" || work.ArtifactID == "" || work.CompatibilityID == "" {
				return fmt.Errorf("historical run %q has incomplete work identity", run.RunID)
			}
			if _, found := seenWork[work.WorkID]; found {
				return fmt.Errorf("historical run %q repeats work %q", run.RunID, work.WorkID)
			}
			seenWork[work.WorkID] = struct{}{}
			if work.ArtifactBytes < 0 || durationIsNegative(work.ExecutionDuration) || timingIsNegative(work.HitTiming) || timingIsNegative(work.MissTiming) {
				return fmt.Errorf("historical run %q work %q has negative measurements", run.RunID, work.WorkID)
			}
		}
	}
	return nil
}

func cloneHistory(history []HistoricalRun) []HistoricalRun {
	cloned := make([]HistoricalRun, len(history))
	for index, run := range history {
		cloned[index] = run
		cloned[index].Work = make([]HistoricalWork, len(run.Work))
		for workIndex, work := range run.Work {
			cloned[index].Work[workIndex] = work
			cloned[index].Work[workIndex].Dependencies = append([]string(nil), work.Dependencies...)
			cloned[index].Work[workIndex].ExecutionDuration = cloneDuration(work.ExecutionDuration)
		}
	}
	return cloned
}

func buildRunReport(runID string, outcomes []FinalOutcome) (RunReport, error) {
	for index := range outcomes {
		sort.Strings(outcomes[index].Dependencies)
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].WorkID < outcomes[j].WorkID })
	report := RunReport{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		Outcomes:      make([]OutcomeReport, 0, len(outcomes)),
		Sources:       make([]SourceReport, 0),
		Eligible:      len(outcomes),
	}
	sources := make(map[Source]*SourceReport)
	var grossMS int64
	knownGross := 0
	knownNet := 0
	baselineWeights := make(map[string]int64, len(outcomes))
	observedWeights := make(map[string]int64, len(outcomes))

	for index, outcome := range outcomes {
		if index == 0 || outcome.StartedAt.Before(report.StartedAt) {
			report.StartedAt = outcome.StartedAt
		}
		if index == 0 || outcome.FinishedAt.After(report.FinishedAt) {
			report.FinishedAt = outcome.FinishedAt
		}
		outcomeReport := reportOutcome(outcome)
		report.Outcomes = append(report.Outcomes, outcomeReport)
		report.Timing = addTiming(report.Timing, outcomeReport.Timing)
		report.Bytes.Downloaded += outcome.Bytes.Downloaded
		report.Bytes.Uploaded += outcome.Bytes.Uploaded
		report.Degraded = report.Degraded || outcome.Degraded

		observed := timingMilliseconds(outcome.Timing)
		switch outcome.Result {
		case ResultHit:
			report.Hits++
			if outcome.ProducerDuration != nil {
				producer := outcome.ProducerDuration.Milliseconds()
				grossMS += producer
				baselineWeights[outcome.WorkID] = producer
				observedWeights[outcome.WorkID] = observed
				knownGross++
				knownNet++
			}
			source := sources[outcome.Source]
			if source == nil {
				source = &SourceReport{Source: outcome.Source}
				sources[outcome.Source] = source
			}
			source.Hits++
			source.Downloaded += outcome.Bytes.Downloaded
			source.Uploaded += outcome.Bytes.Uploaded
		case ResultMiss:
			report.Misses++
			if outcome.ExecutionDuration != nil {
				execution := outcome.ExecutionDuration.Milliseconds()
				baselineWeights[outcome.WorkID] = execution
				observedWeights[outcome.WorkID] = execution + observed
				knownNet++
			}
		}
	}
	if report.Eligible > 0 {
		report.HitRate = float64(report.Hits) / float64(report.Eligible)
	}
	report.GrossAvoidedTaskTime = completeEstimate(grossMS, "producerDuration", knownGross, report.Hits, ConfidenceHigh)
	if err := validateGraph(outcomes); err != nil {
		return RunReport{}, err
	}
	report.NetEstimatedBuildTimeSaved = Estimate{
		Method:     "criticalPath",
		Confidence: ConfidenceUnknown,
		Known:      knownNet,
		Total:      report.Eligible,
	}
	if knownNet == report.Eligible {
		baselineMS := criticalPath(outcomes, baselineWeights)
		observedMS := criticalPath(outcomes, observedWeights)
		netMS := baselineMS - observedMS
		report.NetEstimatedBuildTimeSaved.Milliseconds = &netMS
		report.NetEstimatedBuildTimeSaved.Confidence = ConfidenceMedium
	}
	for _, source := range sources {
		report.Sources = append(report.Sources, *source)
	}
	sort.Slice(report.Sources, func(i, j int) bool { return report.Sources[i].Source < report.Sources[j].Source })
	return report, nil
}

func validateGraph(outcomes []FinalOutcome) error {
	known := make(map[string]struct{}, len(outcomes))
	dependencies := make(map[string][]string, len(outcomes))
	for _, outcome := range outcomes {
		known[outcome.WorkID] = struct{}{}
		dependencies[outcome.WorkID] = outcome.Dependencies
	}
	for workID, required := range dependencies {
		for _, dependency := range required {
			if _, found := known[dependency]; !found {
				return fmt.Errorf("work %q depends on unknown work %q", workID, dependency)
			}
		}
	}
	visiting := make(map[string]bool, len(outcomes))
	visited := make(map[string]bool, len(outcomes))
	var visit func(string) error
	visit = func(workID string) error {
		if visiting[workID] {
			return fmt.Errorf("measurement graph contains a cycle at %q", workID)
		}
		if visited[workID] {
			return nil
		}
		visiting[workID] = true
		for _, dependency := range dependencies[workID] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[workID] = false
		visited[workID] = true
		return nil
	}
	for workID := range dependencies {
		if err := visit(workID); err != nil {
			return err
		}
	}
	return nil
}

func criticalPath(outcomes []FinalOutcome, weights map[string]int64) int64 {
	dependencies := make(map[string][]string, len(outcomes))
	for _, outcome := range outcomes {
		dependencies[outcome.WorkID] = outcome.Dependencies
	}
	memo := make(map[string]int64, len(outcomes))
	var pathTo func(string) int64
	pathTo = func(workID string) int64 {
		if value, found := memo[workID]; found {
			return value
		}
		var preceding int64
		for _, dependency := range dependencies[workID] {
			if candidate := pathTo(dependency); candidate > preceding {
				preceding = candidate
			}
		}
		value := preceding + weights[workID]
		memo[workID] = value
		return value
	}
	var longest int64
	for workID := range dependencies {
		if candidate := pathTo(workID); candidate > longest {
			longest = candidate
		}
	}
	return longest
}

func completeEstimate(milliseconds int64, method string, known, total int, confidence Confidence) Estimate {
	estimate := Estimate{Method: method, Known: known, Total: total, Confidence: ConfidenceUnknown}
	if known > 0 || total == 0 {
		estimate.Milliseconds = &milliseconds
		if known == total {
			estimate.Confidence = confidence
		} else {
			estimate.Confidence = ConfidenceLow
		}
	}
	return estimate
}

func aggregatePeriod(from, to time.Time, reports []RunReport) PeriodReport {
	period := PeriodReport{
		SchemaVersion: SchemaVersion,
		From:          from,
		To:            to,
		Runs:          len(reports),
		Sources:       make([]SourceReport, 0),
	}
	sources := make(map[Source]*SourceReport)
	var grossMS int64
	var netMS int64
	knownGross := 0
	totalGross := 0
	knownNet := 0
	totalNet := 0
	for _, report := range reports {
		period.Eligible += report.Eligible
		period.Hits += report.Hits
		period.Misses += report.Misses
		period.Timing = addTiming(period.Timing, report.Timing)
		period.Bytes.Downloaded += report.Bytes.Downloaded
		period.Bytes.Uploaded += report.Bytes.Uploaded
		period.Degraded = period.Degraded || report.Degraded
		knownGross += report.GrossAvoidedTaskTime.Known
		totalGross += report.GrossAvoidedTaskTime.Total
		if report.GrossAvoidedTaskTime.Milliseconds != nil {
			grossMS += *report.GrossAvoidedTaskTime.Milliseconds
		}
		totalNet += report.NetEstimatedBuildTimeSaved.Total
		if report.NetEstimatedBuildTimeSaved.Milliseconds != nil {
			netMS += *report.NetEstimatedBuildTimeSaved.Milliseconds
			knownNet += report.NetEstimatedBuildTimeSaved.Total
		}
		for _, sourceReport := range report.Sources {
			source := sources[sourceReport.Source]
			if source == nil {
				source = &SourceReport{Source: sourceReport.Source}
				sources[sourceReport.Source] = source
			}
			source.Hits += sourceReport.Hits
			source.Downloaded += sourceReport.Downloaded
			source.Uploaded += sourceReport.Uploaded
		}
	}
	if period.Eligible > 0 {
		period.HitRate = float64(period.Hits) / float64(period.Eligible)
	}
	period.GrossAvoidedTaskTime = completeEstimate(grossMS, "producerDuration", knownGross, totalGross, ConfidenceHigh)
	period.NetEstimatedBuildTimeSaved = completeEstimate(netMS, "criticalPath", knownNet, totalNet, ConfidenceMedium)
	for _, source := range sources {
		period.Sources = append(period.Sources, *source)
	}
	sort.Slice(period.Sources, func(i, j int) bool { return period.Sources[i].Source < period.Sources[j].Source })
	return period
}

func reportOutcome(outcome FinalOutcome) OutcomeReport {
	return OutcomeReport{
		WorkID:              outcome.WorkID,
		Dependencies:        append([]string(nil), outcome.Dependencies...),
		ArtifactID:          outcome.ArtifactID,
		CompatibilityID:     outcome.CompatibilityID,
		Result:              outcome.Result,
		Source:              outcome.Source,
		StartedAt:           outcome.StartedAt,
		FinishedAt:          outcome.FinishedAt,
		ProducerDurationMS:  durationMilliseconds(outcome.ProducerDuration),
		ExecutionDurationMS: durationMilliseconds(outcome.ExecutionDuration),
		Timing:              reportTiming(outcome.Timing),
		Bytes:               outcome.Bytes,
		Degraded:            outcome.Degraded,
	}
}

func reportTiming(timing Timing) TimingReport {
	return TimingReport{
		LookupMS:       timing.Lookup.Milliseconds(),
		DownloadMS:     timing.Download.Milliseconds(),
		VerificationMS: timing.Verification.Milliseconds(),
		RestoreMS:      timing.Restore.Milliseconds(),
		UploadMS:       timing.Upload.Milliseconds(),
	}
}

func addTiming(left, right TimingReport) TimingReport {
	left.LookupMS += right.LookupMS
	left.DownloadMS += right.DownloadMS
	left.VerificationMS += right.VerificationMS
	left.RestoreMS += right.RestoreMS
	left.UploadMS += right.UploadMS
	return left
}

func timingMilliseconds(timing Timing) int64 {
	total := timing.Lookup + timing.Download + timing.Verification + timing.Restore + timing.Upload
	return total.Milliseconds()
}

func durationMilliseconds(duration *time.Duration) *int64 {
	if duration == nil {
		return nil
	}
	milliseconds := duration.Milliseconds()
	return &milliseconds
}

func cloneDuration(duration *time.Duration) *time.Duration {
	if duration == nil {
		return nil
	}
	value := *duration
	return &value
}

func validateOutcome(outcome FinalOutcome) error {
	if outcome.RunID == "" {
		return errors.New("measurement run ID is required")
	}
	if outcome.WorkID == "" {
		return errors.New("measurement work ID is required")
	}
	if outcome.StartedAt.IsZero() || outcome.FinishedAt.IsZero() || outcome.FinishedAt.Before(outcome.StartedAt) {
		return errors.New("measurement outcome needs an ordered start and finish")
	}
	if outcome.Result != ResultHit && outcome.Result != ResultMiss {
		return fmt.Errorf("unsupported final outcome %q", outcome.Result)
	}
	if outcome.Result == ResultHit && outcome.Source == SourceNone {
		return errors.New("cache hit source is required")
	}
	if outcome.Result == ResultMiss && outcome.Source != SourceNone {
		return errors.New("cache miss source must be none")
	}
	if !validSource(outcome.Source) {
		return fmt.Errorf("unsupported cache source %q", outcome.Source)
	}
	if outcome.Bytes.Downloaded < 0 || outcome.Bytes.Uploaded < 0 {
		return errors.New("measurement bytes cannot be negative")
	}
	if durationIsNegative(outcome.ProducerDuration) || durationIsNegative(outcome.ExecutionDuration) || timingIsNegative(outcome.Timing) {
		return errors.New("measurement durations cannot be negative")
	}
	return nil
}

func validSource(source Source) bool {
	switch source {
	case SourceNone, SourceLocalCache, SourceTeamCache, SourcePublicCache, SourceUnattributed:
		return true
	default:
		return false
	}
}

func durationIsNegative(duration *time.Duration) bool {
	return duration != nil && *duration < 0
}

func timingIsNegative(timing Timing) bool {
	return timing.Lookup < 0 || timing.Download < 0 || timing.Verification < 0 || timing.Restore < 0 || timing.Upload < 0
}

func cloneOutcome(outcome FinalOutcome) FinalOutcome {
	outcome.Dependencies = append([]string(nil), outcome.Dependencies...)
	outcome.ProducerDuration = cloneDuration(outcome.ProducerDuration)
	outcome.ExecutionDuration = cloneDuration(outcome.ExecutionDuration)
	return outcome
}
