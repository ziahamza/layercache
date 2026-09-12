package cli

import (
	"context"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
)

const localIntegrationCapabilityLifetime = 12 * time.Hour

func mintLocalIntegrationCapability(ctx context.Context, cfg config.Config, integration string) (string, time.Time, error) {
	discovered, err := discoverConfiguredProject(ctx, cfg, "", true)
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(localIntegrationCapabilityLifetime)
	claims := access.Claims{
		Subject: cfg.InstallationID, Project: cfg.ProjectID, Integration: integration,
		Compatibility: cfg.CompatibilityID, Repository: cfg.ActionsRepository,
		Ref: cfg.ActionsRef, DefaultRef: cfg.ActionsDefaultRef,
		Builder: "local-runtime", Capabilities: []access.Capability{access.CapabilityWrite},
		ExpiresAt: expiresAt,
	}
	if discovered.Root != "" {
		if discovered.ActionsRepository != "" {
			claims.Repository = discovered.ActionsRepository
		}
		claims.Ref = discovered.Ref
		claims.DefaultRef = discovered.DefaultRef
		claims.SourceCommit = discovered.Commit
	}
	token, err := access.MintCapabilityToken(cfg.LocalToken, claims, now)
	return token, expiresAt, err
}
