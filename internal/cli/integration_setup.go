package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/config"
)

const integrationStateVersion = 1

type integrationState struct {
	Version        int                          `json:"version"`
	InstallationID string                       `json:"installationId"`
	Records        map[string]integrationRecord `json:"records"`
}

type integrationRecord struct {
	Name                string    `json:"name"`
	State               string    `json:"state"`
	Path                string    `json:"path,omitempty"`
	Digest              string    `json:"digest,omitempty"`
	OwnershipCaptured   bool      `json:"ownershipCaptured,omitempty"`
	PreviousExisted     bool      `json:"previousExisted,omitempty"`
	PreviousDigest      string    `json:"previousDigest,omitempty"`
	PreviousMode        uint32    `json:"previousMode,omitempty"`
	BackupPath          string    `json:"backupPath,omitempty"`
	ParentCreated       bool      `json:"parentCreated,omitempty"`
	Endpoint            string    `json:"endpoint,omitempty"`
	Builder             string    `json:"builder,omitempty"`
	BuilderNode         string    `json:"builderNode,omitempty"`
	PreviousBuilder     string    `json:"previousBuilder,omitempty"`
	Driver              string    `json:"driver,omitempty"`
	AppliedAt           time.Time `json:"appliedAt"`
	CredentialExpiresAt time.Time `json:"credentialExpiresAt,omitempty"`
}

type fileSnapshot struct {
	Exists bool
	Mode   fs.FileMode
	Data   []byte
}

type integrationCleanupResult struct {
	Restored []string `json:"restored,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	Left     []string `json:"leftUnchanged,omitempty"`
}

func runIntegration(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("an integration name is required: turbo, buildkit, or local-ci")
	}
	switch args[0] {
	case "turbo":
		return runTurboIntegration(ctx, args[1:], stdout, stderr)
	case "buildkit":
		return runBuildkitIntegration(ctx, args[1:], stdout, stderr)
	case "actions", "local-ci":
		return runLocalCIIntegration(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown integration %q", args[0])
	}
}

func commitOwnedFile(
	cfg config.Config,
	key string,
	state integrationState,
	record integrationRecord,
	contents []byte,
) (integrationState, error) {
	if err := validateOwnedIntegrationTarget(state, key, record.Path); err != nil {
		return state, err
	}
	previousState := cloneIntegrationState(state)
	current, err := snapshotRegularFile(record.Path, 1<<20)
	if err != nil {
		return state, err
	}
	backupCreated := false
	if !record.OwnershipCaptured {
		record.OwnershipCaptured = true
		record.PreviousExisted = current.Exists
		record.PreviousMode = uint32(current.Mode.Perm())
		if current.Exists {
			record.PreviousDigest = contentDigest(current.Data)
			record.BackupPath = integrationBackupPath(cfg, key, record.Path)
			if err := ensureRealDirectory(filepath.Dir(record.BackupPath), 0o700); err != nil {
				return state, fmt.Errorf("prepare integration backup directory: %w", err)
			}
			if err := writePrivateFile(record.BackupPath, current.Data); err != nil {
				return state, fmt.Errorf("back up existing integration configuration: %w", err)
			}
			backupCreated = true
		}
		if _, err := os.Lstat(filepath.Dir(record.Path)); errors.Is(err, fs.ErrNotExist) {
			record.ParentCreated = true
		} else if err != nil {
			if backupCreated {
				_ = os.Remove(record.BackupPath)
			}
			return state, fmt.Errorf("inspect integration configuration directory: %w", err)
		}
	}
	record.Digest = contentDigest(contents)
	record.State = "applying"
	state.Records[key] = record
	if err := saveIntegrationState(cfg, state); err != nil {
		if backupCreated {
			_ = os.Remove(record.BackupPath)
		}
		return previousState, err
	}
	if err := ensureRealDirectory(filepath.Dir(record.Path), 0o700); err != nil {
		_ = saveIntegrationState(cfg, previousState)
		if backupCreated {
			_ = os.Remove(record.BackupPath)
		}
		if record.ParentCreated && !current.Exists {
			_ = os.Remove(filepath.Dir(record.Path))
		}
		return previousState, fmt.Errorf("prepare integration configuration directory: %w", err)
	}
	if err := writePrivateFile(record.Path, contents); err != nil {
		rollbackErr := restoreOwnedFileSnapshot(record, current)
		stateErr := saveIntegrationState(cfg, previousState)
		if backupCreated {
			_ = os.Remove(record.BackupPath)
		}
		return previousState, errors.Join(err, rollbackErr, stateErr)
	}
	record.State = "active"
	state.Records[key] = record
	if err := saveIntegrationState(cfg, state); err != nil {
		rollbackErr := restoreOwnedFileSnapshot(record, current)
		stateErr := saveIntegrationState(cfg, previousState)
		if backupCreated {
			_ = os.Remove(record.BackupPath)
		}
		return previousState, errors.Join(err, rollbackErr, stateErr)
	}
	return state, nil
}

func validateOwnedIntegrationTarget(state integrationState, key, target string) error {
	previous, exists := state.Records[key]
	if !exists || !previous.OwnershipCaptured || previous.Path == target {
		return nil
	}
	if previous.Path == "" {
		return fmt.Errorf("%s integration ownership has no recorded path; uninstall Layer Cache before applying it again", key)
	}
	return fmt.Errorf(
		"cannot change %s integration path from %s to %s while Layer Cache owns the original path; uninstall Layer Cache first",
		key, previous.Path, target,
	)
}

func lockIntegrationApplication(
	ctx context.Context,
	cfg config.Config,
	configPath string,
	integration string,
) (func(), error) {
	unlock, err := lockIntegrationState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	current, err := config.Load(configPath)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("reload Layer Cache configuration before applying %s integration: %w", integration, err)
	}
	if !reflect.DeepEqual(current, cfg) {
		unlock()
		return nil, fmt.Errorf("Layer Cache configuration changed before applying %s integration; rerun the command", integration)
	}
	if err := verifyOwnershipMarker(cfg, configPath); err != nil {
		unlock()
		return nil, fmt.Errorf("verify Layer Cache installation before applying %s integration: %w", integration, err)
	}
	return unlock, nil
}

func rejectOwnedIntegrationConfigChange(original, requested config.Config) error {
	state, err := loadIntegrationState(original)
	if err != nil {
		return fmt.Errorf("inspect owned integrations before changing setup: %w", err)
	}
	type affectedIntegration struct {
		name   string
		fields []string
	}
	affected := make([]affectedIntegration, 0, 3)
	if _, owned := state.Records["turbo"]; owned && original.Listen != requested.Listen {
		affected = append(affected, affectedIntegration{name: "turbo", fields: []string{"--listen"}})
	}
	if _, owned := state.Records["local-ci"]; owned {
		fields := make([]string, 0, 7)
		for _, change := range []struct {
			field   string
			changed bool
		}{
			{field: "--listen", changed: original.Listen != requested.Listen},
			{field: "--local-token", changed: original.LocalToken != requested.LocalToken},
			{field: "--project", changed: original.ProjectID != requested.ProjectID},
			{field: "--compatibility-id", changed: original.CompatibilityID != requested.CompatibilityID},
			{field: "--actions-repository", changed: original.ActionsRepository != requested.ActionsRepository},
			{field: "--actions-ref", changed: original.ActionsRef != requested.ActionsRef},
			{field: "--actions-default-ref", changed: original.ActionsDefaultRef != requested.ActionsDefaultRef},
		} {
			if change.changed {
				fields = append(fields, change.field)
			}
		}
		if len(fields) > 0 {
			affected = append(affected, affectedIntegration{name: "local-ci", fields: fields})
		}
	}
	if _, owned := state.Records["buildkit"]; owned && original.BuildkitBuilder != requested.BuildkitBuilder {
		affected = append(affected, affectedIntegration{name: "buildkit", fields: []string{"--buildkit-builder"}})
	}
	if len(affected) == 0 {
		return nil
	}
	details := make([]string, 0, len(affected))
	for _, integration := range affected {
		details = append(details, integration.name+" ("+strings.Join(integration.fields, ", ")+")")
	}
	return fmt.Errorf(
		"cannot change setup while Layer Cache owns affected integrations: %s; uninstall Layer Cache with --preserve-cache, rerun setup, then reapply the integrations",
		strings.Join(details, "; "),
	)
}

func cloneIntegrationState(state integrationState) integrationState {
	clone := state
	clone.Records = make(map[string]integrationRecord, len(state.Records))
	for name, record := range state.Records {
		clone.Records[name] = record
	}
	return clone
}

func snapshotRegularFile(path string, maximum int64) (fileSnapshot, error) {
	data, exists, err := readRegularFile(path, maximum)
	if err != nil || !exists {
		return fileSnapshot{Exists: exists}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{Exists: true, Mode: info.Mode(), Data: data}, nil
}

func restoreFileSnapshot(path string, snapshot fileSnapshot) error {
	if !snapshot.Exists {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := ensureRealDirectory(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := writePrivateFile(path, snapshot.Data); err != nil {
		return err
	}
	return os.Chmod(path, snapshot.Mode.Perm())
}

func restoreOwnedFileSnapshot(record integrationRecord, snapshot fileSnapshot) error {
	err := restoreFileSnapshot(record.Path, snapshot)
	if record.ParentCreated && !snapshot.Exists {
		if removeErr := os.Remove(filepath.Dir(record.Path)); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) && !errors.Is(removeErr, syscall.ENOTEMPTY) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

func integrationBackupPath(cfg config.Config, key, target string) string {
	digest := sha256.Sum256([]byte(target))
	return filepath.Join(cfg.DataDir, "integrations", "backups", key+"-"+hex.EncodeToString(digest[:8])+".backup")
}

func loadIntegrationState(cfg config.Config) (integrationState, error) {
	state := integrationState{
		Version: integrationStateVersion, InstallationID: cfg.InstallationID,
		Records: make(map[string]integrationRecord),
	}
	data, exists, err := readRegularFile(integrationStatePath(cfg), 1<<20)
	if err != nil {
		return integrationState{}, fmt.Errorf("read integration ownership state: %w", err)
	}
	if !exists {
		return state, nil
	}
	return decodeIntegrationState(cfg, data)
}

func decodeIntegrationState(cfg config.Config, data []byte) (integrationState, error) {
	var state integrationState
	if err := json.Unmarshal(data, &state); err != nil {
		return integrationState{}, fmt.Errorf("decode integration ownership state: %w", err)
	}
	if state.Version != integrationStateVersion || state.InstallationID != cfg.InstallationID {
		return integrationState{}, errors.New("integration ownership state belongs to another Layer Cache installation or version")
	}
	if state.Records == nil {
		state.Records = make(map[string]integrationRecord)
	}
	return state, nil
}

func saveIntegrationState(cfg config.Config, state integrationState) error {
	state.Version = integrationStateVersion
	state.InstallationID = cfg.InstallationID
	if state.Records == nil {
		state.Records = make(map[string]integrationRecord)
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode integration ownership state: %w", err)
	}
	encoded = append(encoded, '\n')
	path := integrationStatePath(cfg)
	current, exists, err := readRegularFile(path, 1<<20)
	if err != nil {
		return fmt.Errorf("inspect integration ownership state: %w", err)
	}
	if exists && bytes.Equal(current, encoded) {
		return nil
	}
	if err := ensureRealDirectory(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prepare integration state directory: %w", err)
	}
	// Keep a second, independently renamed copy so repair can recover ownership
	// metadata instead of either abandoning credential-bearing integration files
	// or guessing what Layer Cache owns.
	if err := writePrivateFile(integrationStateRecoveryPath(cfg), encoded); err != nil {
		return fmt.Errorf("write integration ownership recovery copy: %w", err)
	}
	if err := writePrivateFile(path, encoded); err != nil {
		return fmt.Errorf("write integration ownership state: %w", err)
	}
	return nil
}

func integrationStatePath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "integrations", "state.json")
}

func integrationStateRecoveryPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "integrations", "state.recovery.json")
}

func readJSONFile(path string, maximum int64) ([]byte, bool, error) {
	data, exists, err := readRegularFile(path, maximum)
	if err != nil || !exists {
		return data, exists, err
	}
	if !json.Valid(data) {
		return nil, true, errors.New("file is not valid JSON; Layer Cache will not rewrite JSONC or malformed configuration")
	}
	return data, true, nil
}

func readRegularFile(path string, maximum int64) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, errors.New("path is not a regular file")
	}
	if info.Size() > maximum {
		return nil, true, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

func ensureRealDirectory(path string, mode fs.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return os.MkdirAll(path, mode)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path exists but is not a real directory")
	}
	return nil
}

func writePrivateFile(path string, contents []byte) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("refusing to replace a non-regular file")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".layercache-integration-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func contentDigest(contents []byte) string {
	digest := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func printIntegrationResult(stdout io.Writer, jsonOutput bool, result map[string]any, message string) error {
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(result)
	}
	_, err := fmt.Fprintln(stdout, message)
	return err
}

func cleanupOwnedIntegrations(ctx context.Context, cfg config.Config, preserveCache bool) (integrationCleanupResult, error) {
	state, err := loadIntegrationState(cfg)
	if err != nil {
		return integrationCleanupResult{}, err
	}
	result := integrationCleanupResult{}
	if record, exists := state.Records["turbo"]; exists {
		outcome, cleanupErr := restoreOwnedTurboIntegration(record)
		if cleanupErr != nil {
			return result, fmt.Errorf("clean up Turbo integration: %w", cleanupErr)
		}
		result.record("turbo", outcome)
	}
	if record, exists := state.Records["local-ci"]; exists {
		outcome, cleanupErr := restoreOwnedIntegrationFile(record)
		if cleanupErr != nil {
			return result, fmt.Errorf("clean up Local CI integration: %w", cleanupErr)
		}
		result.record("local-ci", outcome)
	}
	if record, exists := state.Records["buildkit"]; exists {
		outcome, cleanupErr := removeOwnedBuildkitBuilder(ctx, "docker", record, preserveCache)
		if cleanupErr != nil {
			return result, cleanupErr
		}
		result.record("buildkit-builder", outcome)
		fileOutcome, cleanupErr := restoreOwnedIntegrationFile(record)
		if cleanupErr != nil {
			return result, fmt.Errorf("clean up BuildKit daemon configuration: %w", cleanupErr)
		}
		result.record("buildkit-config", fileOutcome)
	}
	return result, nil
}

func restoreOwnedIntegrationFile(record integrationRecord) (string, error) {
	if record.Path == "" || record.Digest == "" || !record.OwnershipCaptured {
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
	currentDigest := contentDigest(current.Data)
	if record.PreviousExisted && currentDigest == record.PreviousDigest {
		return "already-restored", nil
	}
	if currentDigest != record.Digest {
		return "left-user-modified", nil
	}
	if record.PreviousExisted {
		backup, exists, err := readRegularFile(record.BackupPath, 1<<20)
		if err != nil || !exists || contentDigest(backup) != record.PreviousDigest {
			return "", errors.New("original configuration backup is missing or changed")
		}
		if err := writePrivateFile(record.Path, backup); err != nil {
			return "", err
		}
		if err := os.Chmod(record.Path, fs.FileMode(record.PreviousMode)); err != nil {
			return "", err
		}
		_ = os.Remove(record.BackupPath)
		return "restored", nil
	}
	if err := os.Remove(record.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if record.ParentCreated {
		_ = os.Remove(filepath.Dir(record.Path))
	}
	return "removed", nil
}

func (result *integrationCleanupResult) record(name, outcome string) {
	switch outcome {
	case "restored", "already-restored":
		result.Restored = append(result.Restored, name)
	case "removed", "removed-state-preserved", "already-absent":
		result.Removed = append(result.Removed, name+"="+outcome)
	default:
		result.Left = append(result.Left, name+"="+outcome)
	}
}

func removeIntegrationOwnershipMetadata(cfg config.Config) error {
	statePath := integrationStatePath(cfg)
	for _, path := range []string{statePath, integrationStateRecoveryPath(cfg), filepath.Join(filepath.Dir(statePath), "state.lock")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	backupsPath := filepath.Join(cfg.DataDir, "integrations", "backups")
	if err := os.RemoveAll(backupsPath); err != nil {
		return err
	}
	for _, directory := range []string{backupsPath, filepath.Dir(statePath)} {
		if err := os.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}
	return nil
}
