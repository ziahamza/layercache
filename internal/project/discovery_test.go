package project_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/project"
)

func TestDiscoverNormalizesGitHubIdentityAndRefs(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-b", "trunk")
	runGit(t, root, "config", "user.email", "qa@layercache.dev")
	runGit(t, root, "config", "user.name", "Layer Cache QA")
	runGit(t, root, "remote", "add", "origin", "git@github.com:Acme/Widgets.git")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "commit", "-m", "fixture")

	identity, err := project.Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Project != "github.com/acme/widgets" || identity.ActionsRepository != "acme/widgets" {
		t.Fatalf("identity = %#v", identity)
	}
	if identity.Ref != "refs/heads/trunk" || identity.Commit == "" || identity.Root != root {
		t.Fatalf("source = %#v", identity)
	}
	if identity.DefaultRefKnown {
		t.Fatalf("default ref unexpectedly treated fallback as discovered: %#v", identity)
	}

	runGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	identity, err = project.Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !identity.DefaultRefKnown || identity.DefaultRef != "refs/heads/trunk" {
		t.Fatalf("discovered default ref = %#v", identity)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
