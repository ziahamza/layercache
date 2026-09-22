package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/credentials"
	"github.com/layercache/layercache/internal/githubauth"
)

type capabilityExchange struct {
	Token             string    `json:"token"`
	TeamToken         string    `json:"teamToken"`
	PublicAccessToken string    `json:"publicAccessToken,omitempty"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

func runLogin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	clientID := flags.String("github-client-id", os.Getenv("LAYERCACHE_GITHUB_CLIENT_ID"), "GitHub OAuth app client ID")
	githubCLI := flags.Bool("github-cli", false, "use the existing GitHub CLI login without copying its credential")
	deviceEndpoint := flags.String("github-device-endpoint", githubauth.DefaultDeviceEndpoint, "GitHub device authorization endpoint")
	tokenEndpoint := flags.String("github-token-endpoint", githubauth.DefaultTokenEndpoint, "GitHub access-token endpoint")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*githubCLI && strings.TrimSpace(*clientID) == "" {
		return errors.New("--github-client-id or LAYERCACHE_GITHUB_CLIENT_ID is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	if err := rejectConfigurationMutationWhileRuntimeActive(cfg); err != nil {
		return fmt.Errorf("refusing to replace login credentials: %w", err)
	}
	if cfg.TeamURL == "" {
		return errors.New("configure --team-url before logging in")
	}
	if *githubCLI {
		return loginWithGitHubCLI(ctx, *configPath, cfg, *jsonOutput, stdout, stderr)
	}
	client := githubauth.DeviceClient{
		ClientID:       *clientID,
		DeviceEndpoint: *deviceEndpoint,
		TokenEndpoint:  *tokenEndpoint,
		HTTPClient:     newCLIHTTPClient(controlRequestTimeout),
	}
	device, err := client.Begin(ctx)
	if err != nil {
		return err
	}
	if *jsonOutput {
		if err := json.NewEncoder(stderr).Encode(map[string]any{
			"event": "authorization_required", "verificationUri": device.VerificationURI,
			"userCode": device.UserCode, "expiresIn": device.ExpiresIn,
		}); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(stdout, "Open %s and enter code %s\n", device.VerificationURI, device.UserCode); err != nil {
		return err
	}
	session, err := client.Poll(ctx, device)
	if err != nil {
		return err
	}
	unlockConfiguration, err := lockConfiguration(ctx, *configPath)
	if err != nil {
		return err
	}
	defer unlockConfiguration()
	current, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("reload Layer Cache configuration before login commit: %w", err)
	}
	if !reflect.DeepEqual(current, cfg) {
		return errors.New("Layer Cache configuration changed during login; rerun login against the current configuration")
	}
	if err := rejectConfigurationMutationWhileRuntimeActive(current); err != nil {
		return fmt.Errorf("refusing to replace login credentials: %w", err)
	}
	account, err := newGitHubCredentialAccount(cfg)
	if err != nil {
		return err
	}
	previousAccount := cfg.GitHubCredentialAccount
	encodedSession, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode GitHub session: %w", err)
	}
	if err := (credentials.Store{}).Put(ctx, account, string(encodedSession)); err != nil {
		return fmt.Errorf("login succeeded but refresh material could not be protected: %w", err)
	}
	exchanged, err := exchangeGitHubCapability(ctx, cfg, session.AccessToken)
	if err != nil {
		return cleanupFailedLogin(ctx, account, err)
	}
	cfg.GitHubCredentialAccount = account
	cfg.GitHubCLIPath = ""
	cfg.TeamToken = exchanged.TeamToken
	cfg.TeamTokenExpiresAt = exchanged.ExpiresAt
	if exchanged.PublicAccessToken != "" {
		cfg.PublicAccessToken = exchanged.PublicAccessToken
		cfg.PublicAccessTokenExpiresAt = exchanged.ExpiresAt
	}
	if err := config.Save(*configPath, cfg); err != nil {
		return cleanupFailedLogin(ctx, account, fmt.Errorf("save login state: %w", err))
	}
	if previousAccount != "" && previousAccount != account {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlRequestTimeout)
		cleanupErr := (credentials.Store{}).Delete(cleanupCtx, previousAccount)
		cancel()
		if cleanupErr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: login succeeded, but the previous protected GitHub session could not be removed: %v\n", cleanupErr)
		}
	}
	return printResult(stdout, *jsonOutput, map[string]any{
		"loggedIn":  true,
		"project":   cfg.ProjectID,
		"expiresAt": exchanged.ExpiresAt,
	}, "Layer Cache login complete")
}

func githubCredentialAccount(cfg config.Config) string {
	sum := sha256.Sum256([]byte(cfg.TeamURL + "\x00" + cfg.ProjectID))
	return "github-" + hex.EncodeToString(sum[:12])
}

func newGitHubCredentialAccount(cfg config.Config) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("create protected GitHub session account: %w", err)
	}
	return githubCredentialAccount(cfg) + "-" + hex.EncodeToString(nonce[:]), nil
}

func cleanupFailedLogin(ctx context.Context, account string, loginErr error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlRequestTimeout)
	defer cancel()
	if err := (credentials.Store{}).Delete(cleanupCtx, account); err != nil {
		return errors.Join(loginErr, fmt.Errorf("remove unused protected GitHub session: %w", err))
	}
	return loginErr
}

func exchangeGitHubCapability(ctx context.Context, cfg config.Config, githubToken string) (capabilityExchange, error) {
	teamExchange, err := exchangeGitHubCapabilityAt(ctx, cfg.TeamURL, cfg, githubToken)
	if err != nil {
		return capabilityExchange{}, err
	}
	if teamExchange.TeamToken == "" {
		teamExchange.TeamToken = teamExchange.Token
	}
	if teamExchange.TeamToken == "" {
		return capabilityExchange{}, errors.New("Team Cache capability exchange did not return a Team token")
	}
	if cfg.PublicURL != "" && strings.TrimRight(cfg.PublicURL, "/") != strings.TrimRight(cfg.TeamURL, "/") {
		publicExchange, exchangeErr := exchangeGitHubCapabilityAt(ctx, cfg.PublicURL, cfg, githubToken)
		if exchangeErr != nil {
			return capabilityExchange{}, exchangeErr
		}
		if publicExchange.PublicAccessToken == "" {
			publicExchange.PublicAccessToken = publicExchange.Token
		}
		if publicExchange.PublicAccessToken == "" {
			return capabilityExchange{}, errors.New("Public Cache capability exchange did not return a Public Build token")
		}
		teamExchange.PublicAccessToken = publicExchange.PublicAccessToken
		if publicExchange.ExpiresAt.Before(teamExchange.ExpiresAt) {
			teamExchange.ExpiresAt = publicExchange.ExpiresAt
		}
	}
	return teamExchange, nil
}

func exchangeGitHubCapabilityAt(ctx context.Context, baseURL string, cfg config.Config, githubToken string) (capabilityExchange, error) {
	body, err := json.Marshal(map[string]string{
		"project": cfg.ProjectID, "compatibility": cfg.CompatibilityID, "githubToken": githubToken,
		"repository": cfg.ActionsRepository, "ref": cfg.ActionsRef, "defaultRef": cfg.ActionsDefaultRef,
	})
	if err != nil {
		return capabilityExchange{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/auth/github/exchange", bytes.NewReader(body))
	if err != nil {
		return capabilityExchange{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := newCLIHTTPClient(controlRequestTimeout).Do(request)
	if err != nil {
		return capabilityExchange{}, fmt.Errorf("exchange GitHub identity for project capability: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return capabilityExchange{}, fmt.Errorf("project capability exchange returned HTTP %d", response.StatusCode)
	}
	var exchanged capabilityExchange
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&exchanged); err != nil {
		return capabilityExchange{}, fmt.Errorf("decode project capability exchange: %w", err)
	}
	if exchanged.Token == "" && exchanged.TeamToken == "" && exchanged.PublicAccessToken == "" || exchanged.ExpiresAt.Before(time.Now().Add(time.Minute)) {
		return capabilityExchange{}, errors.New("project capability exchange returned an invalid or nearly expired token")
	}
	return exchanged, nil
}

func refreshTeamCapability(ctx context.Context, cfg *config.Config) error {
	if cfg.TeamURL == "" || cfg.GitHubCredentialAccount == "" && cfg.GitHubCLIPath == "" {
		return nil
	}
	if cfg.TeamToken != "" && cfg.TeamTokenExpiresAt.After(time.Now().Add(5*time.Minute)) {
		return nil
	}
	if cfg.GitHubCLIPath != "" {
		return refreshGitHubCLICapability(ctx, cfg)
	}
	encoded, err := (credentials.Store{}).Get(ctx, cfg.GitHubCredentialAccount)
	if err != nil {
		return fmt.Errorf("refresh Team Cache capability: %w", err)
	}
	var session githubauth.Session
	if err := json.Unmarshal([]byte(encoded), &session); err != nil || session.AccessToken == "" {
		return errors.New("refresh Team Cache capability: stored GitHub session is invalid")
	}
	if session.AccessTokenNeedsRefresh(time.Now().UTC(), 2*time.Minute) {
		refreshed, refreshErr := (githubauth.DeviceClient{
			ClientID: session.ClientID, TokenEndpoint: session.TokenEndpoint,
			HTTPClient: newCLIHTTPClient(controlRequestTimeout),
		}).Refresh(ctx, session)
		if refreshErr != nil {
			return fmt.Errorf("refresh Team Cache capability: %w", refreshErr)
		}
		refreshedJSON, marshalErr := json.Marshal(refreshed)
		if marshalErr != nil {
			return fmt.Errorf("refresh Team Cache capability: encode rotated GitHub session: %w", marshalErr)
		}
		if putErr := (credentials.Store{}).Put(ctx, cfg.GitHubCredentialAccount, string(refreshedJSON)); putErr != nil {
			return fmt.Errorf("refresh Team Cache capability: protect rotated GitHub session: %w", putErr)
		}
		session = refreshed
	}
	exchanged, err := exchangeGitHubCapability(ctx, *cfg, session.AccessToken)
	if err != nil {
		return err
	}
	cfg.TeamToken = exchanged.TeamToken
	cfg.TeamTokenExpiresAt = exchanged.ExpiresAt
	if exchanged.PublicAccessToken != "" {
		cfg.PublicAccessToken = exchanged.PublicAccessToken
		cfg.PublicAccessTokenExpiresAt = exchanged.ExpiresAt
	}
	return nil
}

func refreshAndPersistTeamCapability(ctx context.Context, configPath string, cfg *config.Config) error {
	before := *cfg
	if err := refreshTeamCapability(ctx, cfg); err != nil {
		return err
	}
	if cfg.TeamToken == before.TeamToken && cfg.TeamTokenExpiresAt.Equal(before.TeamTokenExpiresAt) &&
		cfg.PublicAccessToken == before.PublicAccessToken && cfg.PublicAccessTokenExpiresAt.Equal(before.PublicAccessTokenExpiresAt) {
		return nil
	}
	return persistRefreshedCapabilities(ctx, configPath, before, *cfg)
}

func persistRefreshedCapabilities(
	ctx context.Context,
	configPath string,
	expected config.Config,
	refreshed config.Config,
) error {
	unlock, err := lockConfiguration(ctx, configPath)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("reload configuration before saving refreshed credentials: %w", err)
	}
	if !sameCredentialScope(current, expected) {
		return errors.New("configuration credential scope changed during refresh; discarded refreshed credentials")
	}
	current.TeamToken = refreshed.TeamToken
	current.TeamTokenExpiresAt = refreshed.TeamTokenExpiresAt
	current.PublicAccessToken = refreshed.PublicAccessToken
	current.PublicAccessTokenExpiresAt = refreshed.PublicAccessTokenExpiresAt
	if err := config.Save(configPath, current); err != nil {
		return fmt.Errorf("save refreshed project capabilities: %w", err)
	}
	return nil
}

func sameCredentialScope(left, right config.Config) bool {
	return left.InstallationID == right.InstallationID &&
		left.GitHubCredentialAccount == right.GitHubCredentialAccount &&
		left.GitHubCLIPath == right.GitHubCLIPath &&
		left.TeamURL == right.TeamURL &&
		left.PublicURL == right.PublicURL &&
		left.ProjectID == right.ProjectID &&
		left.CompatibilityID == right.CompatibilityID &&
		left.ActionsRepository == right.ActionsRepository &&
		left.ActionsRef == right.ActionsRef &&
		left.ActionsDefaultRef == right.ActionsDefaultRef
}
