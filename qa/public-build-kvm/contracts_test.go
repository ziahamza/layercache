package publicbuildqa_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publicbuild/sandbox"
)

// A fixture contract drifting from production admission can make cross-build
// checks pass while the first native request is rejected before execution.
func TestMaintainedGuestContractsMatchProductionRecipesOnBothPlatforms(t *testing.T) {
	for _, architecture := range []string{"amd64", "arm64"} {
		for _, integration := range []string{"turbo", "actions", "buildkit"} {
			t.Run(integration+"/"+architecture, func(t *testing.T) {
				path := integration + "-" + architecture + "-contract.json"
				encoded, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(encoded)
				contract, err := sandbox.LoadGuestContract(path, "sha256:"+hex.EncodeToString(digest[:]))
				if err != nil {
					t.Fatal(err)
				}
				platform := publicbuild.Platform("linux/" + architecture)
				if contract.Platform != platform || len(contract.Recipes) != 1 {
					t.Fatalf("contract platform/recipe mismatch: %+v", contract)
				}
				recipe := contract.Recipes[0]
				want, err := publicbuild.MaintainedRecipeDigest(publicbuild.Integration(integration), recipe.Target)
				if err != nil {
					t.Fatal(err)
				}
				if recipe.Digest != want || string(recipe.Integration) != integration {
					t.Fatalf("contract diverged from maintained production recipe %s", want)
				}
				if err := contract.CoversServerRecipes([]string{want}); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(encoded), `"no-network"`) {
					t.Fatal("reviewed offline contract is missing")
				}
			})
		}
	}
}
