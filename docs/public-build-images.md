# Public Build images and native qualification

The maintained Actions, Turbo, and BuildKit recipes can assemble and qualify
Linux amd64 and Linux arm64 guests. Each architecture has its own reviewed
contract. An arm64 worker uses `qemu-system-aarch64`, the `virt` machine, KVM and
the PL011 console. An amd64 worker uses `qemu-system-x86_64` and `microvm`.
Neither worker falls back to software emulation.

The assembly scripts are qualification recipes for Debian or Ubuntu hosts.
They are not yet a hermetic production image factory. They copy native distro
libraries and filesystem tools from the host, record every copied file's digest
and package versions, and hash the resulting image. Ext4 metadata can differ
between assemblies. A successful cross-build does not prove a guest boots or
that a native build passes.

## Run the native gate

Use a dedicated Linux machine of the architecture being qualified. It needs
working `/dev/kvm`, cgroup v2 with CPU, memory and pids controllers, the pinned
Go version from `mise.toml`, QEMU, Docker, `jq`, `curl`, `xz-utils`, `binutils`,
`busybox-static`, `e2fsprogs`, Bash, Dash, and the native glibc/C++ libraries.
The scripts require noninteractive `sudo`; QEMU itself runs as a dedicated
non-root UID with access through the KVM group. Do not run the gate on an
unreviewed pull request on a persistent privileged worker.

Provide these operator-owned inputs before running it:

- A reviewed native kernel and its previously recorded SHA-256. The arm64
  input is an uncompressed Linux `Image` supported by QEMU direct boot, even
  though the installed asset is called `vmlinuz`. Required drivers must be
  built in because these guests do not load an initramfs or kernel modules.
  Include ext4, devtmpfs, virtio MMIO/block/console/rng, cgroup v2/pids, and the
  architecture's serial console. BuildKit also needs namespaces, seccomp and
  its native OCI worker's filesystem support.
- A delegated cgroup directory beneath `/sys/fs/cgroup` whose
  `cgroup.subtree_control` already enables `cpu memory pids`. The gate creates
  its own child and does not enable controllers in unrelated cgroups.
- A unique disposable OCI registry repository. A fresh loopback registry
  avoids external credentials. The trusted host pushes and pulls the graph;
  the guest never receives the registry address or credentials.
- A new absolute evidence-directory path. The gate refuses to replace it.

On the arm64 machine, with those paths and values filled in:

```bash
mise exec -- bash qa/public-build-kvm/run-native.sh arm64 \
  "$LAYER_CACHE_QA_KERNEL" "$LAYER_CACHE_QA_KERNEL_SHA256" \
  "$LAYER_CACHE_QA_CGROUP" "$LAYER_CACHE_QA_REGISTRY_REPOSITORY" \
  "$LAYER_CACHE_QA_EVIDENCE_DIRECTORY"
```

Use `amd64` for the equivalent x64 gate. A cheap prerequisite check is
`bash qa/public-build-kvm/run-native.sh --check arm64`; it fails on an x64
machine before creating files, downloading tools or running privileged work.
It does not replace execution of the full gate.

The full gate assembles fresh guest assets and compiles the guest agents and
QA programs from the current checkout. It then:

1. Executes the maintained Actions workflow twice and compares the native
   key, collected digest and exact archive members.
2. Executes the Turbo task twice, compares its native keys, digests and
   members, and restores both results through the genuine Turbo client.
3. Runs a real Dockerfile `RUN` through BuildKit, publishes its complete OCI
   graph, pulls the immutable digest, and verifies descriptors and contents.
   It does not require byte-identical BuildKit graphs across executions;
   native cache metadata includes execution timestamps.
4. Checks every report's native platform, reviewed kernel digest, offline
   dependency contract, absence of a NIC, collected digest and native key.

Evidence includes the checkout revision and dirty status, hashes covering new
and tracked Public Build source files, native QA binaries, assembly logs,
guest file inventories, package versions, contracts and JSON reports. Reports
bind the kernel, root filesystem and contract through the existing composite
`builderImageDigest`. The gate removes its own temporary assets and empty
cgroup. If cgroup cleanup fails, it preserves assets and exits unsuccessfully.
The caller owns the evidence and disposable registry cleanup.

The manual GitHub workflow `.github/workflows/public-build-native.yml` runs
this gate on a reviewed `main` commit with the labels `self-hosted`, `Linux`,
`ARM64` and `layercache-kvm`. Configure repository variables
`LAYERCACHE_QA_ARM64_KERNEL`, `LAYERCACHE_QA_ARM64_KERNEL_SHA256`,
`LAYERCACHE_QA_CGROUP_PARENT` and `LAYERCACHE_QA_REGISTRY_REPOSITORY` to those
operator-owned inputs. The workflow treats the registry value as a repository
prefix and appends a unique run ID and attempt. Its worker must have the required tools and pinned Go
version already installed. It uploads the evidence for 30 days. Preserve the
qualified release's evidence longer in the image release before that expiry.
The workflow is dispatched explicitly; its mere presence is not evidence of
a successful native arm64 run.

To assemble one guest independently, use
`qa/public-build-kvm/build-actions-arm64-rootfs.sh ASSET_ROOT KERNEL` or the
corresponding `turbo`/`buildkit` and `amd64` script. The original amd64 entry
points remain supported.

## Dependency pins

`qa/public-build-kvm/native-platform.sh` fixes the following inputs separately
for amd64 and arm64:

- Node.js 24.13.0 archive SHA-256, covering both Node and npm 11.6.2. Pins come
  from the [Node release checksum file](https://nodejs.org/dist/v24.13.0/SHASUMS256.txt).
- Turbo 2.10.12 package SHA-512. The platform archives are published as
  [`@turbo/linux-64`](https://registry.npmjs.org/@turbo/linux-64/2.10.12) and
  [`@turbo/linux-arm64`](https://registry.npmjs.org/@turbo/linux-arm64/2.10.12).
- The architecture-specific OCI manifests of
  [BuildKit v0.32.2](https://github.com/moby/buildkit/releases/tag/v0.32.2).
  They are members of the pinned image index
  `sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8`.
  Docker only extracts the image's files; it does not start the image during
  root filesystem assembly.

Downloads verify the pinned complete archive before extraction. The recipes
reject a foreign host architecture before executing Node or any other guest
tool. The QA programs also inspect ELF architecture before using the Turbo
binary or copying static BusyBox into BuildKit's scratch fixture.

## Promote production images

A production image factory still needs to pin the distro and kernel build
inputs. Use a fixed Debian/Ubuntu package snapshot or an image digest, retain
the exact package files and kernel source/config/compiler, and build with a
fixed Go version and dependency graph. Normalize filesystem metadata and
timestamps before comparing independent assemblies. Retain both input
manifests and output digests so a difference can be traced to a file or tool.

Keep assembly and qualification separate from publication credentials. After
both architectures pass native qualification, review the contract's admitted
integration/target/recipe tuples and install the exact immutable kernel,
rootfs and contract bytes on workers. Configure all three SHA-256 pins. The
worker computes its composite builder-image digest from those values; using
only the rootfs digest would omit the executable kernel and admission policy.

Attest and archive the source revision, tool/package inputs, kernel config,
three asset digests, composite digest and native qualification reports in
the image release. Configure the production publisher's approved image
identity and preserve revocation/retirement history during rollout. Do not
silently replace bytes beneath an existing approved digest.

The current development machine is x86_64. Linux arm64 contracts, assembly,
platform selection and cross-builds can be checked here, but native arm64
KVM execution requires an arm64 machine with virtualization support. Keep
that native gate open until its reports exist. A hosted arm64 client test
without usable KVM does not close the Public Build qualification gate.
