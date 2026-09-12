package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/project"
)

// discoverConfiguredProject binds credentials to repository identity. A saved
// checkout path is only a location hint: its origin can change or the directory
// can be replaced by a different repository.
func discoverConfiguredProject(ctx context.Context, cfg config.Config, directory string, allowOutsideRepository bool) (project.Identity, error) {
	discovered, err := project.Discover(ctx, directory)
	if errors.Is(err, project.ErrNoRepository) && allowOutsideRepository {
		// An explicitly selected configuration remains usable for non-Git builds.
		// This exception never applies to a discovered, mismatching repository.
		return project.Identity{}, nil
	}
	if err != nil && !errors.Is(err, project.ErrNoRemote) {
		return project.Identity{}, fmt.Errorf("cannot establish current project identity: %w", err)
	}
	if discovered.Project != "" && discovered.Project == cfg.ProjectID {
		return discovered, nil
	}
	// Team-issued project IDs may be opaque. In that case the saved repository
	// binding, rather than an absolute path, identifies matching clones.
	if err == nil && !strings.HasPrefix(cfg.ProjectID, "github.com/") &&
		!strings.HasPrefix(cfg.ProjectID, "local-") &&
		discovered.ActionsRepository != "" && discovered.ActionsRepository == cfg.ActionsRepository {
		return discovered, nil
	}
	return project.Identity{}, fmt.Errorf("current checkout project %q does not match configured project %q; run setup for this project", discovered.Project, cfg.ProjectID)
}
