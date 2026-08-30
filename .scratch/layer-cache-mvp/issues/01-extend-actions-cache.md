# Determine how far `actions/cache` can extend across Layer Cache

Type: research
Status: resolved
Blocked by: none

## Question

Can Layer Cache preserve stock or near-stock `actions/cache` behavior while providing substantially larger storage and a shared lookup path across Local Cache, Team Cache, and safe Public Cache artifacts? Identify the supported seams for GitHub-hosted runners, self-hosted runners, and Local CI; the v1 and v2 protocol constraints; required workflow changes; scope and key semantics; and any hard blocker to public reuse.

## Answer

[Extending `actions/cache` across Layer Cache](../research/actions-cache-extension.md): unchanged workflows can use a Layer Cache v1 adapter when Local CI controls the runtime URLs; GitHub-hosted and GitHub-managed self-hosted jobs need a near-stock Layer Cache action. Local Cache and Team Cache can share that path and use backend-owned capacity, but general Public Cache restore needs a separate provenance-verifying client because stock `actions/cache` extracts unsigned, unverified archives.
