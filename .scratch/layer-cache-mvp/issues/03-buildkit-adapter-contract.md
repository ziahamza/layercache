# Establish the Docker BuildKit adapter contract

Type: research
Status: resolved
Blocked by: none

## Question

Which native BuildKit interfaces should Layer Cache compose for Local Cache, Team Cache, Public Cache, and Public Builds? Resolve registry and OCI semantics, local-cache lifecycle, target-platform compatibility, concurrent publication, provenance, trust, remote execution, and the signals available for cache-hit and time-saved reporting.

## Answer

Compose a persistent named BuildKit builder for Local Cache, OCI registry cache import and export for Team Cache, digest-pinned registry imports with service-only publication for Public Cache, and Buildx's mutual-TLS remote driver for Public Builds. Serialize mutable cache publication, route cache refs by target platform, and import only remote caches that satisfy the applicable trust policy. BuildKit reports cached vertices and elapsed time, but it does not store producer duration or identify the tier that supplied a hit, so net time saved and tier attribution need Layer Cache measurements. Full evidence and the adapter contract are in [BuildKit adapter contract](../research/buildkit-adapter.md).
