package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/server"
)

type projectGatewayFile struct {
	Listen               string            `json:"listen"`
	Origin               string            `json:"origin"`
	Projects             map[string]string `json:"projects"`
	RegistryURL          string            `json:"registryUrl,omitempty"`
	RegistryUsername     string            `json:"registryUsername,omitempty"`
	RegistryPasswordFile string            `json:"registryPasswordFile,omitempty"`
}

func loadProjectGateway(path string) (projectGatewayFile, server.ProjectGatewayConfig, error) {
	var file projectGatewayFile
	var cfg server.ProjectGatewayConfig
	data, err := readProtectedFile(path)
	if err != nil {
		return file, cfg, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&file); err != nil {
		return file, cfg, errors.New("invalid project gateway configuration")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return file, cfg, errors.New("trailing gateway configuration data")
	}
	host, _, err := net.SplitHostPort(file.Listen)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return file, cfg, errors.New("project gateway must listen on loopback behind a TLS gateway")
	}
	cfg.Origin = file.Origin
	cfg.Projects = map[string]config.Config{}
	for alias, configPath := range file.Projects {
		if !filepath.IsAbs(configPath) {
			return file, cfg, errors.New("project config paths must be absolute")
		}
		if _, readErr := readProtectedFile(configPath); readErr != nil {
			return file, cfg, fmt.Errorf("project %s configuration is not protected", alias)
		}
		project, loadErr := config.Load(configPath)
		if loadErr != nil {
			return file, cfg, fmt.Errorf("cannot load project %s configuration", alias)
		}
		cfg.Projects[alias] = project
	}
	cfg.RegistryURL, cfg.RegistryUsername = file.RegistryURL, file.RegistryUsername
	if file.RegistryPasswordFile != "" {
		if !filepath.IsAbs(file.RegistryPasswordFile) {
			return file, cfg, errors.New("registry credential path must be absolute")
		}
		cfg.RegistryPassword, err = readSecretFile(file.RegistryPasswordFile)
		if err != nil {
			return file, cfg, errors.New("cannot read protected registry credential")
		}
	}
	return file, cfg, nil
}

func runServeProjects(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("serve-projects", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "protected project gateway configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New("serve-projects requires --config and no positional arguments")
	}
	file, cfg, err := loadProjectGateway(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	gateway, err := server.NewProjectGateway(ctx, cfg)
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
	_, _ = fmt.Fprintf(stderr, "Layer Cache project gateway listening on http://%s\n", file.Listen)
	err = httpServer.Serve(listener)
	close(finished)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
