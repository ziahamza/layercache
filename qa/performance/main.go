// Command performance measures an installed Layer Cache binary with real Turbo
// clients. Workload execution and restoration are timed; setup is not.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

type options struct {
	binary, turbo, output string
	sources               []string
	samples               int
	target                time.Duration
}

type evidence struct {
	SchemaVersion string         `json:"schemaVersion"`
	StartedAt     time.Time      `json:"startedAt"`
	FinishedAt    time.Time      `json:"finishedAt"`
	Platform      string         `json:"platform"`
	BinarySHA256  string         `json:"binarySha256"`
	TurboVersion  string         `json:"turboVersion"`
	NodeVersion   string         `json:"nodeVersion"`
	Iterations    int            `json:"pbkdf2Iterations"`
	Sources       []sourceResult `json:"sources"`
	Passed        bool           `json:"passed"`
	Error         string         `json:"error,omitempty"`
}

type sourceResult struct {
	Source  string   `json:"source"`
	Samples []sample `json:"samples"`
	summary
}

type sample struct {
	Index          int         `json:"index"`
	ArtifactSHA256 string      `json:"artifactSha256"`
	Cold           runEvidence `json:"cold"`
	Warm           runEvidence `json:"warm"`
}

type runEvidence struct {
	DurationMS   float64         `json:"durationMs"`
	Stdout       string          `json:"stdout"`
	Stderr       string          `json:"stderr"`
	Report       json.RawMessage `json:"report"`
	TurboSummary json.RawMessage `json:"turboSummary"`
}

func main() {
	var opts options
	var sources string
	flag.StringVar(&opts.binary, "binary", "", "installed Layer Cache executable")
	flag.StringVar(&opts.turbo, "turbo", "", "pinned real Turbo executable")
	flag.StringVar(&opts.output, "output", "", "new JSON evidence file; defaults to stdout")
	flag.StringVar(&sources, "sources", "local,team", "cache sources to measure: local,team")
	flag.IntVar(&opts.samples, "samples", 11, "cold/warm pairs per source, at least 11")
	flag.DurationVar(&opts.target, "target-duration", 2*time.Second, "calibrated CPU work per cold build, at least 2s")
	flag.Parse()
	opts.sources = strings.Split(sources, ",")
	if err := validateOptions(&opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, 20*time.Minute)
	defer timeoutCancel()
	result, err := measure(ctx, opts)
	result.FinishedAt = time.Now().UTC()
	if err != nil {
		result.Error = err.Error()
		result.Passed = false
	}
	document, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, marshalErr)
		os.Exit(1)
	}
	document = append(document, '\n')
	if opts.output == "" {
		_, marshalErr = os.Stdout.Write(document)
	} else {
		var file *os.File
		file, marshalErr = os.OpenFile(opts.output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if marshalErr == nil {
			_, marshalErr = file.Write(document)
			marshalErr = errors.Join(marshalErr, file.Close())
		}
	}
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, "write performance evidence:", marshalErr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func validateOptions(opts *options) error {
	if opts.binary == "" || opts.turbo == "" || opts.samples < 11 || opts.samples > 101 || opts.target < 2*time.Second || opts.target > 30*time.Second {
		return errors.New("provide --binary and --turbo, 11..101 --samples, and --target-duration between 2s and 30s")
	}
	for index, source := range opts.sources {
		if (source != "local" && source != "team") || slices.Contains(opts.sources[:index], source) {
			return fmt.Errorf("unsupported or duplicate cache source %q; this gate executes local and team, and does not qualify Public Cache", source)
		}
	}
	for _, target := range []*string{&opts.binary, &opts.turbo} {
		absolute, err := filepath.Abs(*target)
		if err != nil {
			return err
		}
		info, err := os.Stat(absolute)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("executable unavailable: %s", absolute)
		}
		*target = absolute
	}
	if opts.output != "" {
		if _, err := os.Stat(opts.output); !errors.Is(err, os.ErrNotExist) {
			return errors.New("--output must be a new file")
		}
	}
	return nil
}

func measure(ctx context.Context, opts options) (result evidence, returnErr error) {
	result = evidence{SchemaVersion: "layercache.performance.v1", StartedAt: time.Now().UTC(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Passed: true}
	root, err := os.MkdirTemp("", "layercache-performance-")
	if err != nil {
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(root)) }()
	result.BinarySHA256, err = digestFile(opts.binary)
	if err != nil {
		return result, err
	}
	version, _, err := execute(ctx, root, opts.turbo, "--version")
	if err != nil {
		return result, err
	}
	result.TurboVersion = strings.TrimSpace(version)
	version, _, err = execute(ctx, root, "node", "--version")
	if err != nil {
		return result, err
	}
	result.NodeVersion = strings.TrimSpace(version)
	result.Iterations, err = calibrate(ctx, root, opts.target)
	if err != nil {
		return result, err
	}
	fmt.Fprintf(os.Stderr, "Calibrated %d PBKDF2 iterations for %s of CPU work; Turbo %s\n", result.Iterations, opts.target, result.TurboVersion)
	for _, source := range opts.sources {
		measured, err := measureSource(ctx, root, opts, source, result.Iterations)
		result.Sources = append(result.Sources, measured)
		if err != nil {
			return result, err
		}
		if !measured.Passed {
			returnErr = errors.Join(returnErr, fmt.Errorf("%s warm median %.1fms exceeds 50%% of cold median %.1fms", source, measured.WarmMedianMS, measured.ColdMedianMS))
		}
	}
	return result, returnErr
}
