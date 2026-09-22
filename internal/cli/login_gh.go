package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/credentials"
)

// boundedCredentialOutput never retains or prints an unbounded subprocess output.
type boundedCredentialOutput struct{ bytes.Buffer }

func (b *boundedCredentialOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 16<<10 {
		return 0, errors.New("GitHub credential output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func githubCLIToken(ctx context.Context, executable string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", errors.New("GitHub CLI path must be absolute; rerun login --github-cli")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "auth", "token", "--hostname", "github.com")
	var output boundedCredentialOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", errors.New("GitHub CLI credential unavailable; run gh auth login --hostname github.com")
	}
	token := strings.TrimSpace(output.String())
	if token == "" || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("GitHub CLI returned an invalid credential")
	}
	return token, nil
}

func refreshGitHubCLICapability(ctx context.Context, cfg *config.Config) error {
	token, err := githubCLIToken(ctx, cfg.GitHubCLIPath)
	if err != nil {
		return err
	}
	exchanged, err := exchangeGitHubCapability(ctx, *cfg, token)
	if err != nil {
		return err
	}
	cfg.TeamToken, cfg.TeamTokenExpiresAt = exchanged.TeamToken, exchanged.ExpiresAt
	if exchanged.PublicAccessToken != "" {
		cfg.PublicAccessToken, cfg.PublicAccessTokenExpiresAt = exchanged.PublicAccessToken, exchanged.ExpiresAt
	}
	return nil
}

func loginWithGitHubCLI(ctx context.Context, path string, cfg config.Config, jsonOutput bool, stdout, stderr io.Writer) error {
	executable, err := exec.LookPath("gh")
	if err != nil {
		return errors.New("GitHub CLI is not installed; install gh and run gh auth login")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	updated := cfg
	updated.GitHubCLIPath, updated.GitHubCredentialAccount = executable, ""
	if err := refreshGitHubCLICapability(ctx, &updated); err != nil {
		return err
	}
	unlock, err := lockConfiguration(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := config.Load(path)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, cfg) {
		return errors.New("configuration changed during login; rerun login")
	}
	if err := rejectConfigurationMutationWhileRuntimeActive(current); err != nil {
		return err
	}
	if err := config.Save(path, updated); err != nil {
		return err
	}
	if cfg.GitHubCredentialAccount != "" {
		if err := (credentials.Store{}).Delete(ctx, cfg.GitHubCredentialAccount); err != nil {
			_, _ = fmt.Fprintln(stderr, "warning: old protected GitHub session could not be removed")
		}
	}
	return printResult(stdout, jsonOutput, map[string]any{"loggedIn": true, "project": cfg.ProjectID, "credentialProvider": "github-cli", "expiresAt": updated.TeamTokenExpiresAt}, "Layer Cache login complete using GitHub CLI")
}
