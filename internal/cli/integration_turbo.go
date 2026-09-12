package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/config"
)

func runTurboIntegration(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("integration turbo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	root := flags.String("root", "", "Turbo repository root")
	apply := flags.Bool("apply", false, "write the repo-local Turbo endpoint configuration")
	force := flags.Bool("force", false, "replace a conflicting Turbo endpoint")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("integration turbo accepts no positional arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	projectRoot, err := integrationProjectRoot(ctx, cfg, *root)
	if err != nil {
		return err
	}
	if *apply {
		unlock, lockErr := lockIntegrationApplication(ctx, cfg, *configPath, "Turbo")
		if lockErr != nil {
			return lockErr
		}
		defer unlock()
	}
	turboDirectory := filepath.Join(projectRoot, ".turbo")
	turboConfigPath := filepath.Join(turboDirectory, "config.json")
	var state integrationState
	var record integrationRecord
	if *apply {
		state, err = loadIntegrationState(cfg)
		if err != nil {
			return err
		}
		record = state.Records["turbo"]
		if err := validateOwnedIntegrationTarget(state, "turbo", turboConfigPath); err != nil {
			return err
		}
	}
	endpoint := localRuntimeURL(cfg.Listen)
	current, exists, err := readJSONFile(turboConfigPath, 1<<20)
	if err != nil {
		return fmt.Errorf("inspect Turbo configuration: %w", err)
	}
	values := make(map[string]json.RawMessage)
	if exists {
		if err := json.Unmarshal(current, &values); err != nil {
			return fmt.Errorf("decode %s: %w", turboConfigPath, err)
		}
	}
	currentEndpoint := ""
	if raw, present := values["apiUrl"]; present {
		if err := json.Unmarshal(raw, &currentEndpoint); err != nil || strings.TrimSpace(currentEndpoint) == "" {
			return fmt.Errorf("%s has a non-string or empty apiUrl", turboConfigPath)
		}
	}
	requiresForce := currentEndpoint != "" && currentEndpoint != endpoint
	if requiresForce && *apply && !*force {
		return fmt.Errorf("%s already points Turbo at another endpoint; inspect the preview and rerun with --force to replace only apiUrl", turboConfigPath)
	}
	encodedEndpoint, _ := json.Marshal(endpoint)
	values["apiUrl"] = encodedEndpoint
	desired, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Turbo configuration: %w", err)
	}
	desired = append(desired, '\n')
	changed := !bytes.Equal(current, desired)
	result := map[string]any{
		"integration": "turbo", "preview": !*apply, "changed": changed,
		"configPath": turboConfigPath, "apiUrl": endpoint, "requiresForce": requiresForce,
		"credentialStorage": "none", "runWith": "layercache run -- turbo <args>",
	}
	if !*apply {
		return printIntegrationResult(stdout, *jsonOutput, result,
			fmt.Sprintf("Turbo preview: %s %s", changeVerb(exists, changed), turboConfigPath))
	}
	if record.State == "active" && record.OwnershipCaptured && record.Path == turboConfigPath && record.Digest == contentDigest(current) && !changed {
		result["active"] = true
		return printIntegrationResult(stdout, *jsonOutput, result,
			fmt.Sprintf("Turbo already uses the Layer Cache endpoint from %s", turboConfigPath))
	}
	if record.AppliedAt.IsZero() || changed {
		record.AppliedAt = time.Now().UTC()
	}
	record.Name = "turbo"
	record.Path = turboConfigPath
	record.Endpoint = endpoint
	state, err = commitOwnedFile(cfg, "turbo", state, record, desired)
	if err != nil {
		return err
	}
	result["active"] = true
	return printIntegrationResult(stdout, *jsonOutput, result,
		fmt.Sprintf("Turbo uses the Layer Cache endpoint from %s; run Turbo through layercache run so its short-lived credential stays out of files and command output", turboConfigPath))
}

func integrationProjectRoot(ctx context.Context, cfg config.Config, requested string) (string, error) {
	root := strings.TrimSpace(requested)
	if root == "" {
		discovered, err := discoverConfiguredProject(ctx, cfg, "", true)
		if err != nil {
			return "", err
		}
		root = discovered.Root
	}
	if root == "" {
		root = cfg.ProjectRoot
	}
	if root == "" {
		return "", errors.New("Turbo integration needs --root because no project root is configured")
	}
	if _, err := discoverConfiguredProject(ctx, cfg, root, true); err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve Turbo repository root: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("Turbo repository root %s is not a directory", absolute)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve Turbo repository root symlinks: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func changeVerb(exists, changed bool) string {
	if !changed {
		return "leave unchanged"
	}
	if exists {
		return "update"
	}
	return "create"
}

func restoreOwnedTurboIntegration(record integrationRecord) (string, error) {
	if record.Path == "" || record.Endpoint == "" || !record.OwnershipCaptured {
		return "left-unproven", nil
	}
	current, err := snapshotRegularFile(record.Path, 1<<20)
	if err != nil {
		return "left-unreadable", nil
	}
	if !current.Exists {
		if !record.PreviousExisted {
			return "already-absent", nil
		}
		return "left-user-removed", nil
	}
	var currentValues map[string]json.RawMessage
	if !json.Valid(current.Data) || json.Unmarshal(current.Data, &currentValues) != nil {
		return "left-user-modified", nil
	}
	var currentEndpoint string
	if json.Unmarshal(currentValues["apiUrl"], &currentEndpoint) != nil || currentEndpoint != record.Endpoint {
		return "left-user-modified", nil
	}
	if record.PreviousExisted {
		backup, exists, readErr := readRegularFile(record.BackupPath, 1<<20)
		if readErr != nil || !exists || contentDigest(backup) != record.PreviousDigest {
			return "", errors.New("original Turbo configuration backup is missing or changed")
		}
		var originalValues map[string]json.RawMessage
		if !json.Valid(backup) || json.Unmarshal(backup, &originalValues) != nil {
			return "", errors.New("original Turbo configuration backup is invalid")
		}
		if originalEndpoint, existed := originalValues["apiUrl"]; existed {
			currentValues["apiUrl"] = originalEndpoint
		} else {
			delete(currentValues, "apiUrl")
		}
		currentCanonical, currentErr := json.Marshal(currentValues)
		originalCanonical, originalErr := json.Marshal(originalValues)
		if currentErr == nil && originalErr == nil && bytes.Equal(currentCanonical, originalCanonical) {
			if err := writePrivateFile(record.Path, backup); err != nil {
				return "", err
			}
			if err := os.Chmod(record.Path, fs.FileMode(record.PreviousMode)); err != nil {
				return "", err
			}
			_ = os.Remove(record.BackupPath)
			return "restored", nil
		}
	} else {
		delete(currentValues, "apiUrl")
		if len(currentValues) == 0 {
			if err := os.Remove(record.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			if record.ParentCreated {
				_ = os.Remove(filepath.Dir(record.Path))
			}
			return "removed", nil
		}
	}
	restored, err := json.MarshalIndent(currentValues, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode restored Turbo configuration: %w", err)
	}
	restored = append(restored, '\n')
	if err := writePrivateFile(record.Path, restored); err != nil {
		return "", err
	}
	mode := current.Mode.Perm()
	if record.PreviousExisted {
		mode = fs.FileMode(record.PreviousMode)
	}
	if err := os.Chmod(record.Path, mode); err != nil {
		return "", err
	}
	_ = os.Remove(record.BackupPath)
	return "restored", nil
}
