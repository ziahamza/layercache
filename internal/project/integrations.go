package project

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func DetectIntegrations(root string) []string {
	if root == "" {
		return nil
	}
	detected := make(map[string]struct{})
	for _, name := range []string{"turbo.json", "turbo.jsonc"} {
		if regularFile(filepath.Join(root, name)) {
			detected["turbo"] = struct{}{}
		}
	}
	if packageJSON, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil &&
		(strings.Contains(string(packageJSON), `"turbo"`) || strings.Contains(string(packageJSON), `"turbo.json"`)) {
		detected["turbo"] = struct{}{}
	}
	if regularFile(filepath.Join(root, "Dockerfile")) || regularFile(filepath.Join(root, "docker-bake.hcl")) ||
		regularFile(filepath.Join(root, "compose.yaml")) || regularFile(filepath.Join(root, "docker-compose.yml")) {
		detected["buildkit"] = struct{}{}
	}
	workflowRoot := filepath.Join(root, ".github", "workflows")
	_ = filepath.WalkDir(workflowRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != workflowRoot {
				return fs.SkipDir
			}
			return nil
		}
		if extension := strings.ToLower(filepath.Ext(path)); extension != ".yml" && extension != ".yaml" {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err == nil && (strings.Contains(string(contents), "actions/cache@") || strings.Contains(string(contents), "layercache")) {
			detected["actions"] = struct{}{}
		}
		return nil
	})
	result := make([]string, 0, len(detected))
	for integration := range detected {
		result = append(result, integration)
	}
	slices.Sort(result)
	return result
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
