package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/retention"
)

const maxBacktestInputBytes = 32 << 20

type backtestInput struct {
	SchemaVersion string             `json:"schemaVersion"`
	Runs          []backtestInputRun `json:"runs"`
}

type backtestInputRun struct {
	RunID      string              `json:"runId"`
	StartedAt  time.Time           `json:"startedAt"`
	FinishedAt time.Time           `json:"finishedAt"`
	Work       []backtestInputWork `json:"work"`
}

type backtestInputWork struct {
	WorkID              string              `json:"workId"`
	Dependencies        []string            `json:"dependencies,omitempty"`
	ArtifactID          string              `json:"artifactId"`
	CompatibilityID     string              `json:"compatibilityId"`
	ExecutionDurationMS *int64              `json:"executionDurationMs"`
	ArtifactBytes       int64               `json:"artifactBytes"`
	HitTiming           backtestInputTiming `json:"hitTiming"`
	MissTiming          backtestInputTiming `json:"missTiming"`
}

type backtestInputTiming struct {
	LookupMS       int64 `json:"lookupMs"`
	DownloadMS     int64 `json:"downloadMs"`
	VerificationMS int64 `json:"verificationMs"`
	RestoreMS      int64 `json:"restoreMs"`
	UploadMS       int64 `json:"uploadMs"`
}

func runBacktest(_ context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("backtest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inputPath := flags.String("input", "", "historical run JSON file")
	retentionWindow := flags.Duration("retention", 0, "cache retention duration")
	maxBytes := flags.Int64("max-bytes", 0, "cache byte quota (zero means unbounded)")
	evictionPolicy := flags.String("eviction-policy", "lru", "cache eviction policy: lru or impact")
	sourceValue := flags.String("source", "", "hit source: localCache, teamCache, publicCache, or unattributed")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("backtest does not accept positional arguments")
	}
	seen := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) { seen[value.Name] = true })
	if *inputPath == "" || !seen["retention"] || !seen["source"] {
		return errors.New("--input, --retention, and --source are required")
	}
	source, err := parseBacktestSource(*sourceValue)
	if err != nil {
		return err
	}
	history, err := readBacktestInput(*inputPath)
	if err != nil {
		return err
	}
	report, err := measurement.Backtest(history, measurement.BacktestPolicy{
		Retention: *retentionWindow, HitSource: source, MaxBytes: *maxBytes, EvictionPolicy: retention.Policy(*evictionPolicy),
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(report)
	}
	_, err = fmt.Fprintf(stdout, "Layer Cache backtest: %d/%d hits (%.0f%%), net estimate %s\n",
		report.Period.Hits, report.Period.Eligible, report.Period.HitRate*100,
		formatEstimate(report.Period.NetEstimatedBuildTimeSaved))
	return err
}

func parseBacktestSource(value string) (measurement.Source, error) {
	source := measurement.Source(value)
	switch source {
	case measurement.SourceLocalCache, measurement.SourceTeamCache,
		measurement.SourcePublicCache, measurement.SourceUnattributed:
		return source, nil
	default:
		return "", errors.New("--source must be localCache, teamCache, publicCache, or unattributed")
	}
}

func readBacktestInput(path string) ([]measurement.HistoricalRun, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open backtest input: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxBacktestInputBytes+1))
	decoder.DisallowUnknownFields()
	var input backtestInput
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("decode backtest input: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode backtest input: multiple JSON values")
		}
		return nil, fmt.Errorf("decode backtest input: %w", err)
	}
	if input.SchemaVersion != "1" && input.SchemaVersion != measurement.SchemaVersion {
		return nil, fmt.Errorf("unsupported backtest input schemaVersion %q", input.SchemaVersion)
	}
	history := make([]measurement.HistoricalRun, len(input.Runs))
	for runIndex, inputRun := range input.Runs {
		run := measurement.HistoricalRun{
			RunID: inputRun.RunID, StartedAt: inputRun.StartedAt, FinishedAt: inputRun.FinishedAt,
			Work: make([]measurement.HistoricalWork, len(inputRun.Work)),
		}
		for workIndex, inputWork := range inputRun.Work {
			execution, err := millisecondsPointer(inputWork.ExecutionDurationMS)
			if err != nil {
				return nil, fmt.Errorf("run %q work %q execution duration: %w", inputRun.RunID, inputWork.WorkID, err)
			}
			hitTiming, err := inputWork.HitTiming.domain()
			if err != nil {
				return nil, fmt.Errorf("run %q work %q hit timing: %w", inputRun.RunID, inputWork.WorkID, err)
			}
			missTiming, err := inputWork.MissTiming.domain()
			if err != nil {
				return nil, fmt.Errorf("run %q work %q miss timing: %w", inputRun.RunID, inputWork.WorkID, err)
			}
			run.Work[workIndex] = measurement.HistoricalWork{
				WorkID: inputWork.WorkID, Dependencies: append([]string(nil), inputWork.Dependencies...),
				ArtifactID: inputWork.ArtifactID, CompatibilityID: inputWork.CompatibilityID,
				ExecutionDuration: execution, ArtifactBytes: inputWork.ArtifactBytes,
				HitTiming: hitTiming, MissTiming: missTiming,
			}
		}
		history[runIndex] = run
	}
	return history, nil
}

func (input backtestInputTiming) domain() (measurement.Timing, error) {
	values := []int64{input.LookupMS, input.DownloadMS, input.VerificationMS, input.RestoreMS, input.UploadMS}
	for _, value := range values {
		if value < 0 || value > math.MaxInt64/int64(time.Millisecond) {
			return measurement.Timing{}, errors.New("milliseconds must be non-negative and fit a duration")
		}
	}
	return measurement.Timing{
		Lookup:       time.Duration(input.LookupMS) * time.Millisecond,
		Download:     time.Duration(input.DownloadMS) * time.Millisecond,
		Verification: time.Duration(input.VerificationMS) * time.Millisecond,
		Restore:      time.Duration(input.RestoreMS) * time.Millisecond,
		Upload:       time.Duration(input.UploadMS) * time.Millisecond,
	}, nil
}

func millisecondsPointer(milliseconds *int64) (*time.Duration, error) {
	if milliseconds == nil {
		return nil, nil
	}
	if *milliseconds < 0 || *milliseconds > math.MaxInt64/int64(time.Millisecond) {
		return nil, errors.New("milliseconds must be non-negative and fit a duration")
	}
	value := time.Duration(*milliseconds) * time.Millisecond
	return &value, nil
}
