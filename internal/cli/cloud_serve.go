package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/portal"
	"github.com/layercache/layercache/internal/server"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type cloudFile struct {
	GitHubAPIURL           string                   `json:"githubApiUrl,omitempty"`
	GitHubAuthorizeURL     string                   `json:"githubAuthorizeUrl,omitempty"`
	GitHubTokenURL         string                   `json:"githubTokenUrl,omitempty"`
	Listen                 string                   `json:"listen"`
	Origin                 string                   `json:"origin"`
	DataDir                string                   `json:"dataDir"`
	PostgresURLFile        string                   `json:"postgresUrlFile,omitempty"`
	GitHubClientID         string                   `json:"githubClientId"`
	GitHubClientSecretFile string                   `json:"githubClientSecretFile"`
	SessionKeyFile         string                   `json:"sessionKeyFile"`
	ProjectTemplateFile    string                   `json:"projectTemplateFile"`
	StoragePool            server.StoragePoolConfig `json:"storagePool"`
}

func loadCloud(path string) (cloudFile, portal.Config, error) {
	var file cloudFile
	var cfg portal.Config
	data, err := readProtectedFile(path)
	if err != nil {
		return file, cfg, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&file); err != nil {
		return file, cfg, errors.New("invalid cloud configuration")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return file, cfg, errors.New("trailing cloud configuration data")
	}
	host, _, err := net.SplitHostPort(file.Listen)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return file, cfg, errors.New("cloud must listen on loopback behind a TLS gateway")
	}
	if err = portal.ValidateOrigin(file.Origin); err != nil {
		return file, cfg, err
	}
	for _, p := range []string{file.DataDir, file.GitHubClientSecretFile, file.SessionKeyFile, file.ProjectTemplateFile} {
		if !filepath.IsAbs(p) {
			return file, cfg, errors.New("cloud data, template and secret paths must be absolute")
		}
	}
	if !filepath.IsAbs(file.StoragePool.Path) || file.StoragePool.MaxBytes <= 0 {
		return file, cfg, errors.New("cloud requires an absolute storagePool.path and explicit positive storagePool.maxBytes quota")
	}
	if _, err = readProtectedFile(file.ProjectTemplateFile); err != nil {
		return file, cfg, errors.New("project template must be protected")
	}
	cfg.ProjectTemplate, err = config.Load(file.ProjectTemplateFile)
	if err != nil {
		return file, cfg, errors.New("invalid project template")
	}
	cfg.GitHubClientSecret, err = readSecretFile(file.GitHubClientSecretFile)
	if err != nil {
		return file, cfg, errors.New("cannot read protected GitHub OAuth secret")
	}
	cfg.SessionKey, err = readProtectedFile(file.SessionKeyFile)
	if err != nil || len(cfg.SessionKey) != 32 {
		return file, cfg, errors.New("session key must be a protected file containing exactly 32 raw bytes")
	}
	if file.PostgresURLFile != "" {
		if !filepath.IsAbs(file.PostgresURLFile) {
			return file, cfg, errors.New("Postgres secret path must be absolute")
		}
		cfg.PostgresURL, err = readSecretFile(file.PostgresURLFile)
		if err != nil {
			return file, cfg, errors.New("cannot read protected Postgres URL")
		}
	}
	cfg.GitHubAPIURL, cfg.GitHubAuthorizeURL, cfg.GitHubTokenURL = file.GitHubAPIURL, file.GitHubAuthorizeURL, file.GitHubTokenURL
	cfg.Origin = file.Origin
	cfg.DataDir = file.DataDir
	cfg.GitHubClientID = file.GitHubClientID
	cfg.StoragePool = file.StoragePool
	return file, cfg, nil
}
func runServeCloud(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("serve-cloud", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "protected cloud service configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New("serve-cloud requires --config and no positional arguments")
	}
	file, cfg, err := loadCloud(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	gateway, err := portal.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer gateway.Close()
	listener, err := net.Listen("tcp", file.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	httpServer := &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	finished := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdown); err != nil {
				_ = httpServer.Close()
			}
		case <-finished:
		}
	}()
	_, _ = fmt.Fprintf(stderr, "Layer Cache cloud listening on http://%s\n", file.Listen)
	err = httpServer.Serve(listener)
	close(finished)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
