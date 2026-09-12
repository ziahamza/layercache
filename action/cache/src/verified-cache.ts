import type { DownloadOptions, UploadOptions } from "@actions/cache";
import * as stockCache from "@actions/cache";
import {
  createHash,
  createPublicKey,
  randomUUID,
  verify as verifySignature,
  type KeyObject,
} from "node:crypto";
import {
  createReadStream,
  createWriteStream,
  promises as fs,
} from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, posix, relative, resolve, sep } from "node:path";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { createGunzip } from "node:zlib";
import { pipeline } from "node:stream/promises";
import { Readable, Transform } from "node:stream";
import * as tar from "tar-stream";

const payloadType = "application/vnd.in-toto+json";
const statementType = "https://in-toto.io/Statement/v1";
const predicateType = "https://layercache.dev/attestation/public-cache/v1";
const ed25519SPKIPrefix = Buffer.from("302a300506032b6570032100", "hex");
const maxArchiveMembers = 1_000_000;
const maxArchiveNameBytes = 4 << 10;
const maxExpandedArchiveBytes = 100 * 1024 * 1024 * 1024;
const maxArchiveBytes = 10 * 1024 * 1024 * 1024;
const defaultLookupTimeoutMilliseconds = 2_000;
const archiveResponseTimeoutMilliseconds = 2_000;
const downloadIdleTimeoutMilliseconds = 30_000;

export interface VerifiedCacheIdentity {
  repository: string;
  ref: string;
  commit: string;
  compatibility: string;
  recipeDigest: string;
  target: string;
  platform: string;
  inputs: DeclaredInput[];
  toolchain: string;
  builder: string;
}

export interface DeclaredInput {
  name: string;
  value: string;
}

export interface VerifiedCacheOptions {
  env: NodeJS.ProcessEnv;
  trustKey: string;
  identity: VerifiedCacheIdentity;
  lookupTimeoutMilliseconds?: number;
}

export class VerifiedCacheInputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "VerifiedCacheInputError";
  }
}

export class VerifiedCacheSafetyError extends AggregateError {
  constructor(errors: unknown[], staging: string) {
    super(errors, `Actions cache restore rollback was incomplete; recovery data remains at ${staging}.`);
    this.name = "VerifiedCacheSafetyError";
  }
}

interface Signature {
  keyid: string;
  sig: string;
}

interface Envelope {
  payloadType: string;
  payload: string;
  signatures: Signature[];
}

interface Publication {
  integration: string;
  project: string;
  compatibility: string;
  nativeKey: string;
  repository: string;
  commit: string;
  recipeDigest: string;
  target: string;
  platform: string;
  inputs: DeclaredInput[];
  toolchain: string;
  builder: string;
  builderImageDigest: string;
  buildId: string;
  digest: string;
  size: number;
  durationMs: number;
  issuedAt: string;
  expiresAt: string;
}

interface PublicResolveRequest {
  cacheIdentity: string;
  expected: {
    Integration: string;
    Project: string;
    Compatibility: string;
    NativeKey: string;
    Target: string;
    Inputs: DeclaredInput[];
  };
  repository: string;
  sourceRepository: string;
  ref: string;
  key: string;
  version: string;
  compatibility: string;
  sourceCommit: string;
  recipeDigest: string;
  target: string;
  platform: string;
  toolchain: string;
  builder: string;
}

interface PublicMetadata {
  request: PublicResolveRequest;
  envelope: Envelope;
  digest: string;
  size: number;
  expiresAt: string;
  publicIdentity: string;
}

interface LookupResponse {
  cacheKey: string;
  cacheVersion: string;
  archiveLocation: string;
  layerCacheOrigin?: string;
  layerCachePublic?: PublicMetadata;
}

interface Statement {
  _type: string;
  subject: Array<{ name: string; digest: Record<string, string> }>;
  predicateType: string;
  predicate: Publication;
}

interface ArchiveMember {
  type: string;
  linkTarget: string | undefined;
}

export class VerifiedCacheClient {
  readonly #env: NodeJS.ProcessEnv;
  readonly #key: KeyObject;
  readonly #identity: VerifiedCacheIdentity;
  readonly #lookupTimeoutMilliseconds: number;

  constructor(options: VerifiedCacheOptions) {
    this.#env = options.env;
    this.#identity = {
      ...options.identity,
      inputs: validateDeclaredInputs(options.identity.inputs),
    };
    this.#key = decodeTrustKey(options.trustKey);
    this.#lookupTimeoutMilliseconds = options.lookupTimeoutMilliseconds ?? defaultLookupTimeoutMilliseconds;
    if (!Number.isSafeInteger(this.#lookupTimeoutMilliseconds) || this.#lookupTimeoutMilliseconds < 1_000 ||
        this.#lookupTimeoutMilliseconds > 60 * 60_000) {
      throw new VerifiedCacheInputError("Verified Public Cache lookup timeout must be between 1 and 3600 seconds.");
    }
  }

  async restoreCache(
    paths: string[],
    primaryKey: string,
    restoreKeys: string[] = [],
    options: DownloadOptions = {},
  ): Promise<string | undefined> {
    validateCacheRequest(paths, primaryKey, restoreKeys);
    const endpoint = requiredEnvironment(this.#env, "ACTIONS_CACHE_URL");
    const token = requiredEnvironment(this.#env, "ACTIONS_RUNTIME_TOKEN");
    const workspace = await verifiedWorkspace(this.#env);
    validateRequestedPaths(paths, workspace);
    const version = cacheVersion(paths);
    const target = new URL("_apis/artifactcache/cache", endpoint);
    target.searchParams.set("keys", [primaryKey, ...restoreKeys].join(","));
    target.searchParams.set("version", version);
    const requestInit: RequestInit = {
      headers: { Authorization: `Bearer ${token}` },
      redirect: "error",
      signal: AbortSignal.timeout(this.#lookupTimeoutMilliseconds),
    };
    const response = await fetch(target, requestInit);
    if (response.status === 204 || response.status === 404) {
      return undefined;
    }
    if (!response.ok) {
      throw new Error(`Layer Cache lookup returned HTTP ${response.status}.`);
    }
    const lookup = (await response.json()) as LookupResponse;
    if (lookup.cacheVersion !== version || lookup.cacheKey === "" || lookup.archiveLocation === "") {
      throw new Error("Layer Cache returned an invalid v1 lookup response.");
    }

    let publication: Publication | undefined;
    if (lookup.layerCacheOrigin === "publicCache") {
      if (lookup.cacheKey !== primaryKey) {
        throw new Error("Public Cache restores require an exact primary-key match.");
      }
      if (lookup.layerCachePublic === undefined) {
        throw new Error("Public Cache response omitted signed publication metadata.");
      }
      publication = verifyPublicMetadata(
        this.#key,
        lookup.layerCachePublic,
        this.#identity,
        lookup.cacheKey,
        version,
      );
    }
    if (options.lookupOnly) {
      return lookup.cacheKey;
    }

    const directory = await mkdtemp(join(tmpdir(), "layercache-actions-"));
    const archivePath = join(directory, "cache.archive");
    try {
      const archiveURL = new URL(lookup.archiveLocation, endpoint);
      const endpointURL = new URL(endpoint);
      if (archiveURL.origin !== endpointURL.origin || archiveURL.username !== "" || archiveURL.password !== "") {
        throw new Error("Layer Cache returned an archive URL outside the configured endpoint origin.");
      }
      const downloadController = new AbortController();
      const responseTimeout = setTimeout(
        () => downloadController.abort(new Error("Layer Cache archive response timed out.")),
        archiveResponseTimeoutMilliseconds,
      );
      let download: Response;
      try {
        download = await fetchSameOrigin(archiveURL, endpointURL, downloadController.signal);
      } catch (error) {
        if (downloadController.signal.aborted) {
          throw new Error("Layer Cache archive response timed out.");
        }
        throw error;
      } finally {
        clearTimeout(responseTimeout);
      }
      if (!download.ok || download.body === null) {
        await download.body?.cancel();
        throw new Error(`Layer Cache archive download returned HTTP ${download.status}.`);
      }
      const downloaded = await streamDownload(
        download.body,
        archivePath,
        publication?.size ?? maxArchiveBytes,
        options.segmentTimeoutInMs ?? downloadIdleTimeoutMilliseconds,
        downloadController,
      );
      if (publication !== undefined &&
          (downloaded.size !== publication.size || downloaded.digest !== publication.digest)) {
        throw new Error("Public Cache archive bytes do not match the signed publication.");
      }
      const members = await validateArchive(archivePath, downloaded.size);
      await rejectExistingSymlinkParents(workspace, members);
      const staging = await mkdtemp(join(workspace, ".layercache-restore-"));
      let preserveStaging = false;
      try {
        await extractArchive(archivePath, staging);
        await applyExtractedArchive(staging, workspace, members);
      } catch (error) {
        preserveStaging = error instanceof VerifiedCacheSafetyError;
        throw error;
      } finally {
        if (!preserveStaging) {
          await removeExtractedStaging(staging);
        }
      }
      return lookup.cacheKey;
    } finally {
      await rm(directory, { force: true, recursive: true });
    }
  }

  saveCache(paths: string[], key: string, options?: UploadOptions): Promise<number> {
    return stockCache.saveCache(paths, key, options);
  }
}

async function removeExtractedStaging(root: string): Promise<void> {
  const makeDirectoriesWritable = async (directory: string): Promise<void> => {
    await fs.chmod(directory, 0o700);
    for (const entry of await fs.readdir(directory, { withFileTypes: true })) {
      if (entry.isDirectory()) {
        await makeDirectoriesWritable(join(directory, entry.name));
      }
    }
  };
  try {
    await makeDirectoriesWritable(root);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
      throw error;
    }
  }
  await rm(root, { force: true, recursive: true });
}

async function fetchSameOrigin(initial: URL, endpoint: URL, signal: AbortSignal): Promise<Response> {
  let target = initial;
  for (let redirects = 0; redirects <= 3; redirects += 1) {
    if (target.origin !== endpoint.origin || target.username !== "" || target.password !== "") {
      throw new Error("Layer Cache archive redirect changed endpoint origin.");
    }
    const response = await fetch(target, { redirect: "manual", signal });
    if (![301, 302, 303, 307, 308].includes(response.status)) {
      return response;
    }
    const location = response.headers.get("location");
    if (location === null || redirects === 3) {
      await response.body?.cancel();
      throw new Error("Layer Cache archive returned an invalid or excessive redirect chain.");
    }
    const next = new URL(location, target);
    await response.body?.cancel();
    target = next;
  }
  throw new Error("Layer Cache archive returned an excessive redirect chain.");
}

function decodeTrustKey(encoded: string): KeyObject {
  if (!/^[A-Za-z0-9_-]{43}$/.test(encoded)) {
    throw new Error("Public Cache trust key must be a raw URL-safe base64 Ed25519 key.");
  }
  const raw = Buffer.from(encoded, "base64url");
  if (raw.length !== 32) {
    throw new Error("Public Cache trust key must contain 32 bytes.");
  }
  return createPublicKey({ key: Buffer.concat([ed25519SPKIPrefix, raw]), format: "der", type: "spki" });
}

function verifyPublicMetadata(
  key: KeyObject,
  metadata: PublicMetadata,
  expected: VerifiedCacheIdentity,
  cacheKey: string,
  version: string,
): Publication {
  const envelope = metadata.envelope;
  if (envelope.payloadType !== payloadType || envelope.signatures.length === 0) {
    throw new Error("Public Cache publication has an invalid DSSE envelope.");
  }
  const payload = Buffer.from(envelope.payload, "base64");
  const signed = pae(envelope.payloadType, payload);
  if (!envelope.signatures.some((candidate) =>
    verifySignature(null, signed, key, Buffer.from(candidate.sig, "base64")))) {
    throw new Error("Public Cache publication signature is invalid.");
  }
  const statement = JSON.parse(payload.toString("utf8")) as Statement;
  const publication = statement.predicate;
  const publicationInputs = validateDeclaredInputs(publication.inputs);
  const identity = publicationIdentity(publication);
  if (statement._type !== statementType || statement.predicateType !== predicateType ||
      statement.subject.length !== 1 || statement.subject[0]?.name !== `layercache-public:${identity}` ||
      statement.subject[0]?.digest.sha256 !== publication.digest) {
    throw new Error("Public Cache publication statement is invalid.");
  }
  if (metadata.publicIdentity !== identity || metadata.digest !== publication.digest ||
      metadata.size !== publication.size || metadata.expiresAt !== publication.expiresAt) {
    throw new Error("Public Cache metadata does not match its signed publication.");
  }
  const now = Date.now();
  const issuedAt = Date.parse(publication.issuedAt);
  const expiresAt = Date.parse(publication.expiresAt);
  if (!Number.isFinite(issuedAt) || !Number.isFinite(expiresAt) || issuedAt > now || now > expiresAt ||
      expiresAt <= issuedAt) {
    throw new Error("Public Cache publication lease is expired or not yet valid.");
  }
  const request = metadata.request;
  const expectedRepository = `https://github.com/${expected.repository}`;
  const expectedInputs = actionsPublicInputs(expected, cacheKey, version);
  if (publication.integration !== "actions" || publication.project !== expected.repository ||
      publication.compatibility !== expected.compatibility ||
      publication.repository !== expectedRepository || publication.commit !== expected.commit ||
      publication.recipeDigest !== expected.recipeDigest || publication.target !== expected.target ||
      publication.platform !== expected.platform || publication.toolchain !== expected.toolchain ||
      publication.builder !== expected.builder || request.repository !== expected.repository ||
      request.sourceRepository !== expectedRepository || request.sourceCommit !== expected.commit ||
      request.ref !== expected.ref || request.compatibility !== expected.compatibility ||
      request.recipeDigest !== expected.recipeDigest || request.target !== expected.target ||
      request.platform !== expected.platform ||
      request.toolchain !== expected.toolchain || request.builder !== expected.builder ||
      request.key !== cacheKey || request.version !== version || request.expected.NativeKey !== publication.nativeKey ||
      request.expected.Integration !== publication.integration || request.expected.Project !== publication.project ||
      request.expected.Compatibility !== publication.compatibility ||
      request.expected.Target !== expected.target ||
      !declaredInputsEqual(publicationInputs, expectedInputs) ||
      !declaredInputsEqual(validateDeclaredInputs(request.expected.Inputs), expectedInputs)) {
    throw new Error("Public Cache publication does not match the expected Actions identity.");
  }
  const nativeKey = actionsPublicNativeKey(expected, cacheKey, version);
  const cacheIdentity = createHash("sha256").update([
    "actions", expected.repository, expected.compatibility, nativeKey,
  ].join("\0")).digest("hex");
  if (publication.nativeKey !== nativeKey || request.cacheIdentity !== cacheIdentity) {
    throw new Error("Public Cache publication uses a different native cache coordinate.");
  }
  if (typeof publication.buildId !== "string" || publication.buildId === "" ||
      typeof publication.builderImageDigest !== "string" ||
      !/^sha256:[0-9a-f]{64}$/.test(publication.builderImageDigest) ||
      !/^[0-9a-f]{64}$/.test(publication.digest) || !Number.isSafeInteger(publication.size) ||
      publication.size < 0 || publication.size > maxArchiveBytes ||
      !Number.isSafeInteger(publication.durationMs) || publication.durationMs < 0) {
    throw new Error("Public Cache publication output identity is invalid.");
  }
  return publication;
}

function actionsPublicInputs(
  identity: VerifiedCacheIdentity,
  key: string,
  version: string,
): DeclaredInput[] {
  return [
    { name: "actions.key", value: key },
    { name: "actions.ref", value: identity.ref },
    { name: "actions.version", value: version },
    { name: "compatibility", value: identity.compatibility },
  ];
}

function actionsPublicNativeKey(identity: VerifiedCacheIdentity, key: string, version: string): string {
  const hasher = createHash("sha256");
  for (const value of [
    "layercache/actions-cache/public-identity/v1",
    identity.repository,
    identity.ref,
    key,
    version,
    identity.compatibility,
    identity.commit,
    identity.recipeDigest,
    identity.platform,
    identity.toolchain,
    identity.builder,
  ]) {
    const length = Buffer.alloc(8);
    length.writeBigUInt64BE(BigInt(Buffer.byteLength(value)));
    hasher.update(length);
    hasher.update(value);
  }
  return `sha256:${hasher.digest("hex")}`;
}

function publicationIdentity(publication: Publication): string {
  const parts: string[] = [
    publication.integration,
    publication.project,
    publication.compatibility,
    publication.nativeKey,
    publication.repository,
    publication.commit,
    publication.recipeDigest,
    publication.target,
    publication.platform,
  ];
  for (const input of validateDeclaredInputs(publication.inputs)) {
    parts.push(input.name, input.value);
  }
  parts.push(
    publication.toolchain,
    publication.builder,
    publication.builderImageDigest,
    publication.buildId,
    publication.digest,
    String(publication.size),
  );
  return createHash("sha256").update(parts.join("\0")).digest("hex");
}

function validateDeclaredInputs(value: unknown): DeclaredInput[] {
  if (!Array.isArray(value) || value.length > 32) {
    throw new Error("Public Cache publication declared inputs are invalid.");
  }
  let totalBytes = 0;
  let previous = "";
  const inputs: DeclaredInput[] = [];
  for (const candidate of value) {
    if (typeof candidate !== "object" || candidate === null ||
        !("name" in candidate) || typeof candidate.name !== "string" ||
        !("value" in candidate) || typeof candidate.value !== "string" ||
        !/^[a-z][a-z0-9._-]{0,63}$/.test(candidate.name) || candidate.name <= previous ||
        Buffer.byteLength(candidate.value) > 1024 || /[\0\r\n\x01-\x1f\x7f]/.test(candidate.value)) {
      throw new Error("Public Cache publication declared inputs are invalid.");
    }
    totalBytes += Buffer.byteLength(candidate.name) + Buffer.byteLength(candidate.value);
    if (totalBytes > 16 << 10) {
      throw new Error("Public Cache publication declared inputs are invalid.");
    }
    inputs.push({ name: candidate.name, value: candidate.value });
    previous = candidate.name;
  }
  return inputs;
}

function declaredInputsEqual(left: DeclaredInput[], right: DeclaredInput[]): boolean {
  return left.length === right.length && left.every((input, index) =>
    input.name === right[index]?.name && input.value === right[index]?.value);
}

function pae(type: string, payload: Buffer): Buffer {
  return Buffer.concat([
    Buffer.from(`DSSEv1 ${Buffer.byteLength(type)} ${type} ${payload.length} `),
    payload,
  ]);
}

function cacheVersion(paths: string[]): string {
  const compression = spawnSync("zstd", ["--quiet", "--version"], { stdio: "ignore" }).status === 0
    ? "zstd-without-long"
    : "gzip";
  return createHash("sha256").update([...paths, compression, "1.0"].join("|")).digest("hex");
}

async function streamDownload(
  body: ReadableStream<Uint8Array>,
  destination: string,
  maximumSize: number,
  idleTimeoutMilliseconds: number,
  controller: AbortController,
): Promise<{ digest: string; size: number }> {
  if (!Number.isSafeInteger(idleTimeoutMilliseconds) || idleTimeoutMilliseconds <= 0) {
    throw new Error("Layer Cache archive idle timeout must be a positive integer.");
  }
  const hasher = createHash("sha256");
  let size = 0;
  let idleTimeout: NodeJS.Timeout | undefined;
  const resetIdleTimeout = (): void => {
    if (idleTimeout !== undefined) {
      clearTimeout(idleTimeout);
    }
    idleTimeout = setTimeout(
      () => controller.abort(new Error("Layer Cache archive download made no progress.")),
      idleTimeoutMilliseconds,
    );
  };
  const meter = new Transform({
    transform(chunk: Buffer, _encoding, callback) {
      resetIdleTimeout();
      size += chunk.length;
      if (size > maximumSize) {
        callback(new Error("Layer Cache archive exceeds the allowed size."));
        return;
      }
      hasher.update(chunk);
      callback(null, chunk);
    },
  });
  resetIdleTimeout();
  try {
    await pipeline(ReadableFromWeb(body), meter, createWriteStream(destination, { mode: 0o600 }));
  } catch (error) {
    if (controller.signal.aborted) {
      throw new Error("Layer Cache archive download made no progress.");
    }
    throw error;
  } finally {
    if (idleTimeout !== undefined) {
      clearTimeout(idleTimeout);
    }
  }
  return { digest: hasher.digest("hex"), size };
}

function ReadableFromWeb(body: ReadableStream<Uint8Array>): Readable {
  return Readable.fromWeb(body as import("node:stream/web").ReadableStream<Uint8Array>);
}

async function validateArchive(archivePath: string, compressedSize: number): Promise<Map<string, ArchiveMember>> {
  const members = new Map<string, ArchiveMember>();
  const expandedLimit = Math.min(maxExpandedArchiveBytes, Math.max(1 << 30, compressedSize * 100));
  let declaredRegularBytes = 0;
  const extractor = tar.extract();
  extractor.on("entry", (header, stream, next) => {
    try {
      if (members.size >= maxArchiveMembers) {
        throw new Error("Actions cache archive has too many members.");
      }
      const name = safeArchivePath(header.name);
      if (members.has(name)) {
        throw new Error(`Actions cache archive repeats ${name}.`);
      }
      if (((header.mode ?? 0) & 0o6000) !== 0) {
        throw new Error(`Actions cache archive member ${name} has set-ID permissions.`);
      }
      const type = header.type ?? "file";
      if (!["file", "directory", "symlink", "link"].includes(type)) {
        throw new Error(`Actions cache archive member ${name} has unsupported type ${type}.`);
      }
      if (name === "." && type !== "directory") {
        throw new Error("Actions cache archive root member must be a directory.");
      }
      const pax = header.pax;
      if (typeof pax === "object" && pax !== null && Object.keys(pax).some((key) =>
        key.startsWith("GNU.sparse.") || key === "SCHILY.realsize")) {
        throw new Error(`Actions cache archive member ${name} uses sparse metadata.`);
      }
      const declaredSize = header.size ?? 0;
      if (!Number.isSafeInteger(declaredSize) || declaredSize < 0) {
        throw new Error(`Actions cache archive member ${name} has an invalid size.`);
      }
      if (type === "file") {
        declaredRegularBytes += declaredSize;
        if (!Number.isSafeInteger(declaredRegularBytes) || declaredRegularBytes > expandedLimit) {
          throw new Error("Actions cache archive declares excessive regular-file bytes.");
        }
      }
      let linkTarget: string | undefined;
      if (type === "symlink") {
        linkTarget = safeArchiveSymlinkTarget(name, header.linkname ?? "");
      } else if (type === "link") {
        linkTarget = safeArchivePath(header.linkname ?? "");
      }
      members.set(name, { type, linkTarget });
      stream.on("end", next);
      stream.resume();
    } catch (error) {
      stream.resume();
      queueMicrotask(() => extractor.destroy(error as Error));
    }
  });
  await pipeline(await decompressedArchive(archivePath), byteLimit(expandedLimit), extractor);
  if (members.size === 0) {
    throw new Error("Actions cache archive has no members.");
  }
  for (const [name, member] of members) {
    for (let ancestor = posix.dirname(name); ancestor !== "." && ancestor !== "/"; ancestor = posix.dirname(ancestor)) {
      const ancestorType = members.get(ancestor)?.type;
      if (ancestorType !== undefined && ancestorType !== "directory") {
        throw new Error(`Actions cache archive member ${name} descends through non-directory ${ancestor}.`);
      }
    }
    if (member.linkTarget !== undefined) {
      const target = members.get(member.linkTarget);
      if (target === undefined || target.type === "symlink" || target.type === "link" ||
          (member.type === "link" && target.type !== "file")) {
        throw new Error(`Actions cache archive link ${name} has an unsafe target.`);
      }
    }
  }
  return members;
}

function safeArchiveSymlinkTarget(name: string, target: string): string {
  if (target === "" || Buffer.byteLength(target) > maxArchiveNameBytes || target.includes("\0") ||
      /[\x01-\x1f\x7f]/.test(target) || target.includes("\\") || target.startsWith("/")) {
    throw new Error(`Unsafe Actions cache archive link target ${JSON.stringify(target)}.`);
  }
  return safeArchivePath(posix.join(posix.dirname(name), target));
}

function safeArchivePath(value: string): string {
  if (value === "" || Buffer.byteLength(value) > maxArchiveNameBytes || value.includes("\0") ||
      /[\x01-\x1f\x7f]/.test(value) || value.includes("\\") || value.startsWith("/") || /^[A-Za-z]:/.test(value)) {
    throw new Error(`Unsafe Actions cache archive path ${JSON.stringify(value)}.`);
  }
  const cleaned = posix.normalize(value);
  if (cleaned === ".." || cleaned.startsWith("../") ||
      (cleaned === "." && value !== "." && value !== "./")) {
    throw new Error(`Actions cache archive path escapes the workspace: ${value}.`);
  }
  return cleaned;
}

async function rejectExistingSymlinkParents(workspace: string, members: Map<string, ArchiveMember>): Promise<void> {
  for (const name of members.keys()) {
    const destination = resolve(workspace, ...name.split("/"));
    if (relative(workspace, destination).startsWith(`..${sep}`) || destination === workspace && name !== ".") {
      throw new Error(`Actions cache archive member ${name} escapes the workspace.`);
    }
    try {
      if ((await fs.lstat(destination)).isSymbolicLink()) {
        throw new Error(`Actions cache archive member ${name} would replace an existing symlink.`);
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
        throw error;
      }
    }
    if (destination === workspace) {
      continue;
    }
    let current = dirname(destination);
    while (current !== workspace) {
      try {
        if ((await fs.lstat(current)).isSymbolicLink()) {
          throw new Error(`Actions cache archive member ${name} descends through an existing symlink.`);
        }
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
          throw error;
        }
      }
      const parent = dirname(current);
      if (parent === current) {
        throw new Error(`Actions cache archive member ${name} escapes the workspace.`);
      }
      current = parent;
    }
  }
}

async function extractArchive(archivePath: string, destination: string): Promise<void> {
  const child = spawn("tar", ["--no-same-owner", "-xf", "-", "-C", destination], {
    stdio: ["pipe", "inherit", "inherit"],
  });
  const source = await decompressedArchive(archivePath);
  try {
    await Promise.all([
      pipeline(source, child.stdin),
      childExit(child, "tar extraction"),
    ]);
  } catch (error) {
    child.kill();
    throw error;
  }
}

interface DirectorySnapshot {
  path: string;
  mode: number;
  atime: Date;
  mtime: Date;
}

interface BackupEntry {
  destination: string;
  backup: string;
}

async function applyExtractedArchive(
  staging: string,
  workspace: string,
  members: Map<string, ArchiveMember>,
): Promise<void> {
  const directoryNames = archiveDirectories(members);
  const existingDirectories: DirectorySnapshot[] = [];
  const existingDirectoryByPath = new Map<string, DirectorySnapshot>();
  const backups: BackupEntry[] = [];
  const createdDirectories: string[] = [];
  const appliedEntries: string[] = [];
  const backupRoot = join(staging, `.layercache-backup-${randomUUID()}`);
  await fs.mkdir(backupRoot, { mode: 0o700 });

  try {
    for (const name of directoryNames) {
      const destination = archiveDestination(workspace, name);
      const existing = await optionalLstat(destination);
      if (existing === undefined) {
        continue;
      }
      if (existing.isSymbolicLink()) {
        throw new Error(`Actions cache archive directory ${name} resolves through an existing symlink.`);
      }
      if (!existing.isDirectory() && members.get(name)?.type !== "directory") {
        throw new Error(`Actions cache archive directory ${name} has an existing non-directory ancestor.`);
      }
      if (existing.isDirectory()) {
        const snapshot = {
          path: destination,
          mode: existing.mode & 0o777,
          atime: existing.atime,
          mtime: existing.mtime,
        };
        existingDirectories.push(snapshot);
        existingDirectoryByPath.set(destination, snapshot);
        await fs.chmod(destination, snapshot.mode | 0o700);
      }
    }

    const orderedMembers = [...members].sort(([left], [right]) => pathDepth(left) - pathDepth(right));
    for (const [name, member] of orderedMembers) {
      if (name === ".") {
        continue;
      }
      const destination = archiveDestination(workspace, name);
      const existing = await optionalLstat(destination);
      if (existing === undefined) {
        continue;
      }
      if (existing.isSymbolicLink()) {
        throw new Error(`Actions cache archive member ${name} would replace an existing symlink.`);
      }
      if (existing.isDirectory()) {
        if (member.type !== "directory") {
          throw new Error(`Actions cache archive member ${name} would replace an existing directory.`);
        }
        continue;
      }
      const backup = archiveDestination(backupRoot, name);
      await fs.mkdir(dirname(backup), { mode: 0o700, recursive: true });
      await fs.rename(destination, backup);
      backups.push({ destination, backup });
    }

    for (const name of directoryNames) {
      if (name === ".") {
        continue;
      }
      const destination = archiveDestination(workspace, name);
      if (await optionalLstat(destination) !== undefined) {
        continue;
      }
      const source = archiveDestination(staging, name);
      const sourceStat = await fs.lstat(source);
      if (!sourceStat.isDirectory()) {
        throw new Error(`Actions cache archive directory ${name} was not extracted as a directory.`);
      }
      await fs.mkdir(destination, { mode: 0o700 });
      createdDirectories.push(destination);
    }

    for (const [name, member] of orderedMembers) {
      if (name === "." || member.type === "directory") {
        continue;
      }
      const source = archiveDestination(staging, name);
      const destination = archiveDestination(workspace, name);
      const temporary = join(dirname(destination), `.layercache-entry-${randomUUID()}`);
      const sourceStat = await fs.lstat(source);
      if (member.type === "symlink") {
        if (!sourceStat.isSymbolicLink()) {
          throw new Error(`Actions cache archive member ${name} was not extracted as a symlink.`);
        }
        const extractedTarget = await fs.readlink(source);
        if (safeArchiveSymlinkTarget(name, extractedTarget) !== member.linkTarget) {
          throw new Error(`Actions cache archive symlink ${name} changed during extraction.`);
        }
        await fs.symlink(extractedTarget, temporary);
      } else {
        if (!sourceStat.isFile()) {
          throw new Error(`Actions cache archive member ${name} was not extracted as a file.`);
        }
        if (member.type === "link") {
          if (member.linkTarget === undefined) {
            throw new Error(`Actions cache archive hard link ${name} has no validated target.`);
          }
          const [extractedSource, extractedTarget] = await Promise.all([
            fs.lstat(source, { bigint: true }),
            fs.lstat(archiveDestination(staging, member.linkTarget), { bigint: true }),
          ]);
          if (!extractedSource.isFile() || !extractedTarget.isFile() ||
              extractedSource.dev !== extractedTarget.dev || extractedSource.ino !== extractedTarget.ino) {
            throw new Error(`Actions cache archive hard link ${name} changed during extraction.`);
          }
        }
        await fs.link(source, temporary);
      }
      try {
        await fs.rename(temporary, destination);
      } catch (error) {
        await rm(temporary, { force: true, recursive: false });
        throw error;
      }
      appliedEntries.push(destination);
    }

    for (const name of [...directoryNames].sort((left, right) => pathDepth(right) - pathDepth(left))) {
      const destination = archiveDestination(workspace, name);
      const existing = existingDirectoryByPath.get(destination);
      if (name === "." || existing !== undefined && members.get(name)?.type !== "directory") {
        if (existing !== undefined) {
          await fs.chmod(destination, existing.mode);
          await fs.utimes(destination, existing.atime, existing.mtime);
        }
        continue;
      }
      const sourceStat = await fs.lstat(archiveDestination(staging, name));
      await fs.chmod(destination, sourceStat.mode & 0o777);
      await fs.utimes(destination, sourceStat.atime, sourceStat.mtime);
    }
  } catch (error) {
    const rollbackErrors: unknown[] = [];
    for (const destination of appliedEntries.reverse()) {
      try {
        await rm(destination, { force: true, recursive: false });
      } catch (rollbackError) {
        rollbackErrors.push(rollbackError);
      }
    }
    for (const directory of createdDirectories.reverse()) {
      try {
        await fs.rmdir(directory);
      } catch (rollbackError) {
        if ((rollbackError as NodeJS.ErrnoException).code !== "ENOENT") {
          rollbackErrors.push(rollbackError);
        }
      }
    }
    for (const entry of backups.reverse()) {
      try {
        await fs.mkdir(dirname(entry.destination), { mode: 0o700, recursive: true });
        await fs.rename(entry.backup, entry.destination);
      } catch (rollbackError) {
        rollbackErrors.push(rollbackError);
      }
    }
    for (const directory of [...existingDirectories].reverse()) {
      try {
        await fs.chmod(directory.path, directory.mode);
        await fs.utimes(directory.path, directory.atime, directory.mtime);
      } catch (rollbackError) {
        rollbackErrors.push(rollbackError);
      }
    }
    if (rollbackErrors.length !== 0) {
      throw new VerifiedCacheSafetyError([error, ...rollbackErrors], staging);
    }
    throw error;
  }
}

function archiveDirectories(members: Map<string, ArchiveMember>): string[] {
  const names = new Set<string>(["."]);
  for (const [name, member] of members) {
    let current = member.type === "directory" ? name : posix.dirname(name);
    while (current !== "." && current !== "/") {
      names.add(current);
      current = posix.dirname(current);
    }
  }
  return [...names].sort((left, right) => pathDepth(left) - pathDepth(right));
}

function archiveDestination(root: string, name: string): string {
  return name === "." ? root : resolve(root, ...name.split("/"));
}

function pathDepth(name: string): number {
  return name === "." ? 0 : name.split("/").length;
}

async function optionalLstat(path: string): Promise<import("node:fs").Stats | undefined> {
  try {
    return await fs.lstat(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return undefined;
    }
    throw error;
  }
}

async function decompressedArchive(archivePath: string): Promise<Readable> {
  const handle = await fs.open(archivePath, "r");
  const magic = Buffer.alloc(4);
  await handle.read(magic, 0, magic.length, 0);
  await handle.close();
  if (magic[0] === 0x1f && magic[1] === 0x8b) {
    return createReadStream(archivePath).pipe(createGunzip());
  }
  if (magic.equals(Buffer.from([0x28, 0xb5, 0x2f, 0xfd]))) {
    const child = spawn("zstd", ["-d", "--stdout", "--quiet", archivePath], { stdio: ["ignore", "pipe", "ignore"] });
    void childExit(child, "zstd decompression").catch((error) => child.stdout.destroy(error));
    return child.stdout;
  }
  return createReadStream(archivePath);
}

function byteLimit(maximum: number): Transform {
  let seen = 0;
  return new Transform({
    transform(chunk: Buffer, _encoding, callback) {
      seen += chunk.length;
      callback(seen > maximum ? new Error("Actions cache archive expands beyond its safety limit.") : null, chunk);
    },
  });
}

function childExit(child: ChildProcess, operation: string): Promise<void> {
  return new Promise((resolvePromise, reject) => {
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (code === 0) {
        resolvePromise();
      } else {
        reject(new Error(`${operation} failed with ${signal === null ? `exit code ${code}` : `signal ${signal}`}.`));
      }
    });
  });
}

function requiredEnvironment(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} is required.`);
  }
  return value;
}

export function validateCacheRequest(paths: string[], primaryKey: string, restoreKeys: string[]): void {
  if (paths.length === 0 || paths.some((path) => path === "")) {
    throw new VerifiedCacheInputError("At least one non-empty cache path is required.");
  }
  const keys = [primaryKey, ...restoreKeys];
  if (primaryKey === "" || keys.length > 10) {
    throw new VerifiedCacheInputError("A primary key and at most nine restore keys are supported.");
  }
  if (keys.some((key) => key.length > 512 || key.includes(","))) {
    throw new VerifiedCacheInputError("Cache keys must be at most 512 characters and cannot contain commas.");
  }
}

async function verifiedWorkspace(env: NodeJS.ProcessEnv): Promise<string> {
  const configured = env.GITHUB_WORKSPACE;
  if (configured === undefined || configured === "") {
    throw new VerifiedCacheInputError("GITHUB_WORKSPACE is required in verified Public Cache mode.");
  }
  try {
    const workspace = await fs.realpath(resolve(configured));
    if (!(await fs.stat(workspace)).isDirectory()) {
      throw new Error("not a directory");
    }
    return workspace;
  } catch {
    throw new VerifiedCacheInputError("GITHUB_WORKSPACE must name an existing directory.");
  }
}

function validateRequestedPaths(paths: string[], workspace: string): void {
  for (const original of paths) {
    const value = original.replace(/^!+/, "");
    if (value === "" || value.startsWith("~") || value.includes("\0") || /[\x01-\x1f\x7f]/.test(value) ||
        value.includes("\\") || value.includes("..")) {
      throw new VerifiedCacheInputError("Verified Public Cache paths must be workspace-relative and traversal-free.");
    }
    const destination = resolve(workspace, value);
    const fromWorkspace = relative(workspace, destination);
    if (fromWorkspace === ".." || fromWorkspace.startsWith(`..${sep}`)) {
      throw new VerifiedCacheInputError("Verified Public Cache paths must stay inside GITHUB_WORKSPACE.");
    }
  }
}
