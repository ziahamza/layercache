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
	"strings"
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
	target := localRuntimeURL(cfg.Listen) + "/v1/reports/" + url.PathEscape(*runID)
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
		target = localRuntimeURL(cfg.Listen) + "/v1/reports?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	client := newLocalCLIHTTPClient(5 * time.Second)
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
		return printPeriodReport(stdout, report)
	}
	var report measurement.RunReport
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&report); err != nil {
		return fmt.Errorf("decode Layer Cache run report: %w", err)
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(report)
	}
	return printRunReport(stdout, report)
}

func formatEstimate(estimate measurement.Estimate) string {
	details := make([]string, 0, 3)
	if estimate.Method != "" {
		details = append(details, estimate.Method)
	}
	details = append(details, string(estimate.Confidence)+" confidence")
	if estimate.Total > 0 {
		details = append(details, fmt.Sprintf("%d/%d known", estimate.Known, estimate.Total))
	}
	detail := strings.Join(details, "; ")
	if estimate.Milliseconds == nil {
		return "unavailable (" + detail + ")"
	}
	return fmt.Sprintf("%+dms (%s)", *estimate.Milliseconds, detail)
}

func printRunReport(output io.Writer, report measurement.RunReport) error {
	if _, err := fmt.Fprintf(output, "Layer Cache run %s\n", report.RunID); err != nil {
		return err
	}
	return printReportSummary(output, report.Hits, report.Eligible, report.Misses, report.HitRate,
		report.Bytes, report.Sources, report.GrossAvoidedTaskTime, report.Timing,
		report.NetEstimatedBuildTimeSaved, report.Degraded)
}

func printPeriodReport(output io.Writer, report measurement.PeriodReport) error {
	if _, err := fmt.Fprintf(output, "Layer Cache period %s to %s (%d runs)\n",
		report.From.Format(time.RFC3339), report.To.Format(time.RFC3339), report.Runs); err != nil {
		return err
	}
	return printReportSummary(output, report.Hits, report.Eligible, report.Misses, report.HitRate,
		report.Bytes, report.Sources, report.GrossAvoidedTaskTime, report.Timing,
		report.NetEstimatedBuildTimeSaved, report.Degraded)
}

func printReportSummary(
	output io.Writer,
	hits int,
	eligible int,
	misses int,
	hitRate float64,
	bytes measurement.Bytes,
	sources []measurement.SourceReport,
	gross measurement.Estimate,
	timing measurement.TimingReport,
	net measurement.Estimate,
	degraded bool,
) error {
	if _, err := fmt.Fprintf(output, "Cache outcomes: %d/%d hits (%.0f%%); misses: %d\n",
		hits, eligible, hitRate*100, misses); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Bytes: %s downloaded; %s uploaded\n",
		formatByteCount(bytes.Downloaded), formatByteCount(bytes.Uploaded)); err != nil {
		return err
	}
	if len(sources) == 0 {
		if _, err := fmt.Fprintln(output, "Sources: none"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(output, "Sources:"); err != nil {
			return err
		}
		for _, source := range sources {
			if _, err := fmt.Fprintf(output, "  %s: hits: %d; %s downloaded; %s uploaded\n",
				formatReportSource(source.Source), source.Hits, formatByteCount(source.Downloaded),
				formatByteCount(source.Uploaded)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(output, "Gross avoided task time: %s\n", formatEstimate(gross)); err != nil {
		return err
	}
	totalOverhead := timing.LookupMS + timing.DownloadMS + timing.VerificationMS + timing.RestoreMS + timing.UploadMS
	if _, err := fmt.Fprintf(output,
		"Measured cache overhead: %+dms (lookup %dms; download %dms; verification %dms; restore %dms; upload %dms)\n",
		totalOverhead, timing.LookupMS, timing.DownloadMS, timing.VerificationMS, timing.RestoreMS, timing.UploadMS); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Net estimated build time saved: %s\n", formatEstimate(net)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "Degraded: %s\n", yesNo(degraded))
	return err
}

func formatReportSource(source measurement.Source) string {
	switch source {
	case measurement.SourceNone:
		return "No cache source"
	case measurement.SourceLocalCache:
		return "Local Cache"
	case measurement.SourceTeamCache:
		return "Team Cache"
	case measurement.SourcePublicCache:
		return "Public Cache"
	case measurement.SourceUnattributed:
		return "Unattributed"
	default:
		return string(source)
	}
}
