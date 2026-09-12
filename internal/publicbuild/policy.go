package publicbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaintainedRecipeCatalog accepts only the versioned recipe template compiled
// into Layer Cache. The caller may choose a supported named target, but cannot
// submit build instructions.
type MaintainedRecipeCatalog struct{}

const (
	MaintainedTurboExecutorSemantics    = "turbo-capture-v1"
	MaintainedBuildKitExecutorSemantics = "buildkit-oci-v1"
	MaintainedActionsExecutorSemantics  = "actions-job-v2"
)

func (MaintainedRecipeCatalog) Approve(_ context.Context, integration Integration, target, digest string) error {
	want, err := MaintainedRecipeDigest(integration, target)
	if err != nil {
		return err
	}
	if digest != want {
		return errors.New("digest does not match the maintained recipe for this integration and target")
	}
	return nil
}

func MaintainedRecipeDigest(integration Integration, target string) (string, error) {
	executorSemantics, err := MaintainedExecutorSemantics(integration)
	if err != nil {
		return "", err
	}
	return maintainedRecipeDigest(integration, target, executorSemantics)
}

// MaintainedExecutorSemantics returns the output-affecting executor version
// bound into the maintained recipe digest and guest contract. Any parser,
// command, or archive behavior change must bump the corresponding value.
func MaintainedExecutorSemantics(integration Integration) (string, error) {
	switch integration {
	case IntegrationTurbo:
		return MaintainedTurboExecutorSemantics, nil
	case IntegrationBuildKit:
		return MaintainedBuildKitExecutorSemantics, nil
	case IntegrationActions:
		return MaintainedActionsExecutorSemantics, nil
	default:
		return "", errors.New("unsupported integration")
	}
}

func maintainedRecipeDigest(integration Integration, target, executorSemantics string) (string, error) {
	if err := ValidateMaintainedTarget(integration, target); err != nil {
		return "", err
	}
	recipe := strings.Join([]string{
		"https://layercache.dev/public-build/recipe/v2",
		"integration=" + string(integration),
		"target=" + target,
		"executor-semantics=" + executorSemantics,
	}, "\n") + "\n"
	digest := sha256.Sum256([]byte(recipe))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

type GitHubSourcePolicyOptions struct {
	Client       *http.Client
	BaseURL      string
	ApprovedRefs []string
}

// GitHubSourcePolicy approves commits that GitHub reports as ancestors of one
// explicitly configured branch or release tag.
type GitHubSourcePolicy struct {
	client       *http.Client
	baseURL      string
	approvedRefs []string
}

func NewGitHubSourcePolicy(options GitHubSourcePolicyOptions) *GitHubSourcePolicy {
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &GitHubSourcePolicy{
		client: client, baseURL: baseURL,
		approvedRefs: append([]string(nil), options.ApprovedRefs...),
	}
}

func (policy *GitHubSourcePolicy) Approve(ctx context.Context, repository, commit string) error {
	parsed, err := url.Parse(repository)
	if err != nil {
		return errors.New("repository URL is invalid")
	}
	path := strings.Trim(parsed.Path, "/")
	parts := strings.Split(path, "/")
	if parsed.Host != "github.com" || len(parts) != 2 {
		return errors.New("repository is not a canonical GitHub repository")
	}
	repositoryRoute := policy.baseURL + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	if len(policy.approvedRefs) == 0 {
		return errors.New("no approved branches or release tags are configured")
	}
	for _, approvedRef := range policy.approvedRefs {
		if !validApprovedRef(approvedRef) {
			return fmt.Errorf("approved ref %q is invalid", approvedRef)
		}
		approvedCommit, err := policy.resolveApprovedRef(ctx, repositoryRoute, approvedRef)
		if err != nil {
			continue
		}
		compareRoute := repositoryRoute + "/compare/" + url.PathEscape(commit) + "..." + url.PathEscape(approvedCommit)
		var comparison struct {
			Status string `json:"status"`
		}
		if err := policy.getJSON(ctx, compareRoute, &comparison); err != nil {
			continue
		}
		if comparison.Status == "ahead" || comparison.Status == "identical" {
			return nil
		}
	}
	return errors.New("commit is not reachable from any approved branch or release tag")
}

type githubGitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func (policy *GitHubSourcePolicy) resolveApprovedRef(
	ctx context.Context,
	repositoryRoute string,
	approvedRef string,
) (string, error) {
	gitRef := strings.TrimPrefix(approvedRef, "refs/")
	refRoute := repositoryRoute + "/git/ref/" + escapeGitRef(gitRef)
	var reference struct {
		Ref    string          `json:"ref"`
		Object githubGitObject `json:"object"`
	}
	if err := policy.getJSON(ctx, refRoute, &reference); err != nil {
		return "", err
	}
	if reference.Ref != approvedRef {
		return "", errors.New("GitHub returned a different ref")
	}
	object := reference.Object
	if strings.HasPrefix(approvedRef, "refs/heads/") {
		if object.Type != "commit" {
			return "", errors.New("approved branch does not resolve to a commit")
		}
		return immutableGitHubSHA(object.SHA)
	}
	const maximumAnnotatedTagDepth = 8
	for depth := 0; depth <= maximumAnnotatedTagDepth; depth++ {
		switch object.Type {
		case "commit":
			return immutableGitHubSHA(object.SHA)
		case "tag":
			if depth == maximumAnnotatedTagDepth {
				return "", errors.New("annotated tag chain is too deep")
			}
			tagSHA, err := immutableGitHubSHA(object.SHA)
			if err != nil {
				return "", err
			}
			var tag struct {
				Object githubGitObject `json:"object"`
			}
			if err := policy.getJSON(ctx, repositoryRoute+"/git/tags/"+url.PathEscape(tagSHA), &tag); err != nil {
				return "", err
			}
			object = tag.Object
		default:
			return "", fmt.Errorf("approved tag resolves to unsupported Git object %q", object.Type)
		}
	}
	return "", errors.New("annotated tag chain is too deep")
}

func immutableGitHubSHA(value string) (string, error) {
	if !commitPattern.MatchString(value) {
		return "", errors.New("GitHub returned an invalid immutable commit SHA")
	}
	return strings.ToLower(value), nil
}

func escapeGitRef(ref string) string {
	parts := strings.Split(ref, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func validApprovedRef(ref string) bool {
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return false
	}
	return strings.HasPrefix(ref, "refs/heads/") && len(ref) > len("refs/heads/") ||
		strings.HasPrefix(ref, "refs/tags/") && len(ref) > len("refs/tags/")
}

type RecipeAllowlist struct {
	digests map[string]struct{}
}

func NewRecipeAllowlist(digests []string) (*RecipeAllowlist, error) {
	if len(digests) == 0 {
		return nil, errors.New("at least one maintained Public Build recipe digest is required")
	}
	allowed := make(map[string]struct{}, len(digests))
	for _, digest := range digests {
		digest = strings.ToLower(digest)
		if !digestPattern.MatchString(digest) {
			return nil, fmt.Errorf("invalid maintained Public Build recipe digest %q", digest)
		}
		allowed[digest] = struct{}{}
	}
	return &RecipeAllowlist{digests: allowed}, nil
}

func (allowlist *RecipeAllowlist) Approve(_ context.Context, integration Integration, target, digest string) error {
	if allowlist == nil {
		return errors.New("maintained recipe catalog is unavailable")
	}
	digest = strings.ToLower(digest)
	if _, allowed := allowlist.digests[digest]; !allowed {
		return errors.New("digest is not in the maintained recipe catalog")
	}
	want, err := MaintainedRecipeDigest(integration, target)
	if err != nil {
		return err
	}
	if digest != want {
		return errors.New("digest does not match the maintained integration and target recipe")
	}
	return nil
}

func (policy *GitHubSourcePolicy) getJSON(ctx context.Context, route string, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, route, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "layercache-public-build-admission")
	response, err := policy.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

var _ RecipePolicy = MaintainedRecipeCatalog{}
var _ RecipePolicy = (*RecipeAllowlist)(nil)
var _ SourcePolicy = (*GitHubSourcePolicy)(nil)
