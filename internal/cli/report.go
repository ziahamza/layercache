package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
)

func runReport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	runID := flags.String("run", "", "Layer Cache run identity")
	fromValue := flags.String("from", "", "period start as an RFC3339 timestamp")
	toValue := flags.String("to", "", "exclusive period end as an RFC3339 timestamp")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	hasRun := *runID != ""
	hasPeriod := *fromValue != "" || *toValue != ""
	if hasRun == hasPeriod || hasPeriod && (*fromValue == "" || *toValue == "") {
		return errors.New("choose exactly one report selector: --run, or both --from and --to")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	target := "http://" + cfg.Listen + "/v1/reports/" + url.PathEscape(*runID)
	var from, to time.Time
	if hasPeriod {
		from, err = time.Parse(time.RFC3339, *fromValue)
		if err != nil {
			return fmt.Errorf("parse --from as RFC3339: %w", err)
		}
		to, err = time.Parse(time.RFC3339, *toValue)
		if err != nil {
			return fmt.Errorf("parse --to as RFC3339: %w", err)
		}
		if !to.After(from) {
			return errors.New("--to must be after --from")
		}
		query := make(url.Values, 2)
		query.Set("from", from.Format(time.RFC3339Nano))
		query.Set("to", to.Format(time.RFC3339Nano))
		target = "http://" + cfg.Listen + "/v1/reports?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("read Layer Cache run report: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && hasRun {
		return fmt.Errorf("Layer Cache run %q has no report", *runID)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Layer Cache report returned HTTP %d", response.StatusCode)
	}
	if hasPeriod {
		var report measurement.PeriodReport
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&report); err != nil {
			return fmt.Errorf("decode Layer Cache period report: %w", err)
		}
		if *jsonOutput {
			return json.NewEncoder(stdout).Encode(report)
		}
		_, err = fmt.Fprintf(stdout, "Layer Cache period %s to %s: %d/%d hits (%.0f%%), net estimate %s\n",
			report.From.Format(time.RFC3339), report.To.Format(time.RFC3339), report.Hits, report.Eligible,
			report.HitRate*100, formatEstimate(report.NetEstimatedBuildTimeSaved))
		return err
	}
	var report measurement.RunReport
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&report); err != nil {
		return fmt.Errorf("decode Layer Cache run report: %w", err)
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(report)
	}
	_, err = fmt.Fprintf(stdout, "Layer Cache run %s: %d/%d hits (%.0f%%), net estimate %s\n",
		report.RunID, report.Hits, report.Eligible, report.HitRate*100, formatEstimate(report.NetEstimatedBuildTimeSaved))
	return err
}

func formatEstimate(estimate measurement.Estimate) string {
	if estimate.Milliseconds == nil {
		return "unavailable (" + string(estimate.Confidence) + " confidence)"
	}
	return fmt.Sprintf("%+dms (%s confidence)", *estimate.Milliseconds, estimate.Confidence)
}
