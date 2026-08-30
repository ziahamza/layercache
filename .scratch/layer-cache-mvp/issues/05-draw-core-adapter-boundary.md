# Draw the boundary between the shared core and protocol adapters

Type: grilling
Status: open
Blocked by: 01, 02, 03

## Question

What responsibilities can Turborepo, BuildKit, and GitHub Actions safely share, and which lookup, upload, manifest, scope, and compatibility rules must remain owned by each adapter? Define the smallest deep interface between protocol-specific indexes and shared immutable artifacts.
