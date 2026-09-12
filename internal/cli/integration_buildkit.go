package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/buildkit"
	"github.com/layercache/layercache/internal/config"
)

var buildkitBuilderNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type buildxBuilderRecord struct {
	Name    string `json:"Name"`
	Driver  string `json:"Driver"`
	Current bool   `json:"Current"`
	Nodes   []struct {
		Name   string `json:"Name"`
		Status string `json:"Status"`
	} `json:"Nodes"`
}

type dockerCredentialStorageReport struct {
	Storage string
	Warning string
}

const dockerConfigCredentialWarning = "Docker may have stored the registry credential as reversible auth in config.json; configure an external Docker credential helper and protect Docker's configuration directory"

func runBuildkitIntegration(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("integration buildkit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	dockerCommand := flags.String("docker-command", "docker", "Docker CLI executable")
	registryUsername := flags.String("registry-username", "", "OCI registry username used with --registry-password-stdin")
	registryPasswordStdin := flags.Bool("registry-password-stdin", false, "read the OCI registry password from standard input and run Docker login")
	apply := flags.Bool("apply", false, "create and select the configured named builder")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("integration buildkit accepts no positional arguments")
	}
	*registryUsername = strings.TrimSpace(*registryUsername)
	if (*registryUsername == "") != !*registryPasswordStdin {
		return errors.New("use --registry-username and --registry-password-stdin together")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	if !buildkitBuilderNamePattern.MatchString(cfg.BuildkitBuilder) {
		return fmt.Errorf("configured BuildKit builder name %q is invalid", cfg.BuildkitBuilder)
	}
	registryHost := ""
	if *registryPasswordStdin {
		if cfg.BuildkitTeamRepository == "" {
			return errors.New("registry login requires a configured BuildKit Team Cache repository")
		}
		registryHost, err = buildkitRegistryHost(cfg.BuildkitTeamRepository)
		if err != nil {
			return err
		}
	}
	if *apply {
		unlock, lockErr := lockIntegrationApplication(ctx, cfg, *configPath, "BuildKit")
		if lockErr != nil {
			return lockErr
		}
		defer unlock()
	}
	state, err := loadIntegrationState(cfg)
	if err != nil {
		return err
	}
	if err := validateOwnedBuildkitBuilder(state, cfg.BuildkitBuilder); err != nil {
		return err
	}
	nodeName := buildkitNodeName(cfg)
	record, builderOwned := state.Records["buildkit"]
	builderOwned = builderOwned && record.OwnershipCaptured && record.Builder == cfg.BuildkitBuilder && record.BuilderNode == nodeName && record.Driver == "docker-container"
	builders, err := listBuildxBuilders(ctx, *dockerCommand)
	if err != nil {
		return err
	}
	builder, exists := builders[cfg.BuildkitBuilder]
	if exists && !builderOwned {
		return fmt.Errorf("BuildKit builder %q already exists but is not owned by this Layer Cache installation; choose another --buildkit-builder", cfg.BuildkitBuilder)
	}
	if exists && builder.Driver != "docker-container" {
		return fmt.Errorf("BuildKit builder %q uses driver %q, want docker-container; Layer Cache will not replace it", cfg.BuildkitBuilder, builder.Driver)
	}
	if exists && !builderHasNode(builder, nodeName) {
		return fmt.Errorf("BuildKit builder %q no longer has its Layer Cache-owned node; refusing to modify it", cfg.BuildkitBuilder)
	}
	localUsage := int64(0)
	if stats, statsErr := artifact.ReadStats(ctx, cfg.DataDir); statsErr == nil {
		localUsage = stats.UsageBytes
	}
	buildkitConfigPath, err := resolveThroughExistingAncestor(filepath.Join(cfg.DataDir, "buildkit", "buildkitd.toml"))
	if err != nil {
		return fmt.Errorf("resolve BuildKit daemon configuration path: %w", err)
	}
	buildkitConfig, effectiveMinFreeBytes, err := buildkitGCConfiguration(cfg, buildkitConfigPath)
	if err != nil {
		return err
	}
	configBytes, configExists, err := readRegularFile(buildkitConfigPath, 1<<20)
	if err != nil {
		return fmt.Errorf("inspect BuildKit daemon configuration: %w", err)
	}
	configOwnershipProven := record.OwnershipCaptured && record.Path == buildkitConfigPath && record.Digest != ""
	configUnchanged := configExists && configOwnershipProven && record.Digest == contentDigest(configBytes)
	if configExists && !configUnchanged {
		return fmt.Errorf("%s changed outside Layer Cache or is not covered by this installation's ownership record; restore the recorded file or uninstall the BuildKit integration before applying again", buildkitConfigPath)
	}
	if exists && !configOwnershipProven {
		return fmt.Errorf("BuildKit builder %q is recorded as owned, but its daemon configuration ownership is incomplete; uninstall the integration before applying again", cfg.BuildkitBuilder)
	}
	configNeedsRepair := exists && (!configExists || !bytes.Equal(configBytes, buildkitConfig))
	selected := exists && builder.Current
	changed := !exists || !selected || configNeedsRepair
	configState := "current"
	if !configExists {
		configState = "missing"
	} else if !bytes.Equal(configBytes, buildkitConfig) {
		configState = "policy-stale"
	}
	result := map[string]any{
		"integration": "buildkit", "preview": !*apply, "changed": changed,
		"builder": cfg.BuildkitBuilder, "driver": "docker-container", "selected": selected,
		"builderExists": exists, "buildkitdConfig": buildkitConfigPath, "configurationState": configState,
		"maxUsedBytes":            min(cfg.BuildkitGCBytes, cfg.MaxBytes),
		"currentPruneTargetBytes": buildkitEffectiveMax(cfg, localUsage), "minFreeBytes": effectiveMinFreeBytes,
		"configuredMinFreeBytes": cfg.MinFreeBytes,
		"teamRepository":         cfg.BuildkitTeamRepository,
		"publicRepository":       cfg.BuildkitPublicRepository,
		"teamMutableRef":         cfg.BuildkitBranch,
		"registryLoginRequested": *registryPasswordStdin,
		"registryLoginSucceeded": false,
	}
	if registryHost != "" {
		result["registry"] = registryHost
	}
	if !*apply {
		action := "select"
		if !exists {
			action = "create and globally select"
		} else if configNeedsRepair {
			action = "repair its daemon configuration, recreate, and globally select"
		} else if selected {
			action = "leave unchanged"
		}
		return printIntegrationResult(stdout, *jsonOutput, result,
			fmt.Sprintf("BuildKit preview: %s docker-container builder %q", action, cfg.BuildkitBuilder))
	}
	if *registryPasswordStdin {
		credentialReport, loginErr := loginBuildkitRegistry(ctx, *dockerCommand, registryHost, *registryUsername, os.Stdin)
		if loginErr != nil {
			return loginErr
		}
		result["registryLoginSucceeded"] = true
		result["registryCredentialStorage"] = credentialReport.Storage
		if credentialReport.Warning != "" {
			result["registryCredentialWarning"] = credentialReport.Warning
			if _, err := fmt.Fprintf(stderr, "warning: %s\n", credentialReport.Warning); err != nil {
				return fmt.Errorf("write Docker credential storage warning: %w", err)
			}
		}
	}
	if !exists {
		previousState := cloneIntegrationState(state)
		configSnapshot, snapshotErr := snapshotRegularFile(buildkitConfigPath, 1<<20)
		if snapshotErr != nil {
			return fmt.Errorf("snapshot BuildKit daemon configuration: %w", snapshotErr)
		}
		previousBuilder := currentBuildxBuilder(builders)
		now := time.Now().UTC()
		if !record.OwnershipCaptured {
			record.OwnershipCaptured = true
			record.PreviousExisted = configSnapshot.Exists
			record.PreviousMode = uint32(configSnapshot.Mode.Perm())
			if _, statErr := os.Lstat(filepath.Dir(buildkitConfigPath)); errors.Is(statErr, fs.ErrNotExist) {
				record.ParentCreated = true
			} else if statErr != nil {
				return fmt.Errorf("inspect BuildKit configuration directory: %w", statErr)
			}
		}
		record.Name = "buildkit"
		record.State = "creating"
		record.Path = buildkitConfigPath
		record.Digest = contentDigest(buildkitConfig)
		record.Builder = cfg.BuildkitBuilder
		record.BuilderNode = nodeName
		record.PreviousBuilder = previousBuilder
		record.Driver = "docker-container"
		record.AppliedAt = now
		backupCreated := false
		if configSnapshot.Exists && record.BackupPath == "" {
			record.PreviousDigest = contentDigest(configSnapshot.Data)
			record.BackupPath = integrationBackupPath(cfg, "buildkit", buildkitConfigPath)
			if err := ensureRealDirectory(filepath.Dir(record.BackupPath), 0o700); err != nil {
				return fmt.Errorf("prepare BuildKit configuration backup: %w", err)
			}
			if err := writePrivateFile(record.BackupPath, configSnapshot.Data); err != nil {
				return fmt.Errorf("back up BuildKit daemon configuration: %w", err)
			}
			backupCreated = true
		}
		state.Records["buildkit"] = record
		if err := saveIntegrationState(cfg, state); err != nil {
			if backupCreated {
				_ = os.Remove(record.BackupPath)
			}
			return err
		}
		if err := ensureRealDirectory(filepath.Dir(buildkitConfigPath), 0o700); err != nil {
			stateErr := saveIntegrationState(cfg, previousState)
			if backupCreated {
				_ = os.Remove(record.BackupPath)
			}
			if record.ParentCreated && !configSnapshot.Exists {
				_ = os.Remove(filepath.Dir(buildkitConfigPath))
			}
			return errors.Join(fmt.Errorf("prepare BuildKit configuration directory: %w", err), stateErr)
		}
		if err := writePrivateFile(buildkitConfigPath, buildkitConfig); err != nil {
			rollbackErr := restoreOwnedFileSnapshot(record, configSnapshot)
			stateErr := saveIntegrationState(cfg, previousState)
			if backupCreated {
				_ = os.Remove(record.BackupPath)
			}
			return errors.Join(fmt.Errorf("write BuildKit daemon configuration: %w", err), rollbackErr, stateErr)
		}
		if err := runBoundedCommand(ctx, *dockerCommand, []string{
			"buildx", "create", "--name", cfg.BuildkitBuilder, "--driver", "docker-container",
			"--node", nodeName, "--buildkitd-config", buildkitConfigPath,
		}); err != nil {
			rollbackErr := rollbackBuildkitApplication(ctx, *dockerCommand, cfg.BuildkitBuilder, nodeName,
				previousBuilder, record, configSnapshot, cfg, previousState)
			return errors.Join(fmt.Errorf("create BuildKit builder %q: %w", cfg.BuildkitBuilder, err), rollbackErr)
		}
		if err := runBoundedCommand(ctx, *dockerCommand, []string{"buildx", "use", "--global", cfg.BuildkitBuilder}); err != nil {
			rollbackErr := rollbackBuildkitApplication(ctx, *dockerCommand, cfg.BuildkitBuilder, nodeName,
				previousBuilder, record, configSnapshot, cfg, previousState)
			return errors.Join(fmt.Errorf("select BuildKit builder %q: %w", cfg.BuildkitBuilder, err), rollbackErr)
		}
		record = state.Records["buildkit"]
		record.State = "active"
		state.Records["buildkit"] = record
		if err := saveIntegrationState(cfg, state); err != nil {
			rollbackErr := rollbackBuildkitApplication(ctx, *dockerCommand, cfg.BuildkitBuilder, nodeName,
				previousBuilder, record, configSnapshot, cfg, previousState)
			return errors.Join(err, rollbackErr)
		}
	} else if configNeedsRepair {
		state, err = repairOwnedBuildkitIntegration(ctx, *dockerCommand, cfg, state, record, buildkitConfigPath, buildkitConfig)
		if err != nil {
			return err
		}
		record = state.Records["buildkit"]
		selected = true
	} else if !selected {
		if err := runBoundedCommand(ctx, *dockerCommand, []string{"buildx", "use", "--global", cfg.BuildkitBuilder}); err != nil {
			return fmt.Errorf("select BuildKit builder %q: %w", cfg.BuildkitBuilder, err)
		}
		selected = true
	}
	if exists && record.State != "active" {
		record.State = "active"
		state.Records["buildkit"] = record
		if err := saveIntegrationState(cfg, state); err != nil {
			return fmt.Errorf("finalize BuildKit integration ownership state: %w", err)
		}
	}
	result["active"] = true
	result["selected"] = true
	result["configurationState"] = "current"
	return printIntegrationResult(stdout, *jsonOutput, result,
		fmt.Sprintf("BuildKit builder %q uses docker-container state and is selected globally", cfg.BuildkitBuilder))
}

func repairOwnedBuildkitIntegration(
	ctx context.Context,
	dockerCommand string,
	cfg config.Config,
	state integrationState,
	record integrationRecord,
	configPath string,
	desired []byte,
) (integrationState, error) {
	previousState := cloneIntegrationState(state)
	configSnapshot, err := snapshotRegularFile(configPath, 1<<20)
	if err != nil {
		return state, fmt.Errorf("snapshot BuildKit daemon configuration before repair: %w", err)
	}
	record.State = "repairing"
	record.Path = configPath
	record.Digest = contentDigest(desired)
	record.AppliedAt = time.Now().UTC()
	state.Records["buildkit"] = record
	if err := saveIntegrationState(cfg, state); err != nil {
		return previousState, fmt.Errorf("record BuildKit integration repair: %w", err)
	}
	if err := ensureRealDirectory(filepath.Dir(configPath), 0o700); err != nil {
		stateErr := saveIntegrationState(cfg, previousState)
		return previousState, errors.Join(fmt.Errorf("prepare BuildKit configuration directory for repair: %w", err), stateErr)
	}
	if err := writePrivateFile(configPath, desired); err != nil {
		rollbackErr := restoreOwnedFileSnapshot(record, configSnapshot)
		stateErr := saveIntegrationState(cfg, previousState)
		return previousState, errors.Join(fmt.Errorf("repair BuildKit daemon configuration: %w", err), rollbackErr, stateErr)
	}
	if err := runBoundedCommand(ctx, dockerCommand, []string{"buildx", "rm", "--keep-state", cfg.BuildkitBuilder}); err != nil {
		rollbackErr := restoreOwnedFileSnapshot(record, configSnapshot)
		stateErr := saveIntegrationState(cfg, previousState)
		return previousState, errors.Join(fmt.Errorf("restart owned BuildKit builder %q after configuration repair: %w", cfg.BuildkitBuilder, err), rollbackErr, stateErr)
	}
	if err := runBoundedCommand(ctx, dockerCommand, []string{
		"buildx", "create", "--name", cfg.BuildkitBuilder, "--driver", "docker-container",
		"--node", record.BuilderNode, "--buildkitd-config", configPath,
	}); err != nil {
		return state, fmt.Errorf("recreate owned BuildKit builder %q after configuration repair: %w; rerun integration buildkit --apply to retry", cfg.BuildkitBuilder, err)
	}
	if err := runBoundedCommand(ctx, dockerCommand, []string{"buildx", "use", "--global", cfg.BuildkitBuilder}); err != nil {
		return state, fmt.Errorf("select repaired BuildKit builder %q: %w; rerun integration buildkit --apply to retry", cfg.BuildkitBuilder, err)
	}
	record.State = "active"
	state.Records["buildkit"] = record
	if err := saveIntegrationState(cfg, state); err != nil {
		return state, fmt.Errorf("finalize repaired BuildKit integration ownership state: %w", err)
	}
	return state, nil
}

func buildkitRegistryHost(repository string) (string, error) {
	repository = strings.TrimSpace(strings.TrimSuffix(repository, "/"))
	if repository == "" || strings.ContainsAny(repository, "@,?#\r\n\t ") || strings.Contains(repository, "://") || strings.HasPrefix(repository, "/") {
		return "", fmt.Errorf("BuildKit Team Cache repository %q is not a safe OCI repository", repository)
	}
	first, _, _ := strings.Cut(repository, "/")
	if first == "" {
		return "", fmt.Errorf("BuildKit Team Cache repository %q has no registry namespace", repository)
	}
	if first == "localhost" || strings.Contains(first, ".") || strings.Contains(first, ":") {
		return first, nil
	}
	return "docker.io", nil
}

func loginBuildkitRegistry(ctx context.Context, dockerCommand, registry, username string, passwordInput io.Reader) (dockerCredentialStorageReport, error) {
	if passwordInput == nil {
		return dockerCredentialStorageReport{}, errors.New("read Docker registry password: standard input is unavailable")
	}
	password, err := io.ReadAll(io.LimitReader(passwordInput, (64<<10)+1))
	if err != nil {
		return dockerCredentialStorageReport{}, fmt.Errorf("read Docker registry password: %w", err)
	}
	if len(password) > 64<<10 {
		return dockerCredentialStorageReport{}, errors.New("Docker registry password exceeds 64 KiB")
	}
	password = bytes.TrimSuffix(password, []byte("\n"))
	password = bytes.TrimSuffix(password, []byte("\r"))
	if len(password) == 0 {
		return dockerCredentialStorageReport{}, errors.New("Docker registry password from standard input is empty")
	}
	password = append(password, '\n')
	defer clear(password)
	command := exec.CommandContext(ctx, dockerCommand, "login", registry, "--username", username, "--password-stdin")
	command.Stdin = bytes.NewReader(password)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return dockerCredentialStorageReport{}, fmt.Errorf("Docker registry login to %s failed: %w", registry, err)
	}
	return inspectDockerCredentialStorage(registry), nil
}

func inspectDockerCredentialStorage(registry string) dockerCredentialStorageReport {
	report := dockerCredentialStorageReport{
		Storage: "unverified",
		Warning: dockerConfigCredentialWarning,
	}
	configPath, ok := dockerConfigPath()
	if !ok {
		return report
	}
	configFile, err := os.Open(configPath)
	if err != nil {
		return report
	}
	defer configFile.Close()
	info, err := configFile.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return report
	}
	var dockerConfig struct {
		CredentialsStore  string              `json:"credsStore"`
		CredentialHelpers map[string]string   `json:"credHelpers"`
		Auths             map[string]struct{} `json:"auths"`
	}
	decoder := json.NewDecoder(io.LimitReader(configFile, 1<<20))
	if err := decoder.Decode(&dockerConfig); err != nil {
		return report
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return report
	}
	if strings.TrimSpace(dockerConfig.CredentialsStore) != "" {
		return dockerCredentialStorageReport{Storage: "external-helper"}
	}
	for _, registryName := range dockerCredentialRegistryNames(registry) {
		if strings.TrimSpace(dockerConfig.CredentialHelpers[registryName]) != "" {
			return dockerCredentialStorageReport{Storage: "external-helper"}
		}
	}
	for _, registryName := range dockerCredentialRegistryNames(registry) {
		if _, ok := dockerConfig.Auths[registryName]; ok {
			return dockerCredentialStorageReport{
				Storage: "config.json",
				Warning: dockerConfigCredentialWarning,
			}
		}
	}
	return report
}

func dockerConfigPath() (string, bool) {
	if directory := os.Getenv("DOCKER_CONFIG"); directory != "" {
		return filepath.Join(directory, "config.json"), true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".docker", "config.json"), true
}

func dockerCredentialRegistryNames(registry string) []string {
	if registry == "docker.io" {
		return []string{"docker.io", "index.docker.io", "https://index.docker.io/v1/"}
	}
	return []string{registry}
}

func buildkitEffectiveMax(cfg config.Config, localUsage int64) int64 {
	remaining := cfg.MaxBytes - localUsage
	if remaining > cfg.BuildkitGCBytes {
		remaining = cfg.BuildkitGCBytes
	}
	if remaining < 1 {
		return 1
	}
	return remaining
}

func buildkitGCConfiguration(cfg config.Config, configPath string) ([]byte, int64, error) {
	minFreeBytes, err := buildkit.EffectiveMinFreeBytes(configPath, cfg.MinFreeBytes)
	if err != nil {
		return nil, 0, fmt.Errorf("calculate BuildKit GC free-space floor: %w", err)
	}
	maximum := min(cfg.BuildkitGCBytes, cfg.MaxBytes)
	if maximum < 1 {
		maximum = 1
	}
	reserved := maximum / 10
	if reserved > 2<<30 {
		reserved = 2 << 30
	}
	return []byte(fmt.Sprintf(
		"[worker.oci]\n  gc = true\n  reservedSpace = %q\n  maxUsedSpace = %q\n  minFreeSpace = %q\n",
		fmt.Sprintf("%dB", reserved), fmt.Sprintf("%dB", maximum), fmt.Sprintf("%dB", minFreeBytes),
	)), minFreeBytes, nil
}

func buildkitNodeName(cfg config.Config) string {
	digest := sha256.Sum256([]byte(cfg.InstallationID + "\x00" + cfg.BuildkitBuilder))
	return "layercache-" + hex.EncodeToString(digest[:8])
}

func builderHasNode(builder buildxBuilderRecord, nodeName string) bool {
	if len(builder.Nodes) != 1 {
		return false
	}
	return builder.Nodes[0].Name == nodeName
}

func currentBuildxBuilder(builders map[string]buildxBuilderRecord) string {
	for name, builder := range builders {
		if builder.Current {
			return name
		}
	}
	return ""
}

func rollbackBuildkitApplication(
	ctx context.Context,
	dockerCommand, builderName, nodeName, previousBuilder string,
	record integrationRecord,
	configSnapshot fileSnapshot,
	cfg config.Config,
	previousState integrationState,
) error {
	var result error
	builders, err := listBuildxBuilders(ctx, dockerCommand)
	if err != nil {
		result = errors.Join(result, fmt.Errorf("inspect BuildKit builder during rollback: %w", err))
	} else {
		if builder, exists := builders[builderName]; exists && builder.Driver == "docker-container" && builderHasNode(builder, nodeName) {
			if removeErr := runBoundedCommand(ctx, dockerCommand, []string{"buildx", "rm", builderName}); removeErr != nil {
				result = errors.Join(result, fmt.Errorf("roll back BuildKit builder: %w", removeErr))
			}
		}
	}
	if previousBuilder != "" {
		if useErr := runBoundedCommand(ctx, dockerCommand, []string{"buildx", "use", "--global", previousBuilder}); useErr != nil {
			result = errors.Join(result, fmt.Errorf("restore previous BuildKit builder selection: %w", useErr))
		}
	}
	if restoreErr := restoreOwnedFileSnapshot(record, configSnapshot); restoreErr != nil {
		result = errors.Join(result, fmt.Errorf("restore BuildKit configuration: %w", restoreErr))
	}
	if stateErr := saveIntegrationState(cfg, previousState); stateErr != nil {
		result = errors.Join(result, fmt.Errorf("restore integration ownership state: %w", stateErr))
	}
	return result
}

func listBuildxBuilders(ctx context.Context, dockerCommand string) (map[string]buildxBuilderRecord, error) {
	command := exec.CommandContext(ctx, dockerCommand, "buildx", "ls", "--timeout", "3s", "--format", "json")
	var stdout bytes.Buffer
	var stderr limitedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("list BuildKit builders: %s", commandFailure(err, stderr.String()))
	}
	builders := make(map[string]buildxBuilderRecord)
	scanner := bufio.NewScanner(io.LimitReader(&stdout, 2<<20))
	scanner.Buffer(make([]byte, 16<<10), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var builder buildxBuilderRecord
		if err := json.Unmarshal(scanner.Bytes(), &builder); err != nil {
			return nil, fmt.Errorf("decode BuildKit builder list: %w", err)
		}
		if builder.Name != "" {
			builders[builder.Name] = builder
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read BuildKit builder list: %w", err)
	}
	return builders, nil
}

func runBoundedCommand(ctx context.Context, path string, args []string) error {
	command := exec.CommandContext(ctx, path, args...)
	command.Stdout = io.Discard
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return errors.New(commandFailure(err, stderr.String()))
	}
	return nil
}

type limitedBuffer struct {
	value bytes.Buffer
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := (64 << 10) - buffer.value.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = buffer.value.Write(data)
	}
	return written, nil
}

func (buffer *limitedBuffer) String() string {
	return buffer.value.String()
}

func commandFailure(err error, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return err.Error()
	}
	return err.Error() + ": " + detail
}

func validateOwnedBuildkitBuilder(state integrationState, requested string) error {
	record, exists := state.Records["buildkit"]
	if !exists || record.Builder == requested {
		return nil
	}
	if record.Builder == "" {
		return errors.New("BuildKit integration ownership has no recorded builder; uninstall Layer Cache before applying it again")
	}
	return fmt.Errorf(
		"cannot change BuildKit integration builder from %q to %q while Layer Cache owns the original builder; uninstall Layer Cache first",
		record.Builder, requested,
	)
}

func removeOwnedBuildkitBuilder(
	ctx context.Context,
	dockerCommand string,
	record integrationRecord,
	preserveCache bool,
) (string, error) {
	if record.Builder == "" || record.BuilderNode == "" || record.Driver != "docker-container" {
		return "left-unproven", nil
	}
	builders, err := listBuildxBuilders(ctx, dockerCommand)
	if err != nil {
		return "", fmt.Errorf("inspect owned BuildKit builder before uninstall: %w", err)
	}
	builder, exists := builders[record.Builder]
	if !exists {
		return "already-absent", nil
	}
	if builder.Driver != record.Driver || !builderHasNode(builder, record.BuilderNode) {
		return "left-user-modified", nil
	}
	arguments := []string{"buildx", "rm"}
	if preserveCache {
		arguments = append(arguments, "--keep-state")
	}
	arguments = append(arguments, record.Builder)
	if err := runBoundedCommand(ctx, dockerCommand, arguments); err != nil {
		return "", fmt.Errorf("remove owned BuildKit builder %q: %w", record.Builder, err)
	}
	if record.PreviousBuilder != "" && record.PreviousBuilder != record.Builder {
		if previous, ok := builders[record.PreviousBuilder]; ok && previous.Name != "" {
			if err := runBoundedCommand(ctx, dockerCommand, []string{"buildx", "use", "--global", record.PreviousBuilder}); err != nil {
				return "", fmt.Errorf("restore previous BuildKit builder selection %q: %w", record.PreviousBuilder, err)
			}
		}
	}
	if preserveCache {
		return "removed-state-preserved", nil
	}
	return "removed", nil
}
