# Prototype transparent GitHub Actions caching across Workspaces

Type: prototype
Status: open
Blocked by: 01, 05, 06, 07, 08, 10, 12, 13

## Question

Can existing `actions/cache` declarations run unchanged under Local CI while new worktrees and disposable VMs reuse Local Cache, fall through to a substantially larger Team Cache, and safely consume eligible Public Cache artifacts? Prove expected key, version, ref, restore-key, save, and security behavior. Also prove that replacing one action reference on GitHub-hosted or GitHub-managed self-hosted runners reaches the same backend without changing the workflow's cache design.
