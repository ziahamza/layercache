package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
)

func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	commandArgs := flags.Args()
	if len(commandArgs) == 0 {
		return errors.New("a command is required after --")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if _, err := probeRuntime(cfg); err != nil {
		return errors.New("Layer Cache is not running; run layercache start first")
	}
	random, err := config.NewToken()
	if err != nil {
		return err
	}
	runID := "run-" + random[:20]
	workspaceToken, err := access.MintWorkspaceToken(cfg.LocalToken, runID, time.Now().UTC(), 12*time.Hour)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = os.Environ()
	injected := map[string]string{
		"LAYER_CACHE_RUN_ID":       runID,
		"LAYER_CACHE_CONFIG":       *configPath,
		"LAYER_CACHE_BYPASS":       strings.Join(cfg.BypassAdapters, ","),
		"TURBO_API":                "http://" + cfg.Listen,
		"TURBO_TOKEN":              workspaceToken,
		"TURBO_TEAM":               cfg.ProjectID,
		"ACTIONS_CACHE_URL":        "http://" + cfg.Listen + "/",
		"ACTIONS_RUNTIME_TOKEN":    workspaceToken,
		"ACTIONS_CACHE_SERVICE_V2": "",
	}
	if isAdapterBypassed(cfg, "turbo") {
		injected["TURBO_API"] = ""
		injected["TURBO_TOKEN"] = ""
		injected["TURBO_TEAM"] = ""
	}
	if isAdapterBypassed(cfg, "actions") {
		injected["ACTIONS_CACHE_URL"] = ""
		injected["ACTIONS_RUNTIME_TOKEN"] = ""
	}
	for key, value := range injected {
		command.Env = replaceEnvironment(command.Env, key, value)
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("command failed in Layer Cache run %s: %w", runID, err)
	}
	_, _ = fmt.Fprintf(stderr, "Layer Cache run: %s\n", runID)
	return nil
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	if value != "" {
		result = append(result, prefix+value)
	}
	return result
}
