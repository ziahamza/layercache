package measurement

import (
	"crypto/sha256"
	"fmt"
)

// WorkspaceIdentity keeps a local filesystem path out of persisted reports
// while retaining stable correlation across worktrees and clones.
func WorkspaceIdentity(project, canonicalRoot string) string {
	if canonicalRoot == "" {
		return ""
	}
	return privateIdentity("workspace", project, canonicalRoot)
}

// BuildkitArtifactIdentity binds a report row to the completed BuildKit graph
// and target compatibility without persisting the build context path or CLI
// arguments.
func BuildkitArtifactIdentity(project, compatibilityID, graphDigest string) string {
	return privateIdentity("buildkit", project, compatibilityID, graphDigest)
}

func privateIdentity(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		_, _ = digest.Write([]byte(part))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil))
}
