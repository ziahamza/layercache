# LayerCache launch plan — 2026-09-26

The immediate product is a self-serve **Team Cache**: an engineer installs the CLI, signs in with GitHub, creates a team and repository project, connects a local build, invites a teammate, and sees real cache reuse. The implementation for that flow is merged in [dashboard PR #11](https://github.com/ziahamza/layercache/pull/11) and [cloud PR #12](https://github.com/ziahamza/layercache/pull/12). The service is not deployed, and no immutable CLI release exists yet. [The evidence audit](launch-readiness-research.md) separates implemented code from observed behavior.

## Launch gates, in order

| Priority | Gate | Evidence required to close it |
| --- | --- | --- |
| Closed 2026-09-26 | Preserve existing cache availability | [Config PR #26](https://github.com/ziahamza/config/pull/26) pins public replies to the direct route while Tailscale uses an exit node. The [unchanged main-branch external health run](https://github.com/ziahamza/layercache/actions/runs/36220261008) passed; continue monitoring before adding another public route. The existing gateway and project data were untouched. |
| P0 | Bind a project to its immutable GitHub repository identity | A repository recreated at the same `owner/repo` path cannot receive the previous project's Actions capability. Existing operator-managed projects keep their current policy. |
| P0 | Publish an installable beta CLI | A release tag from protected main passes native Local/Team and cloud persistence gates, creates immutable attested archives, and installs through `scripts/install.sh` on a clean machine. The cache-only beta excludes Public Build claims and does not wait for native Public Build KVM qualification; stable releases still require that qualification. |
| P0 | Bound beta admission | Only an invited set of GitHub identities can create teams before billing or project lifecycle exists. Invited teammates can still accept membership and use a project. Record the initial cohort and storage budget. |
| P0 | Deploy one isolated cloud origin | Real GitHub OAuth with device flow; exact HTTPS host/callback; protected secrets; separate bounded storage; a local-only service listener; public health; metadata/key backup and restart recovery. Verify sign-in, project creation, invites, roles, cache requests, and rollback on the real route. |
| P0 | Prove the customer path | From a clean install, one engineer creates a team and connects a repository. A second identity accepts an invitation. A cold Turbo build uploads, and an independent fresh GitHub runner or machine with an empty Local Cache restores the same output through Team Cache. Confirm source, bytes, hit status and reported timing without counting a same-daemon hit as remote reuse. Verify role downgrade/removal ends access. |

The initial launch is a limited beta with an explicit operator budget and cohort. Project creation is already bounded per team and globally; storage admissions fail closed at the physical pool boundary. Implement the team-creator admission gate before exposing the new route. Before unrestricted sign-up, add self-serve billing/quotas, account and project lifecycle, and support escalation. Do not describe an unqualified public signup path as generally available.

## Next investments

1. **Make the first successful build easy to repeat.** Generate copyable CI setup for a selected project and immutable release, measure where sign-in, install, repository permission checks, or cache connection fail, and keep cache misses non-blocking for builds. Qualify one more representative workload and publish precise hit/restore evidence. BuildKit's managed registry is outside this cloud slice.
2. **Add machine enrollment once teams use the cache.** Give each machine a revocable project-scoped identity, last-seen state, short credentials, and bounded heartbeat/usage reporting. The current CLI configuration and daemon PID are not cloud machine identities. Add remote cache policy management only after enrollment and audit history are defined.
3. **Add hosted runners after cache adoption and operating costs are known.** Runner provisioning needs job admission and leases, cancellation, resource limits, network/secrets isolation, cleanup, usage accounting, and recovery. It should be a separate service from Public Builds, whose output publication has a different trust model. Avoid binding the cache beta to runner scheduling.
4. **Expand platforms and Public Builds with separate evidence.** Public Build release claims still require native arm64 KVM qualification. Actions cache v2, Windows, Xcode, and a wider adapter SDK remain separate product choices rather than launch blockers for the Team Cache beta.

Review this order after the real two-user, fresh-runner acceptance run. The first material signal is whether users can restore useful work from another environment; an empty dashboard or a same-daemon hit is not enough.
