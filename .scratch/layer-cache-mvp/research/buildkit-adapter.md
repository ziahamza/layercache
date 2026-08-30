# BuildKit adapter contract

Research date: 2026-08-29

## Decision

Layer Cache should compose BuildKit's existing interfaces. It should not invent a Docker cache protocol.

| Layer Cache capability | Native BuildKit interface | Contract |
| --- | --- | --- |
| Local Cache | A named `docker-container` builder and its internal cache | Keep BuildKit's automatic local cache enabled. Persist the builder's state volume and configure BuildKit garbage collection to enforce the user's storage budget. |
| Portable Local Cache snapshot | `type=local` cache importer and exporter | Use only for explicit snapshot, restore, or migration. Serialize exports to one destination and prune unreferenced blobs. |
| Team Cache | `type=registry` importer and exporter | Import project and branch refs. Publish with `mode=max` to an authenticated project namespace. Never let concurrent builds publish the same mutable ref without coordination. |
| Public Cache | `type=registry` importer only | Layer Cache's trusted public builders are the only writers. The client imports a digest-pinned cache manifest after the control plane verifies its build record. |
| Public Builds | Buildx `remote` driver connected to managed `buildkitd` | Use mutual TLS. Prefer registry output with `--push`; use `--load` only when the result must enter the caller's local Docker image store. |

BuildKit already queries its internal cache on every solve. External caches augment it through one or more `--cache-from` entries, and BuildKit supports more than one importer and exporter in a build. The default `docker` driver has version and image-store-dependent cache-export support, while `docker-container` supports the full set and persists state in a dedicated Docker volume. A named `docker-container` builder is therefore the predictable Linux and Docker Desktop baseline. It works with stock `docker buildx`; non-default builders require `--load` when the caller wants the image in the local Docker daemon. [Cache backends](https://docs.docker.com/build/cache/backends/), [build drivers](https://docs.docker.com/build/builders/drivers/), [docker-container cache persistence](https://docs.docker.com/build/builders/drivers/docker-container/).

## Cache interfaces and lifecycle

BuildKit's internal cache belongs to a builder. Different builders do not share it. A `docker-container` builder stores its state and cache in a Docker volume, and `docker buildx rm --keep-state` can preserve that volume for a replacement builder with the same name. BuildKit garbage collection removes cache by age and disk thresholds. Layer Cache should translate the user's local storage budget into `reservedSpace`, `maxUsedSpace`, and `minFreeSpace` policies rather than run its own deletion logic inside BuildKit's state directory. [Docker cache optimization](https://docs.docker.com/build/cache/optimize/#use-an-external-cache), [cache persistence](https://docs.docker.com/build/builders/drivers/docker-container/#cache-persistence), [garbage collection](https://docs.docker.com/build/cache/garbage-collection/).

The `local` backend writes an OCI Image Layout to a client-side directory. Export replaces the selected entry in `index.json`, but older content-addressed blobs can remain. Current BuildKit supports `reset=true` to remove blobs no longer referenced by the current index. A local destination is not a safe concurrent write target. Layer Cache must lock each destination or export to unique destinations and promote one only after completion. [Docker local cache backend](https://docs.docker.com/build/cache/backends/local/), [current BuildKit local exporter options](https://github.com/moby/buildkit/blob/master/README.md#local-directory-1).

The `registry` backend stores cache separately from the output image. `mode=min` exports layers needed by the final result. `mode=max` also exports transferable intermediate build stages and is the Team Cache and Public Cache default. A missing import records an error in cache resolution but the build continues. Export errors fail by default; `ignore-error=true` lets a cache outage avoid failing a successful build. [Registry cache backend](https://docs.docker.com/build/cache/backends/registry/), [cache modes](https://docs.docker.com/build/cache/backends/#cache-mode).

`inline` is not part of the Layer Cache contract. It embeds cache metadata in the output image, supports only `mode=min`, and ties cache retention to image publication. `gha` remains an experimental or beta integration with GitHub branch restrictions, shared quota, scope overwrite behavior, and service-specific credentials. Docker's current overview calls `s3` and `azblob` unreleased, while upstream BuildKit documents them as experimental and requires daemon-side cloud credentials in common configurations. They may become storage implementations behind Layer Cache, but they are not portable client contracts. [Inline backend](https://docs.docker.com/build/cache/backends/inline/), [GitHub Actions backend](https://docs.docker.com/build/cache/backends/gha/), [upstream S3 and Azure implementations](https://github.com/moby/buildkit/blob/master/README.md#s3-cache-experimental).

## OCI and registry semantics

Current BuildKit defaults `local` and `registry` cache exports to OCI media types and, since BuildKit 0.21, an OCI image manifest. The manifest's layers point to compressed cache blobs. Its config descriptor points to a BuildKit-specific JSON document with media type `application/vnd.buildkit.cacheconfig.v0`. That document records cache graph digests, input links, layer indexes, diff IDs, sizes, and creation times. It does not contain an execution duration, publisher identity, or top-level target platform. [BuildKit registry options](https://github.com/moby/buildkit/blob/master/README.md#registry-push-image-and-cache-separately), [cache config schema](https://github.com/moby/buildkit/blob/master/cache/remotecache/v1/types/spec.go), [cache exporter source](https://github.com/moby/buildkit/blob/master/cache/remotecache/export.go).

OCI descriptors make blobs content-addressed. A descriptor carries the media type, byte size, and digest. A consumer should verify size and digest before processing bytes from an untrusted transport. This detects corruption or substitution after a trusted digest is known. It does not authenticate who produced the digest. OCI registry tags are mutable pointers to manifests, while a digest identifies one exact manifest byte sequence. [OCI descriptors](https://github.com/opencontainers/image-spec/blob/main/descriptor.md), [OCI image layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md), [OCI Distribution pull and push rules](https://github.com/opencontainers/distribution-spec/blob/main/spec.md).

A registry push uploads missing blobs first and publishes the manifest last. That makes one completed manifest a useful publication boundary. It does not merge two cache graphs. BuildKit's documentation warns that writing one cache location twice overwrites the previous cache. Team Cache should use immutable build refs, plus a serialized promotion for a mutable branch or main ref. If the service wants several candidates, its control plane can return a bounded list because BuildKit accepts multiple imports. [Multiple caches and overwrite behavior](https://docs.docker.com/build/cache/backends/#multiple-caches).

## Target-platform compatibility

The compatibility key is the requested target platform, `os/architecture/variant`, not the laptop's operating system. Docker Desktop on macOS runs the BuildKit daemon in a Linux VM. The native `buildctl` client exists for macOS, but upstream BuildKit does not ship a native macOS `buildkitd`. A Mac and Linux host targeting the same `linux/amd64` graph can reuse matching BuildKit records. `BUILDPLATFORM` or native tool behavior may still make some records builder-platform-specific when the Dockerfile consumes those inputs. [BuildKit platform availability](https://github.com/moby/buildkit/blob/master/README.md#quick-start), [automatic platform arguments](https://docs.docker.com/build/building/variables/#multi-platform-build-arguments).

The exported cache config has no top-level platform field. BuildKit keys individual graph operations, but its multi-node registry exporter still has an open last-writer problem where the cache from one node can replace the other platform's cache at a shared ref. Layer Cache should publish separate mutable refs per target platform and import each platform ref needed by a build. This is a service routing rule, not a replacement for BuildKit's cache keys. [Open multi-node cache publication issue](https://github.com/docker/buildx/issues/1044), [cache config schema](https://github.com/moby/buildkit/blob/master/cache/remotecache/v1/types/spec.go).

## Trust and provenance

BuildKit draws a sharp line here. Its security guide says untrusted remote cache imports may not be used, and warns that an attacker who manipulates a remote cache can cause an incorrect solver match. Digest verification alone is therefore insufficient for Public Cache. [BuildKit security boundary](https://github.com/moby/buildkit/blob/master/PROJECT.md#security-boundary).

The adapter contract is:

- Team Cache requires TLS, authenticated reads, and project-scoped write authorization.
- Public Cache accepts writes only from Layer Cache's Public Builds identity.
- The public control plane records the exact source commit, Dockerfile and frontend, build parameters, target platform, output digest, cache manifest digest, and builder identity.
- The client asks that control plane for an approved cache manifest digest and imports `registry/repository@sha256:...`. It does not trust a public mutable tag.
- Layer Cache verifies its signed public-build provenance before returning that digest. BuildKit provenance attaches to build results, while the cache config schema has no provenance field, so the control plane must bind the signed build record to the cache manifest digest.

BuildKit supports SLSA provenance attestations for build results, and its provenance records include build parameters and materials. Secret values must enter through secret or SSH mounts. BuildKit promises not to persist secret values or include them in cache checksums, but a Dockerfile can still leak a secret copied through `ARG`, `ENV`, or the build context. [Build attestations](https://docs.docker.com/build/metadata/attestations/), [SLSA provenance](https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-provenance.md), [BuildKit stored-data guarantees](https://github.com/moby/buildkit/blob/master/PROJECT.md#stored-data), [Docker build secrets](https://docs.docker.com/build/building/secrets/).

## Public Builds and remote execution

Buildx's `remote` driver connects an unmodified client to an externally managed BuildKit daemon over TCP or another supported connection. It supports a CA certificate, client certificate, client key, and TLS server name. Docker warns that an unauthenticated TCP BuildKit endpoint grants arbitrary BuildKit access. Public Builds must require mutual TLS or an authenticated gateway that creates an equally strong per-request identity. [Buildx remote driver](https://docs.docker.com/build/builders/drivers/remote/).

The remote builder runs on Linux even when the caller uses macOS. `--push` keeps the result and cache transfer between cloud services. `--load` sends a single-platform result back into the local Docker daemon and can erase much of the offload benefit for a large image. A pinned remote Git context avoids uploading a large local build context and gives the public builder a reproducible source identity. These are routing choices exposed by the Layer Cache CLI, not changes to Buildx.

One shared `buildkitd` is not a tenant boundary. BuildKit documents that concurrent clients can share pulls, cache mounts, and other build resources without namespacing. Public Builds must place mutually untrusted customers or repositories in separate BuildKit trust domains. The exact worker isolation mechanism belongs to the public-build isolation decision. [BuildKit multi-client warning](https://github.com/moby/buildkit/blob/master/PROJECT.md#examples-of-issues-not-currently-considered-security).

## Telemetry contract

BuildKit exposes enough data for observed hit rate and elapsed time:

- `docker buildx history inspect --format json` reports build start, completion, duration, total steps, completed steps, and cached steps. [Buildx history inspect](https://docs.docker.com/reference/cli/docker/buildx/history/inspect/)
- The BuildKit solve stream exposes each vertex digest, name, `Cached` flag, start time, and completion time. `--progress=rawjson` makes that stream machine-readable in Buildx. [Solve status types](https://github.com/moby/buildkit/blob/master/client/graph.go), [Buildx progress modes](https://docs.docker.com/build/building/variables/#build-tool-configuration-variables)
- `buildctl --debug-json-cache-metrics` emits user cacheable, cached, and missed counts plus client duration. [Buildctl cache metrics](https://github.com/moby/buildkit/blob/master/cmd/buildctl/cachemetrics.go)

Two desired measurements are not native. The cache config stores `CreatedAt` but no producer execution duration. The solve stream marks a hit but does not identify which importer supplied it. Layer Cache must not label registry manifest or blob reads as hits because BuildKit may inspect several sources before choosing a record.

The adapter should record requested cache refs, target platform, vertex statuses, total wall time, cache import and export spans, and bytes served per Layer Cache registry scope. It can report observed overall vertex hit rate immediately. Net time saved remains an estimate based on historical uncached duration for the same task fingerprint minus current lookup, transfer, and restore overhead. Sampled replays on disposable empty builders provide the counterfactual needed to calibrate that estimate. Tier-level hit attribution requires later BuildKit instrumentation or controlled single-source experiments.

## Stock-client command shape

The wrapper can generate a build equivalent to:

```bash
docker buildx build \
  --builder layercache \
  --platform linux/amd64 \
  --cache-from type=registry,ref=cache.example/team/acme/widget/linux-amd64:main \
  --cache-from type=registry,ref=cache.example/public/widget/linux-amd64@sha256:<approved-manifest> \
  --cache-to type=registry,ref=cache.example/team/acme/widget/linux-amd64:<unique-build>,mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true \
  --load \
  .
```

BuildKit uses the named builder's internal state as Local Cache without another flag. The service serializes promotion of `<unique-build>` to a mutable branch ref. Public Cache has no `--cache-to`. For Public Builds, the wrapper selects a separately configured mutual-TLS `remote` builder and normally replaces `--load` with `--push`.
