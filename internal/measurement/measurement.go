package measurement

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/retention"
)

const SchemaVersion = "2"

var (
	ErrRunNotFound         = errors.New("measurement run not found")
	ErrActionsMissNotFound = errors.New("Actions miss outcome not found")
)

type Result string

const (
	ResultHit     Result = "hit"
	ResultMiss    Result = "miss"
	ResultUnknown Result = "unknown"
)

type Integration string

const (
	IntegrationTurbo    Integration = "turbo"
	IntegrationActions  Integration = "actions"
	IntegrationBuildkit Integration = "buildkit"
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
	WorkspaceID       string
	Integration       Integration
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
	// EligibleUnits and HitUnits let one BuildKit vertex group retain its
	// native cached-vertex ratio. Zero values mean one binary unit, preserving
	// the task/archive model used by Turbo and Actions.
	EligibleUnits int
	HitUnits      int
}

// ActionsMissCompletion contains the bounded measurements observed when a
// cache save completes after an Actions lookup miss. It carries no cache key,
// path, environment value, credential, or artifact contents.
type ActionsMissCompletion struct {
	RunID             string
	WorkID            string
	FinishedAt        time.Time
	ExecutionDuration time.Duration
	UploadDuration    time.Duration
	UploadedBytes     int64
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
	WorkspaceID         string       `json:"workspaceId,omitempty"`
	Integration         Integration  `json:"integration,omitempty"`
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
	EligibleUnits       int          `json:"eligibleUnits"`
	HitUnits            int          `json:"hitUnits"`
}

type SourceReport struct {
	Source     Source `json:"source"`
	Hits       int    `json:"hits"`
	Downloaded int64  `json:"downloadedBytes"`
	Uploaded   int64  `json:"uploadedBytes"`
}

type IntegrationReport struct {
	Integration Integration  `json:"integration"`
	Groups      int          `json:"groups"`
	Eligible    int          `json:"eligible"`
	Hits        int          `json:"hits"`
	Misses      int          `json:"misses"`
	HitRate     float64      `json:"hitRate"`
	Timing      TimingReport `json:"timing"`
	Bytes       Bytes        `json:"bytes"`
	Degraded    bool         `json:"degraded"`
}

type RunReport struct {
	SchemaVersion              string              `json:"schemaVersion"`
	RunID                      string              `json:"runId"`
	StartedAt                  time.Time           `json:"startedAt"`
	FinishedAt                 time.Time           `json:"finishedAt"`
	Outcomes                   []OutcomeReport     `json:"outcomes"`
	Groups                     int                 `json:"groups"`
	Eligible                   int                 `json:"eligible"`
	Hits                       int                 `json:"hits"`
	Misses                     int                 `json:"misses"`
	HitRate                    float64             `json:"hitRate"`
	GrossAvoidedTaskTime       Estimate            `json:"grossAvoidedTaskTime"`
	NetEstimatedBuildTimeSaved Estimate            `json:"netEstimatedBuildTimeSaved"`
	Timing                     TimingReport        `json:"timing"`
	Bytes                      Bytes               `json:"bytes"`
	Sources                    []SourceReport      `json:"sources"`
	Integrations               []IntegrationReport `json:"integrations,omitempty"`
	Degraded                   bool                `json:"degraded"`
}

type PeriodReport struct {
	SchemaVersion              string              `json:"schemaVersion"`
	From                       time.Time           `json:"from"`
	To                         time.Time           `json:"to"`
	Runs                       int                 `json:"runs"`
	Groups                     int                 `json:"groups"`
	Eligible                   int                 `json:"eligible"`
	Hits                       int                 `json:"hits"`
	Misses                     int                 `json:"misses"`
	HitRate                    float64             `json:"hitRate"`
	GrossAvoidedTaskTime       Estimate            `json:"grossAvoidedTaskTime"`
	NetEstimatedBuildTimeSaved Estimate            `json:"netEstimatedBuildTimeSaved"`
	Timing                     TimingReport        `json:"timing"`
	Bytes                      Bytes               `json:"bytes"`
	Sources                    []SourceReport      `json:"sources"`
	Integrations               []IntegrationReport `json:"integrations,omitempty"`
	Degraded                   bool                `json:"degraded"`
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
	Retention      time.Duration
	HitSource      Source
	MaxBytes       int64
	EvictionPolicy retention.Policy
}

type BacktestReport struct {
	SchemaVersion        string           `json:"schemaVersion"`
	Method               string           `json:"method"`
	RetentionMS          int64            `json:"retentionMs"`
	MaxBytes             int64            `json:"maxBytes"`
	EvictionPolicy       retention.Policy `json:"evictionPolicy"`
	ImpactEvictions      int              `json:"impactEvictions"`
	LRUFallbackEvictions int              `json:"lruFallbackEvictions"`
	TotalWork            int              `json:"totalWork"`
	KnownFingerprints    int              `json:"knownFingerprints"`
	FingerprintCoverage  float64          `json:"fingerprintCoverage"`
	Runs                 []RunReport      `json:"runs"`
	Period               PeriodReport     `json:"period"`
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

// EnrichActionsMiss completes one recorded Actions miss without creating a
// second outcome for the later save.
func (recorder *Recorder) EnrichActionsMiss(completion ActionsMissCompletion) error {
	if err := validateActionsMissCompletion(completion); err != nil {
		return err
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	work := recorder.runs[completion.RunID]
	outcome, found := work[completion.WorkID]
	if !found || outcome.Integration != IntegrationActions || outcome.Result != ResultMiss {
		return actionsMissNotFound(completion)
	}
	if err := validateActionsMissCompletionOrder(outcome, completion); err != nil {
		return err
	}
	executionDuration := completion.ExecutionDuration
	outcome.FinishedAt = completion.FinishedAt
	outcome.ExecutionDuration = &executionDuration
	outcome.Timing.Upload = completion.UploadDuration
	outcome.Bytes.Uploaded = completion.UploadedBytes
	work[completion.WorkID] = outcome
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
	validatedPolicy, err := retention.Normalize(policy.EvictionPolicy)
	if err != nil {
		return BacktestReport{}, err
	}
	policy.EvictionPolicy = validatedPolicy
	if policy.Retention < 0 {
		return BacktestReport{}, errors.New("backtest retention cannot be negative")
	}
	if policy.MaxBytes < 0 {
		return BacktestReport{}, errors.New("backtest cache quota cannot be negative")
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
		SchemaVersion:  SchemaVersion,
		Method:         "causalReplay",
		RetentionMS:    policy.Retention.Milliseconds(),
		MaxBytes:       policy.MaxBytes,
		EvictionPolicy: policy.EvictionPolicy,
		Runs:           make([]RunReport, 0, len(runs)),
	}
	if len(runs) == 0 {
		result.Period = aggregatePeriod(time.Time{}, time.Time{}, nil)
		return result, nil
	}

	cache := make(map[historicalKey]historicalArtifact)
	pending := make([]pendingHistoricalArtifact, 0)
	for _, historicalRun := range runs {
		var decisions replayEvictions
		cache, pending, decisions = activateHistoricalArtifacts(cache, pending, historicalRun.StartedAt, policy)
		result.ImpactEvictions += decisions.impact
		result.LRUFallbackEvictions += decisions.fallback
		outcomes := make([]FinalOutcome, 0, len(historicalRun.Work))
		for _, work := range historicalRun.Work {
			result.TotalWork++
			if work.ArtifactID == "" || work.CompatibilityID == "" {
				outcomes = append(outcomes, FinalOutcome{
					RunID: historicalRun.RunID, WorkID: work.WorkID,
					Result: ResultUnknown, Source: SourceUnattributed,
					StartedAt: historicalRun.StartedAt, FinishedAt: historicalRun.FinishedAt,
				})
				continue
			}
			result.KnownFingerprints++
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
				artifact.AccessCount = min(artifact.AccessCount+1, retention.MaxAccessCount)
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
						CreatedAt:        historicalRun.FinishedAt,
						Size:             work.ArtifactBytes,
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
	if result.TotalWork > 0 {
		result.FingerprintCoverage = float64(result.KnownFingerprints) / float64(result.TotalWork)
	}
	result.Period = aggregatePeriod(runs[0].StartedAt, latestHistoryFinish(runs), result.Runs)
	return result, nil
}

type historicalArtifact struct {
	ProducerDuration *time.Duration
	LastAccess       time.Time
	Size             int64
	CreatedAt        time.Time
	AccessCount      int64
}

type replayEvictions struct {
	impact   int
	fallback int
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

func activateHistoricalArtifacts(
	cache map[historicalKey]historicalArtifact,
	pending []pendingHistoricalArtifact,
	now time.Time,
	policy BacktestPolicy,
) (map[historicalKey]historicalArtifact, []pendingHistoricalArtifact, replayEvictions) {
	var decisions replayEvictions
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].AvailableAt.Before(pending[j].AvailableAt) })
	remaining := pending[:0]
	for _, candidate := range pending {
		if candidate.AvailableAt.After(now) {
			remaining = append(remaining, candidate)
			continue
		}
		existing, found := cache[candidate.Key]
		if found && !expired(existing.LastAccess, candidate.AvailableAt, policy.Retention) {
			continue
		}
		for key, artifact := range cache {
			if expired(artifact.LastAccess, candidate.AvailableAt, policy.Retention) {
				delete(cache, key)
			}
		}
		if policy.MaxBytes > 0 {
			if candidate.Artifact.Size > policy.MaxBytes {
				continue
			}
			for historicalCacheBytes(cache) > policy.MaxBytes-candidate.Artifact.Size {
				oldest, found, effective := selectHistoricalVictim(cache, policy.EvictionPolicy, candidate.AvailableAt)
				if !found {
					break
				}
				delete(cache, oldest)
				if policy.EvictionPolicy == retention.Impact {
					if effective == retention.Impact {
						decisions.impact++
					} else {
						decisions.fallback++
					}
				}
			}
		}
		cache[candidate.Key] = candidate.Artifact
	}
	return cache, remaining, decisions
}

func selectHistoricalVictim(cache map[historicalKey]historicalArtifact, policy retention.Policy, now time.Time) (historicalKey, bool, retention.Policy) {
	if policy != retention.Impact {
		key, found := oldestHistoricalArtifact(cache)
		return key, found, retention.LRU
	}
	keys := make([]historicalKey, 0, len(cache))
	for key := range cache {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].ArtifactID < keys[j].ArtifactID || keys[i].ArtifactID == keys[j].ArtifactID && keys[i].CompatibilityID < keys[j].CompatibilityID
	})
	candidates := make([]retention.Candidate, 0, len(keys))
	for _, key := range keys {
		artifact := cache[key]
		candidate := retention.Candidate{Bytes: artifact.Size, LastAccess: artifact.LastAccess, CreatedAt: artifact.CreatedAt, AccessCount: artifact.AccessCount}
		if artifact.ProducerDuration != nil {
			candidate.ProducerDurationMS = artifact.ProducerDuration.Milliseconds()
			candidate.DurationKnown = true
		}
		candidates = append(candidates, candidate)
	}
	index, effective := retention.Choose(policy, candidates, now)
	if effective == retention.LRU {
		// Preserve the historical LRU contract, including its key ordering
		// for equal access timestamps, when timing coverage forces fallback.
		key, found := oldestHistoricalArtifact(cache)
		return key, found, effective
	}
	if index < 0 {
		return historicalKey{}, false, effective
	}
	return keys[index], true, effective
}

func historicalCacheBytes(cache map[historicalKey]historicalArtifact) int64 {
	var total int64
	for _, artifact := range cache {
		if artifact.Size > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += artifact.Size
	}
	return total
}

func oldestHistoricalArtifact(cache map[historicalKey]historicalArtifact) (historicalKey, bool) {
	var oldest historicalKey
	var oldestArtifact historicalArtifact
	found := false
	for key, artifact := range cache {
		if !found || artifact.LastAccess.Before(oldestArtifact.LastAccess) ||
			artifact.LastAccess.Equal(oldestArtifact.LastAccess) &&
				(key.ArtifactID < oldest.ArtifactID || key.ArtifactID == oldest.ArtifactID && key.CompatibilityID < oldest.CompatibilityID) {
			oldest, oldestArtifact, found = key, artifact, true
		}
	}
	return oldest, found
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
			if work.WorkID == "" {
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
		Groups:        len(outcomes),
	}
	sources := make(map[Source]*SourceReport)
	integrations := make(map[Integration]*IntegrationReport)
	var grossMS int64
	knownGross := 0
	knownNet := 0
	baselineWeights := make(map[string]int64, len(outcomes))
	observedWeights := make(map[string]int64, len(outcomes))

	for index, outcome := range outcomes {
		eligibleUnits, hitUnits := outcomeUnits(outcome)
		report.Eligible += eligibleUnits
		report.Hits += hitUnits
		report.Misses += eligibleUnits - hitUnits
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
		if outcome.Integration != "" {
			integration := integrations[outcome.Integration]
			if integration == nil {
				integration = &IntegrationReport{Integration: outcome.Integration}
				integrations[outcome.Integration] = integration
			}
			integration.Groups++
			integration.Eligible += eligibleUnits
			integration.Hits += hitUnits
			integration.Misses += eligibleUnits - hitUnits
			integration.Timing = addTiming(integration.Timing, outcomeReport.Timing)
			integration.Bytes.Downloaded += outcome.Bytes.Downloaded
			integration.Bytes.Uploaded += outcome.Bytes.Uploaded
			integration.Degraded = integration.Degraded || outcome.Degraded
		}

		observed := timingMilliseconds(outcome.Timing)
		switch outcome.Result {
		case ResultHit:
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
			source.Hits += hitUnits
			source.Downloaded += outcome.Bytes.Downloaded
			source.Uploaded += outcome.Bytes.Uploaded
		case ResultMiss:
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
	hitGroups := 0
	for _, outcome := range outcomes {
		if outcome.Result == ResultHit {
			hitGroups++
		}
	}
	report.GrossAvoidedTaskTime = completeEstimate(grossMS, "producerDuration", knownGross, hitGroups, ConfidenceHigh)
	if err := validateGraph(outcomes); err != nil {
		return RunReport{}, err
	}
	report.NetEstimatedBuildTimeSaved = Estimate{
		Method:     "criticalPath",
		Confidence: ConfidenceUnknown,
		Known:      knownNet,
		Total:      report.Groups,
	}
	if knownNet == report.Groups {
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
	for _, integration := range integrations {
		if integration.Eligible > 0 {
			integration.HitRate = float64(integration.Hits) / float64(integration.Eligible)
		}
		report.Integrations = append(report.Integrations, *integration)
	}
	sort.Slice(report.Integrations, func(i, j int) bool {
		return report.Integrations[i].Integration < report.Integrations[j].Integration
	})
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
	integrations := make(map[Integration]*IntegrationReport)
	var grossMS int64
	var netMS int64
	knownGross := 0
	totalGross := 0
	knownNet := 0
	totalNet := 0
	for _, report := range reports {
		period.Groups += report.Groups
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
		for _, integrationReport := range report.Integrations {
			integration := integrations[integrationReport.Integration]
			if integration == nil {
				integration = &IntegrationReport{Integration: integrationReport.Integration}
				integrations[integrationReport.Integration] = integration
			}
			integration.Groups += integrationReport.Groups
			integration.Eligible += integrationReport.Eligible
			integration.Hits += integrationReport.Hits
			integration.Misses += integrationReport.Misses
			integration.Timing = addTiming(integration.Timing, integrationReport.Timing)
			integration.Bytes.Downloaded += integrationReport.Bytes.Downloaded
			integration.Bytes.Uploaded += integrationReport.Bytes.Uploaded
			integration.Degraded = integration.Degraded || integrationReport.Degraded
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
	for _, integration := range integrations {
		if integration.Eligible > 0 {
			integration.HitRate = float64(integration.Hits) / float64(integration.Eligible)
		}
		period.Integrations = append(period.Integrations, *integration)
	}
	sort.Slice(period.Integrations, func(i, j int) bool {
		return period.Integrations[i].Integration < period.Integrations[j].Integration
	})
	return period
}

func reportOutcome(outcome FinalOutcome) OutcomeReport {
	eligibleUnits, hitUnits := outcomeUnits(outcome)
	return OutcomeReport{
		WorkID:              outcome.WorkID,
		WorkspaceID:         outcome.WorkspaceID,
		Integration:         outcome.Integration,
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
		EligibleUnits:       eligibleUnits,
		HitUnits:            hitUnits,
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
	if outcome.Integration != "" && !validIntegration(outcome.Integration) {
		return fmt.Errorf("unsupported measurement integration %q", outcome.Integration)
	}
	if outcome.StartedAt.IsZero() || outcome.FinishedAt.IsZero() || outcome.FinishedAt.Before(outcome.StartedAt) {
		return errors.New("measurement outcome needs an ordered start and finish")
	}
	if outcome.Result != ResultHit && outcome.Result != ResultMiss && outcome.Result != ResultUnknown {
		return fmt.Errorf("unsupported final outcome %q", outcome.Result)
	}
	if outcome.Result == ResultHit && outcome.Source == SourceNone {
		return errors.New("cache hit source is required")
	}
	if outcome.Result == ResultMiss && outcome.Source != SourceNone {
		return errors.New("cache miss source must be none")
	}
	if outcome.Result == ResultUnknown && outcome.Source != SourceUnattributed {
		return errors.New("unknown cache result source must be unattributed")
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
	if outcome.EligibleUnits < 0 || outcome.HitUnits < 0 {
		return errors.New("measurement unit counts cannot be negative")
	}
	if outcome.EligibleUnits == 0 && outcome.HitUnits != 0 {
		return errors.New("measurement hit units require eligible units")
	}
	if outcome.EligibleUnits > 0 {
		if outcome.HitUnits > outcome.EligibleUnits {
			return errors.New("measurement hit units cannot exceed eligible units")
		}
		if outcome.Result == ResultHit && outcome.HitUnits == 0 {
			return errors.New("cache hit group needs at least one hit unit")
		}
		if outcome.Result == ResultMiss && outcome.HitUnits != 0 {
			return errors.New("cache miss group cannot contain hit units")
		}
	}
	return nil
}

func validateActionsMissCompletion(completion ActionsMissCompletion) error {
	if err := validateBoundedText("Actions miss completion run ID", completion.RunID, maximumRunIDBytes, false); err != nil {
		return err
	}
	if err := validateBoundedText("Actions miss completion work ID", completion.WorkID, maximumIdentityBytes, false); err != nil {
		return err
	}
	if completion.FinishedAt.IsZero() {
		return errors.New("Actions miss completion finish is required")
	}
	if completion.ExecutionDuration < 0 || completion.UploadDuration < 0 {
		return errors.New("Actions miss completion durations cannot be negative")
	}
	if completion.UploadedBytes < 0 {
		return errors.New("Actions miss completion bytes cannot be negative")
	}
	return nil
}

func validateActionsMissCompletionOrder(outcome FinalOutcome, completion ActionsMissCompletion) error {
	if completion.FinishedAt.Before(outcome.FinishedAt) {
		return errors.New("Actions miss completion finish cannot precede the lookup finish")
	}
	elapsed := completion.FinishedAt.Sub(outcome.FinishedAt)
	if completion.ExecutionDuration > elapsed || completion.UploadDuration > elapsed-completion.ExecutionDuration {
		return errors.New("Actions miss completion durations exceed the observed completion interval")
	}
	return nil
}

func actionsMissNotFound(completion ActionsMissCompletion) error {
	return fmt.Errorf("%w: run %q work %q", ErrActionsMissNotFound, completion.RunID, completion.WorkID)
}

func validIntegration(integration Integration) bool {
	switch integration {
	case IntegrationTurbo, IntegrationActions, IntegrationBuildkit:
		return true
	default:
		return false
	}
}

func outcomeUnits(outcome FinalOutcome) (int, int) {
	if outcome.Result == ResultUnknown {
		return 0, 0
	}
	if outcome.EligibleUnits > 0 {
		return outcome.EligibleUnits, outcome.HitUnits
	}
	if outcome.Result == ResultHit {
		return 1, 1
	}
	return 1, 0
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
