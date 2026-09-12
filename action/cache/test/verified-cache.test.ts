import { createHash, generateKeyPairSync, sign } from "node:crypto";
import { createServer } from "node:http";
import { promises as fs } from "node:fs";
import {
  chmod as fsChmod,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  readlink as fsReadlink,
  rm,
  stat as fsStat,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Writable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { createGzip } from "node:zlib";

import * as tar from "tar-stream";
import { describe, expect, it, vi } from "vitest";

import { VerifiedCacheClient, type VerifiedCacheIdentity } from "../src/verified-cache.js";

const payloadType = "application/vnd.in-toto+json";

describe("verified Public Cache client", () => {
  it("verifies, validates, and extracts a signed archive through the v1 protocol", async () => {
    const workspace = await mkdtemp(join(tmpdir(), "layercache-workspace-"));
    const archive = await gzipArchive("node_modules/widget/index.js", "restored safely");
    let signedArchive = archive;
    let servedArchive = archive;
    let stallArchive = false;
    let lookupDelayMilliseconds = 0;
    let publicationCompatibility = "linux-amd64-schema1";
    let publicationInputs: Array<{ name: string; value: string }> | undefined;
    let publicationBuilderImageDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";
    const { publicKey, privateKey } = generateKeyPairSync("ed25519");
    const trustKey = publicKey.export({ format: "der", type: "spki" }).subarray(-32).toString("base64url");
    const identity: VerifiedCacheIdentity = {
      repository: "acme/widgets",
      ref: "refs/heads/main",
      commit: "0123456789abcdef0123456789abcdef01234567",
      compatibility: "linux-amd64-schema1",
      recipeDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      target: ".github/workflows/public-cache.yml#public-cache",
      platform: "linux/amd64",
      inputs: [{ name: "compatibility", value: "linux-amd64-schema1" }],
      toolchain: "actions/cache@6.2.0",
      builder: "layercache-public-builder@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    };
    const primaryKey = "pnpm-linux";
    let endpoint = "";
    const server = createServer(async (request, response) => {
      const target = new URL(request.url ?? "/", endpoint);
      if (target.pathname.endsWith("/_apis/artifactcache/cache")) {
        if (lookupDelayMilliseconds !== 0) {
          await new Promise((resolvePromise) => setTimeout(resolvePromise, lookupDelayMilliseconds));
          lookupDelayMilliseconds = 0;
        }
        const digest = createHash("sha256").update(signedArchive).digest("hex");
        const version = target.searchParams.get("version") ?? "";
        const exactInputs = actionsPublicInputs(identity, primaryKey, version);
        const nativeKey = actionsPublicNativeKey(identity, primaryKey, version);
        const publication = {
          integration: "actions",
          project: identity.repository,
          compatibility: publicationCompatibility,
          nativeKey,
          repository: `https://github.com/${identity.repository}`,
          commit: identity.commit,
          recipeDigest: identity.recipeDigest,
          target: identity.target,
          platform: identity.platform,
          inputs: publicationInputs ?? exactInputs,
          toolchain: identity.toolchain,
          builder: identity.builder,
          builderImageDigest: publicationBuilderImageDigest,
          digest,
          size: signedArchive.length,
          durationMs: 1,
          buildId: "build-123",
          issuedAt: new Date(Date.now() - 60_000).toISOString(),
          expiresAt: new Date(Date.now() + 60 * 60_000).toISOString(),
        };
        const publicIdentity = publicationIdentity(publication);
        const payload = Buffer.from(JSON.stringify({
          _type: "https://in-toto.io/Statement/v1",
          subject: [{ name: `layercache-public:${publicIdentity}`, digest: { sha256: digest } }],
          predicateType: "https://layercache.dev/attestation/public-cache/v1",
          predicate: publication,
        }));
        const signature = sign(null, pae(payloadType, payload), privateKey).toString("base64");
        const cacheIdentity = createHash("sha256").update([
          "actions", identity.repository, identity.compatibility, nativeKey,
        ].join("\0")).digest("hex");
        response.setHeader("Content-Type", "application/json");
        response.end(JSON.stringify({
          cacheKey: primaryKey,
          cacheVersion: version,
          archiveLocation: new URL("archive", endpoint).toString(),
          layerCacheOrigin: "publicCache",
          layerCachePublic: {
            request: {
              cacheIdentity,
              expected: {
                Integration: "actions",
                Project: identity.repository,
                Compatibility: publicationCompatibility,
                NativeKey: nativeKey,
                Target: identity.target,
                Inputs: exactInputs,
              },
              repository: identity.repository,
              sourceRepository: `https://github.com/${identity.repository}`,
              ref: identity.ref,
              key: primaryKey,
              version,
              compatibility: identity.compatibility,
              sourceCommit: identity.commit,
              recipeDigest: identity.recipeDigest,
              target: identity.target,
              platform: identity.platform,
              toolchain: identity.toolchain,
              builder: identity.builder,
            },
            envelope: { payloadType, payload: payload.toString("base64"), signatures: [{ keyid: "test", sig: signature }] },
            digest,
            size: signedArchive.length,
            expiresAt: publication.expiresAt,
            publicIdentity,
          },
        }));
        return;
      }
      if (target.pathname.endsWith("/archive")) {
        response.setHeader("Content-Type", "application/octet-stream");
        if (stallArchive) {
          response.write(servedArchive.subarray(0, 1));
          setTimeout(() => response.end(servedArchive.subarray(1)), 200);
        } else {
          response.end(servedArchive);
        }
        return;
      }
      response.statusCode = 404;
      response.end();
    });

    try {
      await new Promise<void>((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
      const address = server.address();
      if (address === null || typeof address === "string") {
        throw new Error("test server did not expose a TCP address");
      }
      endpoint = `http://127.0.0.1:${address.port}/v1/_layercache/compatibility/${identity.compatibility}/`;
      await mkdir(join(workspace, "node_modules/widget"), { recursive: true });
      await writeFile(join(workspace, "node_modules/widget/index.js"), "stale contents");
      await writeFile(join(workspace, "unrelated.txt"), "preserve me");
      const client = new VerifiedCacheClient({
        env: {
          ACTIONS_CACHE_URL: endpoint,
          ACTIONS_RUNTIME_TOKEN: "team-token",
          GITHUB_WORKSPACE: workspace,
        },
        trustKey,
        identity,
      });

      await expect(client.restoreCache(["node_modules"], primaryKey)).resolves.toBe(primaryKey);
      await expect(readFile(join(workspace, "node_modules/widget/index.js"), "utf8")).resolves.toBe("restored safely");
      await expect(readFile(join(workspace, "unrelated.txt"), "utf8")).resolves.toBe("preserve me");

      signedArchive = await gzipReadOnlyDirectoryArchive();
      servedArchive = signedArchive;
      await expect(client.restoreCache(["node_modules"], primaryKey)).resolves.toBe(primaryKey);
      await expect(readFile(join(workspace, "node_modules/readonly/data.txt"), "utf8")).resolves.toBe("read only");
      expect((await fsStat(join(workspace, "node_modules/readonly"))).mode & 0o777).toBe(0o555);

      signedArchive = await gzipSymlinkArchive();
      servedArchive = signedArchive;
      await expect(client.restoreCache(["node_modules"], primaryKey)).resolves.toBe(primaryKey);
      await expect(fsReadlink(join(workspace, "node_modules/current.js"))).resolves.toBe("widget/index.js");

      signedArchive = await gzipHardlinkArchive();
      servedArchive = signedArchive;
      await expect(client.restoreCache(["node_modules"], primaryKey)).resolves.toBe(primaryKey);
      const hardlinkTarget = await fsStat(join(workspace, "node_modules/widget/index.js"));
      const hardlink = await fsStat(join(workspace, "node_modules/current-hardlink.js"));
      expect([hardlink.dev, hardlink.ino]).toEqual([hardlinkTarget.dev, hardlinkTarget.ino]);

      const rollbackParent = join(workspace, "node_modules/rollback-parent");
      const rollbackChild = join(rollbackParent, "child");
      await mkdir(join(rollbackChild, "conflict"), { recursive: true });
      await fsChmod(rollbackChild, 0o300);
      await fsChmod(rollbackParent, 0o100);
      signedArchive = await gzipRollbackConflictArchive();
      servedArchive = signedArchive;
      const chmod = vi.spyOn(fs, "chmod");
      try {
        await expect(client.restoreCache(["node_modules"], primaryKey))
          .rejects.toThrow("would replace an existing directory");
        const restoredDirectories = chmod.mock.calls
          .filter(([path, mode]) =>
            path === rollbackChild && mode === 0o300 || path === rollbackParent && mode === 0o100)
          .map(([path]) => path);
        expect(restoredDirectories).toEqual([rollbackChild, rollbackParent]);
      } finally {
        chmod.mockRestore();
        await fsChmod(rollbackChild, 0o700);
        await fsChmod(rollbackParent, 0o700);
      }

      signedArchive = archive;
      servedArchive = archive;

      lookupDelayMilliseconds = 100;
      await expect(client.restoreCache(["node_modules"], primaryKey)).resolves.toBe(primaryKey);

      publicationCompatibility = "darwin-arm64-schema1";
      await expect(client.restoreCache(["node_modules"], primaryKey)).rejects.toThrow("expected Actions identity");
      publicationCompatibility = identity.compatibility;

      publicationInputs = [{ name: "compatibility", value: "linux-amd64-schema2" }];
      await expect(client.restoreCache(["node_modules"], primaryKey)).rejects.toThrow("expected Actions identity");
      publicationInputs = undefined;

      publicationBuilderImageDigest = "sha256:invalid";
      await expect(client.restoreCache(["node_modules"], primaryKey)).rejects.toThrow("output identity");
      publicationBuilderImageDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";

      await writeFile(join(workspace, "node_modules/widget/index.js"), "sentinel after success");
      servedArchive = Buffer.from(archive);
      servedArchive[servedArchive.length - 1] = (servedArchive.at(-1) ?? 0) ^ 0xff;
      await expect(client.restoreCache(["node_modules"], primaryKey)).rejects.toThrow("signed publication");
      await expect(readFile(join(workspace, "node_modules/widget/index.js"), "utf8"))
        .resolves.toBe("sentinel after success");

      signedArchive = await gzipArchive("../layercache-escape.txt", "must not escape");
      servedArchive = signedArchive;
      await expect(client.restoreCache(["node_modules"], primaryKey)).rejects.toThrow("escapes the workspace");
      await expect(readFile(join(workspace, "node_modules/widget/index.js"), "utf8"))
        .resolves.toBe("sentinel after success");

      signedArchive = archive;
      servedArchive = archive;
      stallArchive = true;
      await expect(client.restoreCache(["node_modules"], primaryKey, [], { segmentTimeoutInMs: 50 }))
        .rejects.toThrow("made no progress");
      await expect(readFile(join(workspace, "node_modules/widget/index.js"), "utf8"))
        .resolves.toBe("sentinel after success");
      expect((await readdir(workspace)).filter((name) => name.startsWith(".layercache-restore-"))).toEqual([]);
    } finally {
      await new Promise<void>((resolvePromise, reject) => server.close((error) => error ? reject(error) : resolvePromise()));
      await fsChmod(join(workspace, "node_modules/readonly"), 0o755).catch(() => undefined);
      await rm(workspace, { recursive: true, force: true });
    }
  });
});

async function gzipArchive(name: string, contents: string): Promise<Buffer> {
  const chunks: Buffer[] = [];
  const pack = tar.pack();
  const completed = pipeline(pack, createGzip(), new Writable({
    write(chunk: Buffer, _encoding, callback) {
      chunks.push(Buffer.from(chunk));
      callback();
    },
  }));
  pack.entry({ name, mode: 0o644 }, contents);
  pack.finalize();
  await completed;
  return Buffer.concat(chunks);
}

async function gzipReadOnlyDirectoryArchive(): Promise<Buffer> {
  const chunks: Buffer[] = [];
  const pack = tar.pack();
  const completed = pipeline(pack, createGzip(), new Writable({
    write(chunk: Buffer, _encoding, callback) {
      chunks.push(Buffer.from(chunk));
      callback();
    },
  }));
  pack.entry({ name: "node_modules/readonly", type: "directory", mode: 0o555 });
  pack.entry({ name: "node_modules/readonly/data.txt", mode: 0o444 }, "read only");
  pack.finalize();
  await completed;
  return Buffer.concat(chunks);
}

async function gzipSymlinkArchive(): Promise<Buffer> {
  const chunks: Buffer[] = [];
  const pack = tar.pack();
  const completed = pipeline(pack, createGzip(), new Writable({
    write(chunk: Buffer, _encoding, callback) {
      chunks.push(Buffer.from(chunk));
      callback();
    },
  }));
  pack.entry({ name: "node_modules/widget/index.js", mode: 0o644 }, "linked contents");
  pack.entry({
    name: "node_modules/current.js",
    type: "symlink",
    linkname: "widget/index.js",
  });
  pack.finalize();
  await completed;
  return Buffer.concat(chunks);
}

async function gzipHardlinkArchive(): Promise<Buffer> {
  const chunks: Buffer[] = [];
  const pack = tar.pack();
  const completed = pipeline(pack, createGzip(), new Writable({
    write(chunk: Buffer, _encoding, callback) {
      chunks.push(Buffer.from(chunk));
      callback();
    },
  }));
  pack.entry({ name: "node_modules/widget/index.js", mode: 0o644 }, "linked contents");
  pack.entry({
    name: "node_modules/current-hardlink.js",
    type: "link",
    linkname: "node_modules/widget/index.js",
  });
  pack.finalize();
  await completed;
  return Buffer.concat(chunks);
}

async function gzipRollbackConflictArchive(): Promise<Buffer> {
  const chunks: Buffer[] = [];
  const pack = tar.pack();
  const completed = pipeline(pack, createGzip(), new Writable({
    write(chunk: Buffer, _encoding, callback) {
      chunks.push(Buffer.from(chunk));
      callback();
    },
  }));
  pack.entry({ name: "node_modules/rollback-parent", type: "directory", mode: 0o755 });
  pack.entry({ name: "node_modules/rollback-parent/child", type: "directory", mode: 0o755 });
  pack.entry({ name: "node_modules/rollback-parent/child/conflict", mode: 0o644 }, "replacement");
  pack.finalize();
  await completed;
  return Buffer.concat(chunks);
}

function actionsPublicNativeKey(identity: VerifiedCacheIdentity, key: string, version: string): string {
  const hasher = createHash("sha256");
  for (const value of [
    "layercache/actions-cache/public-identity/v1", identity.repository, identity.ref, key, version,
    identity.compatibility, identity.commit, identity.recipeDigest, identity.platform, identity.toolchain,
    identity.builder,
  ]) {
    const length = Buffer.alloc(8);
    length.writeBigUInt64BE(BigInt(Buffer.byteLength(value)));
    hasher.update(length);
    hasher.update(value);
  }
  return `sha256:${hasher.digest("hex")}`;
}

function actionsPublicInputs(identity: VerifiedCacheIdentity, key: string, version: string) {
  return [
    { name: "actions.key", value: key },
    { name: "actions.ref", value: identity.ref },
    { name: "actions.version", value: version },
    { name: "compatibility", value: identity.compatibility },
  ];
}

function publicationIdentity(
  publication: Record<string, string | number | Array<{ name: string; value: string }>>,
): string {
  const inputs = publication.inputs as Array<{ name: string; value: string }>;
  const parts = [
    publication.integration, publication.project, publication.compatibility, publication.nativeKey,
    publication.repository, publication.commit, publication.recipeDigest, publication.target,
    publication.platform,
  ];
  for (const input of inputs) {
    parts.push(input.name, input.value);
  }
  parts.push(publication.toolchain, publication.builder, publication.builderImageDigest, publication.buildId,
    publication.digest, String(publication.size),
  );
  return createHash("sha256").update(parts.join("\0")).digest("hex");
}

function pae(type: string, payload: Buffer): Buffer {
  return Buffer.concat([Buffer.from(`DSSEv1 ${Buffer.byteLength(type)} ${type} ${payload.length} `), payload]);
}
