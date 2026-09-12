package project

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrNoRepository = errors.New("workspace is not a Git repository")
var ErrNoRemote = errors.New("Git repository has no usable origin remote")

type Identity struct {
	Root              string
	Project           string
	ActionsRepository string
	SourceRepository  string
	Commit            string
	Ref               string
	DefaultRef        string
	DefaultRefKnown   bool
}

func Discover(ctx context.Context, directory string) (Identity, error) {
	if directory == "" {
		var err error
		directory, err = os.Getwd()
		if err != nil {
			return Identity{}, err
		}
	}
	root, err := git(ctx, directory, "rev-parse", "--show-toplevel")
	if err != nil {
		return Identity{}, ErrNoRepository
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Identity{}, err
	}
	commit, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return Identity{}, fmt.Errorf("discover Git commit: %w", err)
	}
	ref := strings.TrimSpace(os.Getenv("GITHUB_REF"))
	if ref == "" {
		ref, err = git(ctx, root, "symbolic-ref", "-q", "HEAD")
		if err != nil {
			ref = "refs/commits/" + commit
		}
	}
	defaultRef, err := git(ctx, root, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
	defaultRefKnown := err == nil
	if err == nil {
		defaultRef = strings.TrimPrefix(defaultRef, "refs/remotes/origin/")
		defaultRef = "refs/heads/" + defaultRef
	} else {
		defaultRef = "refs/heads/main"
	}
	rawRemote, err := git(ctx, root, "remote", "get-url", "origin")
	if err != nil {
		localID, _ := git(ctx, root, "config", "--local", "--get", "layercache.project-id")
		if !validLocalID(localID) {
			localID = ""
		}
		return Identity{
			Root: root, Project: localID, Commit: commit, Ref: ref,
			DefaultRef: defaultRef, DefaultRefKnown: defaultRefKnown,
		}, ErrNoRemote
	}
	projectID, actionsRepository, sourceRepository, err := normalizeRemote(rawRemote)
	if err != nil {
		return Identity{
			Root: root, Commit: commit, Ref: ref,
			DefaultRef: defaultRef, DefaultRefKnown: defaultRefKnown,
		}, ErrNoRemote
	}
	return Identity{
		Root: root, Project: projectID, ActionsRepository: actionsRepository,
		SourceRepository: sourceRepository, Commit: commit, Ref: ref,
		DefaultRef: defaultRef, DefaultRefKnown: defaultRefKnown,
	}, nil
}

func normalizeRemote(raw string) (string, string, string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@github.com:") {
		raw = "https://github.com/" + strings.TrimPrefix(raw, "git@github.com:")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	if parsed.Scheme == "ssh" && strings.EqualFold(parsed.Hostname(), "github.com") {
		parsed.Scheme = "https"
		parsed.User = nil
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", "", "", ErrNoRemote
	}
	path := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !validPart(parts[0]) || !validPart(parts[1]) {
		return "", "", "", ErrNoRemote
	}
	repository := strings.ToLower(parts[0] + "/" + parts[1])
	return "github.com/" + repository, repository, "https://github.com/" + repository, nil
}

func SaveLocalID(ctx context.Context, root, projectID string) error {
	if !validLocalID(projectID) {
		return errors.New("local project identity must start with local- and contain only lowercase letters, digits, or hyphens")
	}
	if _, err := git(ctx, root, "config", "--local", "layercache.project-id", projectID); err != nil {
		return fmt.Errorf("persist local project identity: %w", err)
	}
	return nil
}

func validLocalID(projectID string) bool {
	if len(projectID) < len("local-")+12 || !strings.HasPrefix(projectID, "local-") {
		return false
	}
	for _, character := range strings.TrimPrefix(projectID, "local-") {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func validPart(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func git(ctx context.Context, directory string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
