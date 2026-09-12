package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

var runIDPattern = regexp.MustCompile(`(?m)^Layer Cache run: (run-\S+)$`)

func measureSource(ctx context.Context, root string, opts options, source string, iterations int) (result sourceResult, returnErr error) {
	result.Source = source
	directory := filepath.Join(root, source)
	template := filepath.Join(directory, "template")
	if err := createTemplate(ctx, template); err != nil {
		return result, err
	}
	var processes []*daemon
	defer func() {
		for index := len(processes) - 1; index >= 0; index-- {
			returnErr = errors.Join(returnErr, processes[index].close())
		}
	}()
	var team *daemon
	var err error
	if source == "team" {
		team, err = startDaemon(ctx, opts.binary, directory, template, "team", "team", nil)
		if err != nil {
			return result, err
		}
		processes = append(processes, team)
	}
	coldHost, err := startDaemon(ctx, opts.binary, directory, template, "producer", "local", team)
	if err != nil {
		return result, err
	}
	processes = append(processes, coldHost)
	warmHost := coldHost
	if source == "team" {
		warmHost, err = startDaemon(ctx, opts.binary, directory, template, "consumer", "local", team)
		if err != nil {
			return result, err
		}
		processes = append(processes, warmHost)
	}
	var coldDurations, warmDurations []float64
	for index := 0; index < opts.samples; index++ {
		pair, err := measurePair(ctx, opts, directory, template, coldHost, warmHost, source, index, iterations)
		result.Samples = append(result.Samples, pair)
		if err != nil {
			return result, fmt.Errorf("%s sample %d: %w", source, index+1, err)
		}
		coldDurations = append(coldDurations, pair.Cold.DurationMS)
		warmDurations = append(warmDurations, pair.Warm.DurationMS)
		fmt.Fprintf(os.Stderr, "%s %d/%d: cold %.1fms, warm %.1fms, verified %s hit\n", source, index+1, opts.samples, pair.Cold.DurationMS, pair.Warm.DurationMS, source)
	}
	result.summary, err = summarize(coldDurations, warmDurations)
	if err != nil {
		return result, err
	}
	fmt.Fprintf(os.Stderr, "%s medians: cold %.1fms, warm %.1fms, ratio %.3f\n", source, result.ColdMedianMS, result.WarmMedianMS, result.WarmColdRatio)
	return result, nil
}

func measurePair(ctx context.Context, opts options, directory, template string, coldHost, warmHost *daemon, source string, index, iterations int) (sample, error) {
	pair := sample{Index: index + 1}
	coldWorkspace := filepath.Join(directory, fmt.Sprintf("cold-%02d", index))
	warmWorkspace := filepath.Join(directory, fmt.Sprintf("warm-%02d", index))
	seed := fmt.Sprintf("%s-%02d", source, index)
	for _, workspace := range []string{coldWorkspace, warmWorkspace} {
		if err := cloneWorkspace(ctx, template, workspace, seed, iterations); err != nil {
			return pair, err
		}
		// Both clones must start without outputs or native caches. Cloning the
		// tracked template makes deleting caller-owned directories unnecessary.
		for _, relative := range []string{".turbo", "packages/app/.turbo", "packages/app/dist"} {
			if _, err := os.Stat(filepath.Join(workspace, relative)); !errors.Is(err, os.ErrNotExist) {
				return pair, fmt.Errorf("fresh Workspace unexpectedly contains %s", relative)
			}
		}
	}
	var err error
	pair.Cold, err = measureRun(ctx, opts, coldWorkspace, coldHost, "")
	if err != nil {
		return pair, err
	}
	pair.ArtifactSHA256, err = digestFile(filepath.Join(coldWorkspace, "packages/app/dist/result.txt"))
	if err != nil {
		return pair, err
	}
	pair.Warm, err = measureRun(ctx, opts, warmWorkspace, warmHost, source)
	if err != nil {
		return pair, err
	}
	warmDigest, err := digestFile(filepath.Join(warmWorkspace, "packages/app/dist/result.txt"))
	if err != nil {
		return pair, err
	}
	if warmDigest != pair.ArtifactSHA256 {
		return pair, errors.New("restored output differs from the cold build")
	}
	return pair, nil
}

func measureRun(ctx context.Context, opts options, workspace string, host *daemon, source string) (result runEvidence, returnErr error) {
	started := time.Now()
	result.Stdout, result.Stderr, returnErr = execute(ctx, workspace, opts.binary,
		"run", "--config", host.configPath, "--", opts.turbo, "run", "build", "--cache=remote:rw", "--summarize")
	result.DurationMS = float64(time.Since(started)) / float64(time.Millisecond)
	if returnErr != nil {
		return result, returnErr
	}
	match := runIDPattern.FindStringSubmatch(result.Stderr)
	if len(match) != 2 {
		return result, errors.New("installed command omitted the Layer Cache run identity")
	}
	reportJSON, _, err := execute(ctx, workspace, opts.binary, "report", "--config", host.configPath, "--run", match[1], "--json")
	if err != nil {
		return result, err
	}
	result.Report = json.RawMessage(reportJSON)
	var report measurement.RunReport
	if err := json.Unmarshal(result.Report, &report); err != nil {
		return result, err
	}
	if report.Eligible != 1 || len(report.Outcomes) != 1 || report.Degraded {
		return result, fmt.Errorf("expected one non-degraded eligible Turbo outcome, got eligible=%d outcomes=%d degraded=%t", report.Eligible, len(report.Outcomes), report.Degraded)
	}
	outcome := report.Outcomes[0]
	if source == "" {
		if report.Misses != 1 || report.Hits != 0 || outcome.ExecutionDurationMS == nil || *outcome.ExecutionDurationMS < 1000 || report.Bytes.Uploaded == 0 {
			return result, errors.New("cold build did not record a cache miss, at least one second of real execution, and uploaded bytes")
		}
	} else {
		wanted := measurement.SourceLocalCache
		if source == "team" {
			wanted = measurement.SourceTeamCache
		}
		if report.Hits != 1 || report.Misses != 0 || outcome.Source != wanted || report.Bytes.Downloaded == 0 || outcome.ProducerDurationMS == nil {
			return result, fmt.Errorf("warm build did not prove one %s hit with downloaded bytes and producer duration", source)
		}
	}
	paths, err := filepath.Glob(filepath.Join(workspace, ".turbo", "runs", "*.json"))
	if err != nil || len(paths) != 1 {
		return result, errors.New("real Turbo did not emit exactly one Run Summary")
	}
	result.TurboSummary, err = os.ReadFile(paths[0])
	if err != nil {
		return result, err
	}
	var native struct {
		Tasks []struct {
			Cache struct {
				Status string `json:"status"`
				Source string `json:"source"`
			} `json:"cache"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(result.TurboSummary, &native); err != nil || len(native.Tasks) != 1 {
		return result, errors.New("real Turbo summary does not contain the fixture task")
	}
	cache := native.Tasks[0].Cache
	if source == "" && !strings.EqualFold(cache.Status, "MISS") || source != "" && (!strings.EqualFold(cache.Status, "HIT") || !strings.EqualFold(cache.Source, "REMOTE")) {
		return result, fmt.Errorf("unexpected native Turbo cache result: status=%s source=%s", cache.Status, cache.Source)
	}
	return result, nil
}
