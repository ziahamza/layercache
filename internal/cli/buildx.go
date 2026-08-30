package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/layercache/layercache/internal/buildkit"
	"github.com/layercache/layercache/internal/config"
)

func runBuildx(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "plan" && args[0] != "build") {
		return errors.New("use buildx plan or buildx build")
	}
	operation := args[0]
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("buildx "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	platform := flags.String("platform", "linux/amd64", "target platform")
	teamExport := flags.String("team-export", "", "immutable Team Cache build ID")
	load := flags.Bool("load", false, "load one platform into the local image store")
	push := flags.Bool("push", false, "push the build output")
	jsonOutput := flags.Bool("json", false, "print the result as JSON")
	var teamImports stringList
	flags.Var(&teamImports, "team-import", "Team Cache tag to import, repeatable")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return errors.New("buildx arguments must end with a build context")
	}
	if *load && *push {
		return errors.New("choose only one of --load or --push")
	}
	output := buildkit.OutputLoad
	if *push {
		output = buildkit.OutputPush
	}
	contextPath := remaining[len(remaining)-1]
	extraArgs := append([]string(nil), remaining[:len(remaining)-1]...)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if isAdapterBypassed(cfg, "buildkit") {
		teamImports = nil
		*teamExport = ""
		extraArgs = append(extraArgs, "--no-cache")
		_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit bypass is active; cache imports and Team Cache publication are disabled")
	}
	adapter, err := buildkit.New(buildkit.Config{
		BuilderName: cfg.BuildkitBuilder, TeamRepository: cfg.BuildkitTeamRepository,
	})
	if err != nil {
		return err
	}
	request := buildkit.BuildRequest{
		ContextPath: contextPath, TargetPlatform: *platform,
		TeamImportTags: teamImports, TeamExportID: *teamExport,
		Output: output, ExtraArgs: extraArgs,
	}
	if operation == "plan" {
		plan, err := adapter.Plan(request)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(plan)
	}
	buildOutput := stdout
	if *jsonOutput {
		// Keep stdout machine-readable. Buildx builder/bootstrap diagnostics are
		// still visible on stderr alongside the native raw progress stream.
		buildOutput = stderr
	}
	result, err := adapter.Execute(ctx, request, buildOutput, stderr)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(result)
	}
	_, err = fmt.Fprintf(stderr, "Layer Cache BuildKit: %d/%d completed vertices were cached\n", result.Metrics.CachedVertices, result.Metrics.CompletedVertices)
	return err
}

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	if value == "" {
		return errors.New("value cannot be empty")
	}
	*values = append(*values, value)
	return nil
}
