package publicbuild

import "testing"

func TestMaintainedRecipeDigestBindsExecutorSemantics(t *testing.T) {
	const target = ".github/workflows/public-cache.yml#public-cache"
	first, err := maintainedRecipeDigest(IntegrationActions, target, "actions-job-v1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := maintainedRecipeDigest(IntegrationActions, target, "actions-job-v2")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("executor semantics version did not change the maintained recipe digest")
	}
}
