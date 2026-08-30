# Establish the Turborepo adapter contract

Type: research
Status: resolved
Blocked by: none

## Question

What exact Turborepo v8 remote-cache behavior must Layer Cache implement, which native Local Cache behavior should it preserve, which metadata can support ROI reporting, and where must platform compatibility, authorization, integrity, and Public Cache provenance be enforced?

## Answer

Preserve Turborepo's native Local Cache and implement one opaque-byte v8 Remote
Cache gateway that resolves Team Cache before trusted, host-compatible Public
Cache. Turbo's stored task duration and Run Summary support task hit rate and
net estimated savings, but raw cache events do not. Enforce compatibility and
authorization in gateway namespaces, and let only attested Public Builds create
public metadata; native shared-secret HMAC is insufficient for public
provenance. Full contract and citations: [Turborepo adapter
research](../research/turborepo-adapter.md).
