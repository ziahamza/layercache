package publicbuild_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestGitHubSourcePolicyRequiresReachabilityFromConfiguredRef(t *testing.T) {
	commit := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.EscapedPath() {
		case "/repos/acme/widgets/git/ref/heads/main":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ref": "refs/heads/main", "object": map[string]string{"type": "commit", "sha": head},
			})
		case "/repos/acme/widgets/compare/" + commit + "..." + head:
			_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ahead"})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()

	policy := publicbuild.NewGitHubSourcePolicy(publicbuild.GitHubSourcePolicyOptions{
		BaseURL: api.URL, ApprovedRefs: []string{"refs/heads/main"},
	})
	if err := policy.Approve(context.Background(), "https://github.com/acme/widgets", commit); err != nil {
		t.Fatalf("approve reachable commit: %v", err)
	}
	if err := policy.Approve(context.Background(), "https://github.com/acme/widgets", strings.Repeat("b", 40)); err == nil {
		t.Fatal("approved commit that API did not report reachable")
	}
}

func TestGitHubSourcePolicyPeelsExactAnnotatedTagWithoutUsingSameNamedBranch(t *testing.T) {
	commit := strings.Repeat("a", 40)
	firstTag := strings.Repeat("b", 40)
	secondTag := strings.Repeat("c", 40)
	tagCommit := strings.Repeat("d", 40)
	var requestedPaths []string
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPaths = append(requestedPaths, request.URL.EscapedPath())
		switch request.URL.EscapedPath() {
		case "/repos/acme/widgets/git/ref/tags/release":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ref": "refs/tags/release", "object": map[string]string{"type": "tag", "sha": firstTag},
			})
		case "/repos/acme/widgets/git/tags/" + firstTag:
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"object": map[string]string{"type": "tag", "sha": secondTag},
			})
		case "/repos/acme/widgets/git/tags/" + secondTag:
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"object": map[string]string{"type": "commit", "sha": tagCommit},
			})
		case "/repos/acme/widgets/compare/" + commit + "..." + tagCommit:
			_ = json.NewEncoder(writer).Encode(map[string]string{"status": "identical"})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()

	policy := publicbuild.NewGitHubSourcePolicy(publicbuild.GitHubSourcePolicyOptions{
		BaseURL: api.URL, ApprovedRefs: []string{"refs/tags/release"},
	})
	if err := policy.Approve(context.Background(), "https://github.com/acme/widgets", commit); err != nil {
		t.Fatalf("approve annotated release tag: %v", err)
	}
	for _, path := range requestedPaths {
		if strings.Contains(path, "/git/ref/heads/release") {
			t.Fatalf("tag admission consulted same-named branch: %s", path)
		}
	}
}

func TestRecipeAllowlistRejectsArbitraryCompleteDigest(t *testing.T) {
	const target = "@acme/widgets#build"
	maintained, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationTurbo, target)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := publicbuild.NewRecipeAllowlist([]string{maintained})
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Approve(context.Background(), publicbuild.IntegrationTurbo, target, maintained); err != nil {
		t.Fatalf("approve maintained recipe: %v", err)
	}
	if err := policy.Approve(context.Background(), publicbuild.IntegrationTurbo, target, "sha256:"+strings.Repeat("b", 64)); err == nil {
		t.Fatal("approved arbitrary complete recipe digest")
	}
	if err := policy.Approve(context.Background(), publicbuild.IntegrationTurbo, "@acme/widgets#unrelated", maintained); err == nil {
		t.Fatal("reused maintained digest under an unrelated target")
	}
	if err := policy.Approve(context.Background(), publicbuild.IntegrationActions, "build", maintained); err == nil {
		t.Fatal("reused maintained digest under an unrelated integration")
	}
}
