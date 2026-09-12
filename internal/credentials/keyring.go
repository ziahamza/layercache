package credentials

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const (
	service                   = "io.layercache.credentials"
	darwinSecurityPath        = "/usr/bin/security"
	linuxSecretToolPath       = "/usr/bin/secret-tool"
	darwinEncodedSecretPrefix = "layercache-keychain-v1:"
	maximumSecurityInputLine  = 4095
)

var ErrUnavailable = errors.New("operating-system credential manager is unavailable")

// Store keeps refresh material outside the Layer Cache configuration file.
// Linux uses Secret Service through secret-tool and macOS uses Keychain.
type Store struct{}

func (Store) Put(ctx context.Context, account, secret string) error {
	if !validAccount(account) || secret == "" {
		return errors.New("credential account and secret are required")
	}
	switch runtime.GOOS {
	case "linux":
		command, err := protectedExecutable(linuxSecretToolPath, "secret-tool")
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, command, "store", "--label=Layer Cache", "service", service, "account", account)
		cmd.Stdin = strings.NewReader(secret)
		if _, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("store Layer Cache credential in Secret Service: %w", err)
		}
		return nil
	case "darwin":
		command, err := protectedExecutable(darwinSecurityPath, "security")
		if err != nil {
			return err
		}
		input, err := darwinStoreCommand(account, secret)
		if err != nil {
			return err
		}
		// security's documented -w argument exposes its value through argv.
		// Interactive mode parses this command from a pipe instead, keeping the
		// encoded secret out of process listings.
		cmd := exec.CommandContext(ctx, command, "-i")
		cmd.Stdin = strings.NewReader(input)
		if _, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("store Layer Cache credential in Keychain: %w", err)
		}
		stored, err := (Store{}).Get(ctx, account)
		if err != nil {
			return fmt.Errorf("verify Layer Cache Keychain write: %w", err)
		}
		if len(stored) != len(secret) || subtle.ConstantTimeCompare([]byte(stored), []byte(secret)) != 1 {
			return errors.New("verify Layer Cache Keychain write: stored value did not match")
		}
		return nil
	default:
		return fmt.Errorf("%w on %s", ErrUnavailable, runtime.GOOS)
	}
}

func (Store) Get(ctx context.Context, account string) (string, error) {
	if !validAccount(account) {
		return "", errors.New("credential account is required")
	}
	var cmd *exec.Cmd
	encodedDarwinValue := false
	switch runtime.GOOS {
	case "linux":
		command, err := protectedExecutable(linuxSecretToolPath, "secret-tool")
		if err != nil {
			return "", err
		}
		cmd = exec.CommandContext(ctx, command, "lookup", "service", service, "account", account)
	case "darwin":
		encodedDarwinValue = true
		command, err := protectedExecutable(darwinSecurityPath, "security")
		if err != nil {
			return "", err
		}
		cmd = exec.CommandContext(ctx, command, "find-generic-password", "-a", account, "-s", service, "-w")
	default:
		return "", fmt.Errorf("%w on %s", ErrUnavailable, runtime.GOOS)
	}
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read Layer Cache credential %q: %w", account, err)
	}
	secret := strings.TrimSpace(string(output))
	if secret == "" {
		return "", fmt.Errorf("read Layer Cache credential %q: empty value", account)
	}
	if encodedDarwinValue {
		decoded, err := decodeDarwinSecret(secret)
		if err != nil {
			return "", fmt.Errorf("read Layer Cache credential %q: %w", account, err)
		}
		return decoded, nil
	}
	return secret, nil
}

func (Store) Delete(ctx context.Context, account string) error {
	if !validAccount(account) {
		return errors.New("credential account is required")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		command, err := protectedExecutable(linuxSecretToolPath, "secret-tool")
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, command, "clear", "service", service, "account", account)
	case "darwin":
		command, err := protectedExecutable(darwinSecurityPath, "security")
		if err != nil {
			return err
		}
		cmd = exec.CommandContext(ctx, command, "delete-generic-password", "-a", account, "-s", service)
	default:
		return fmt.Errorf("%w on %s", ErrUnavailable, runtime.GOOS)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("delete Layer Cache credential %q: %w: %s", account, err, cleanOutput(output))
	}
	return nil
}

func protectedExecutable(path, name string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%w: install a protected %s at %s", ErrUnavailable, name, path)
	}
	return path, nil
}

func darwinStoreCommand(account, secret string) (string, error) {
	encoded := darwinEncodedSecretPrefix + base64.RawStdEncoding.EncodeToString([]byte(secret))
	command := "add-generic-password -U -a " + account + " -s " + service + " -w " + encoded + "\n"
	if len(command) > maximumSecurityInputLine {
		return "", fmt.Errorf("Keychain credential is too large for security's %d-byte interactive command limit", maximumSecurityInputLine)
	}
	return command, nil
}

func decodeDarwinSecret(secret string) (string, error) {
	// Raw values are retained for credentials written by releases before the
	// interactive, argv-safe encoding was introduced.
	if !strings.HasPrefix(secret, darwinEncodedSecretPrefix) {
		return secret, nil
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(secret, darwinEncodedSecretPrefix))
	if err != nil || len(decoded) == 0 {
		return "", errors.New("invalid encoded value")
	}
	return string(decoded), nil
}

func validAccount(account string) bool {
	if account == "" || len(account) > 128 || strings.TrimSpace(account) != account {
		return false
	}
	for _, character := range account {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && !strings.ContainsRune("._:@/-", character) {
			return false
		}
	}
	return true
}

func cleanOutput(output []byte) string {
	output = bytes.TrimSpace(output)
	if len(output) > 512 {
		output = output[:512]
	}
	return string(output)
}
