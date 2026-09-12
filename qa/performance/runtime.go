package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type daemon struct {
	configPath string
	address    string
	token      string
	command    *exec.Cmd
	log        *os.File
}

func startDaemon(ctx context.Context, binary, root, workspace, name, role string, team *daemon) (*daemon, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return nil, err
	}
	configPath := filepath.Join(root, name+".json")
	arguments := []string{
		"setup", "--config", configPath, "--data-dir", filepath.Join(root, name+"-cache"),
		"--listen", address, "--role", role, "--max-size", "268435456", "--non-interactive", "--json",
	}
	if team != nil {
		arguments = append(arguments, "--team-url", "http://"+team.address, "--team-token", team.token)
	}
	if _, _, err := execute(ctx, workspace, binary, arguments...); err != nil {
		return nil, err
	}
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var config struct {
		LocalToken string `json:"localToken"`
	}
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return nil, err
	}
	log, err := os.Create(filepath.Join(root, name+"-daemon.log"))
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, binary, "serve", "--config", configPath)
	command.Dir = workspace
	command.Env = isolatedEnvironment()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		log.Close()
		return nil, err
	}
	process := &daemon{configPath: configPath, address: address, token: config.LocalToken, command: command, log: log}
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := client.Get("http://" + address + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process, nil
			}
		}
		select {
		case <-ctx.Done():
			_ = process.close()
			return nil, ctx.Err()
		case <-deadline.C:
			_ = process.close()
			return nil, fmt.Errorf("%s daemon did not become healthy", name)
		case <-ticker.C:
		}
	}
}

func (process *daemon) close() error {
	_ = process.command.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- process.command.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		<-done
		err = errors.New("performance daemon required forced shutdown")
	}
	return errors.Join(err, process.log.Close())
}

func execute(ctx context.Context, directory, program string, arguments ...string) (string, string, error) {
	command := exec.CommandContext(ctx, program, arguments...)
	command.Dir = directory
	command.Env = isolatedEnvironment()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		// Arguments can contain temporary daemon credentials. Retain process
		// diagnostics without dumping the command line.
		err = fmt.Errorf("%s failed: %w\n%s\n%s", filepath.Base(program), err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String(), err
}

func isolatedEnvironment() []string {
	overrides := map[string]string{
		"CI": "true", "NO_COLOR": "1", "TURBO_TELEMETRY_DISABLED": "1", "GIT_TERMINAL_PROMPT": "0",
		"GITHUB_REF": "", "LAYER_CACHE_BYPASS": "", "TURBO_FORCE": "", "TURBO_RUN_SUMMARY": "",
		"TURBO_API": "", "TURBO_TOKEN": "", "TURBO_TEAM": "", "TURBO_CACHE": "",
		"LAYER_CACHE_CONFIG": "", "LAYER_CACHE_RUN_ID": "", "LAYER_CACHE_MEASUREMENT_TOKEN": "",
	}
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, exists := overrides[key]; !exists {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		if value != "" {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
