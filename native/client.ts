import { createHash, randomUUID } from 'node:crypto';
import { createReadStream, createWriteStream } from 'node:fs';
import { lstat, mkdir, readdir, readFile, rename, rm, stat, utimes, writeFile } from 'node:fs/promises';
import { homedir } from 'node:os';
import { basename, dirname, isAbsolute, join, resolve } from 'node:path';
import { Readable, Transform } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { setTimeout as delay } from 'node:timers/promises';
import { createGunzip } from 'node:zlib';
import * as tar from 'tar';

export interface Identity {
  project: string;
  compatibility: string;
  key: string;
}
export interface Options {
  endpoint?: string;
  token?: string;
  cacheDir?: string;
  maxBytes?: number;
  timeoutMs?: number;
}
export interface Result {
  hit: boolean;
  source: 'local' | 'team' | 'miss';
  path?: string;
  digest?: string;
  bytes: number;
  elapsedMs: number;
}

export function environmentOptions(options: Options = {}, env: NodeJS.ProcessEnv = process.env): Options {
  if (env.LAYER_CACHE_TOKEN) return { ...options, endpoint: env.LAYER_CACHE_URL ?? options.endpoint, token: env.LAYER_CACHE_TOKEN };
  // Keep the injected endpoint and capability paired. A local Workspace token
  // must never be sent to the remote endpoint in an app's static configuration.
  if (env.TURBO_TOKEN && env.TURBO_API) return { ...options, endpoint: env.TURBO_API, token: env.TURBO_TOKEN };
  return { ...options, endpoint: env.LAYER_CACHE_URL ?? options.endpoint, token: undefined };
}

const sha = (value: string) => createHash('sha256').update(value).digest('hex');
export function artifactKey(identity: Identity): string {
  if (!identity.project || identity.project.length > 256 || /[\x00-\x20\x7f]/.test(identity.project)) throw new Error('Invalid project');
  if (!/^[a-z0-9][a-z0-9_.:+@-]{0,255}$/.test(identity.compatibility)) throw new Error('Explicit toolchain compatibility required');
  if (!identity.key || identity.key.length > 8192) throw new Error('Explicit native artifact key required');
  return `native-v1-${sha(JSON.stringify([identity.project, identity.compatibility, identity.key]))}`;
}
export async function digestFile(path: string): Promise<string> {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest('hex');
}
function origin(value: string): string {
  const url = new URL(value);
  if (url.username || url.password || url.pathname !== '/' || url.search || url.hash ||
      (url.protocol !== 'https:' && !(url.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname)))) {
    throw new Error('Cache endpoint must be an HTTPS origin, or HTTP loopback');
  }
  return url.origin;
}

// Each cache operation holds one cross-process lease. Downloads and extraction
// are bounded by the configured budget; verified archives are immutable.
export class NativeCache {
  readonly root: string;
  readonly maxBytes: number;
  readonly options: Options;
  constructor(options: Options = {}) {
    this.root = resolve(options.cacheDir ?? process.env.LAYER_CACHE_NATIVE_DIR ?? join(homedir(), '.cache', 'layercache', 'native-v1'));
    this.maxBytes = options.maxBytes ?? 5 * 1024 ** 3;
    if (!Number.isSafeInteger(this.maxBytes) || this.maxBytes <= 0) throw new Error('maxBytes must be positive integer bytes');
    this.options = { ...options, endpoint: options.endpoint ? origin(options.endpoint) : undefined };
  }
  private async locked<T>(run: () => Promise<T>): Promise<T> {
    await mkdir(this.root, { recursive: true, mode: 0o700 });
    const lock = join(this.root, '.lock');
    // Never break another process's lease based on wall-clock age. A crashed
    // lease fails closed and can be removed after confirming no cache process runs.
    const deadline = Date.now() + (this.options.timeoutMs ?? 120_000);
    while (true) {
      try { await mkdir(lock); break; }
      catch (error) {
        if (!(error instanceof Error) || !('code' in error) || error.code !== 'EEXIST') throw error;
        if (Date.now() >= deadline) throw new Error('Native cache is busy; inspect its .lock directory');
        await delay(100);
      }
    }
    try { return await run(); } finally { await rm(lock, { recursive: true, force: true }); }
  }
  private url(identity: Identity): URL | null {
    if (!this.options.endpoint || !this.options.token) return null;
    const url = new URL(`/v8/artifacts/${artifactKey(identity)}`, this.options.endpoint);
    url.searchParams.set('teamId', identity.project);
    return url;
  }
  private headers(identity: Identity): Record<string, string> {
    return { Authorization: `Bearer ${this.options.token}`, 'X-LayerCache-Compatibility': identity.compatibility };
  }
  private async evict(reserve: number, keep?: string): Promise<void> {
    if (reserve > this.maxBytes) throw new Error('Artifact exceeds local cache budget');
    const entries = await Promise.all((await readdir(this.root)).filter(name => /^native-v1-[a-f0-9]{64}\.tgz$/.test(name)).map(async name => {
      const path = join(this.root, name);
      const info = await stat(path);
      return { path, size: info.size, used: info.mtimeMs };
    }));
    let bytes = entries.reduce((total, entry) => total + entry.size, 0);
    for (const entry of entries.sort((a, b) => a.used - b.used)) {
      if (bytes + reserve <= this.maxBytes) break;
      if (entry.path === keep) continue;
      await rm(entry.path, { force: true });
      await rm(`${entry.path}.sha256`, { force: true });
      bytes -= entry.size;
    }
    if (bytes + reserve > this.maxBytes) throw new Error('Insufficient local cache budget');
  }
  private async verified(path: string): Promise<string | null> {
    try {
      const expected = (await readFile(`${path}.sha256`, 'utf8')).trim();
      if (!/^[a-f0-9]{64}$/.test(expected) || await digestFile(path) !== expected) throw new Error('Corrupt local artifact');
      return expected;
    } catch {
      await rm(path, { force: true });
      await rm(`${path}.sha256`, { force: true });
      return null;
    }
  }
  async restore(identity: Identity, destination?: string): Promise<Result> {
    const started = performance.now();
    const key = artifactKey(identity);
    return this.locked(async () => {
      const path = join(this.root, `${key}.tgz`);
      let digest = await this.verified(path);
      let source: Result['source'] = 'local';
      if (!digest) {
        const url = this.url(identity);
        if (!url) return { hit: false, source: 'miss', bytes: 0, elapsedMs: performance.now() - started };
        const response = await fetch(url, { headers: this.headers(identity), redirect: 'error', signal: AbortSignal.timeout(this.options.timeoutMs ?? 120_000) });
        if (response.status === 404) { await response.body?.cancel(); return { hit: false, source: 'miss', bytes: 0, elapsedMs: performance.now() - started }; }
        if (!response.ok) { await response.body?.cancel(); throw new Error(`Team Cache returned HTTP ${response.status}`); }
        const expected = response.headers.get('x-layercache-digest') ?? '';
        const size = Number(response.headers.get('content-length'));
        if (!/^sha256:[a-f0-9]{64}$/.test(expected) || !Number.isSafeInteger(size) || size <= 0 || size > this.maxBytes || !response.body) {
          await response.body?.cancel(); throw new Error('Invalid artifact digest or size');
        }
        await this.evict(size);
        const temporary = `${path}.${randomUUID()}.partial`;
        let received = 0;
        try {
          await pipeline(Readable.fromWeb(response.body), new Transform({ transform(chunk, _encoding, callback) {
            received += chunk.length;
            callback(received > size ? new Error('Artifact exceeds declared size') : null, chunk);
          } }), createWriteStream(temporary, { flags: 'wx', mode: 0o600 }));
          digest = await digestFile(temporary);
          if (received !== size || `sha256:${digest}` !== expected) throw new Error('Artifact integrity check failed');
          await rename(temporary, path);
          await writeFile(`${path}.sha256`, digest, { mode: 0o600 });
        } finally { await rm(temporary, { force: true }); }
        source = 'team';
      }
      await this.evict(0, path);
      await utimes(path, new Date(), new Date());
      await inspect(path, this.maxBytes);
      if (destination) await extract(path, destination, this.maxBytes);
      return { hit: true, source, path: destination ? resolve(destination) : path, digest, bytes: (await stat(path)).size, elapsedMs: performance.now() - started };
    });
  }
  async save(identity: Identity, input: string): Promise<Result> {
    const started = performance.now();
    const key = artifactKey(identity);
    return this.locked(async () => {
      const path = join(this.root, `${key}.tgz`);
      const temporary = `${path}.${randomUUID()}.partial`;
      try {
        const inputPath = resolve(input);
        const info = await stat(inputPath);
        if (!info.isDirectory() && !info.isFile()) throw new Error('Build must be a directory or regular file');
        // Gzip's worst-case overhead is below this conservative tar-size bound.
        // Reserve before staging instead of temporarily exceeding the disk cap.
        const reserve = await archiveBound(inputPath);
        await this.evict(reserve);
        const archive = tar.c({ cwd: dirname(inputPath), portable: true, gzip: true, noMtime: true, strict: true }, [basename(inputPath)]);
        let size = 0;
        const limit = reserve;
        await pipeline(archive, new Transform({ transform(chunk, _encoding, callback) {
          size += chunk.length;
          callback(size > limit ? new Error('Artifact exceeds local cache budget') : null, chunk);
        } }), createWriteStream(temporary, { flags: 'wx', mode: 0o600 }));
        // Validate link/path/expanded-size rules before publishing anything.
        await inspect(temporary, this.maxBytes);
        const digest = await digestFile(temporary);
        await rename(temporary, path);
        await writeFile(`${path}.sha256`, digest, { mode: 0o600 });
        const url = this.url(identity);
        if (url) {
          const blob = await import('node:fs').then(fs => fs.openAsBlob(path));
          const response = await fetch(url, { method: 'PUT', headers: { ...this.headers(identity), 'Content-Type': 'application/octet-stream', 'Content-Length': String(size) }, body: blob,
            redirect: 'error', signal: AbortSignal.timeout(this.options.timeoutMs ?? 120_000) });
          await response.body?.cancel();
          if (!response.ok && response.status !== 409) throw new Error(`Team Cache upload returned HTTP ${response.status}; Local Cache retained`);
          if (response.status === 409) throw new Error('Native artifact key already has different content; use a complete build key');
        }
        return { hit: false, source: 'local', path, digest, bytes: size, elapsedMs: performance.now() - started };
      } finally { await rm(temporary, { force: true }); }
    });
  }
}

// Never extract into a checkout or an existing destination. Permit only internal
// symlinks such as Apple framework Current -> A; reject hard links and special files.
async function inspect(archive: string, maxBytes: number): Promise<void> {
  let bytes = 0;
  let count = 0;
  let failure: Error | undefined;
  let expandedBytes = 0;
  const paths = new Set<string>();
  const foldedPaths = new Set<string>();
  const links = new Map<string, string>();
  const parser = tar.t({ strict: true, onReadEntry(entry) {
    // Event callbacks must not throw outside tar's promise. Finish parsing then
    // reject before extraction; no bytes are written to the destination here.
    if (failure) return;
    try {
    if (++count > 100_000) throw new Error('Too many archive entries');
    const path = entry.path.replace(/\/$/, '');
    if (!path || isAbsolute(path) || path.includes('\\') || path.split('/').some(part => part === '..' || part === '') || /[\x00-\x1f]/.test(path)) throw new Error('Unsafe archive path');
    if (paths.has(path) || foldedPaths.has(path.toLowerCase())) throw new Error('Duplicate or case-colliding archive path');
    paths.add(path); foldedPaths.add(path.toLowerCase());
    if (!['File', 'Directory', 'SymbolicLink'].includes(entry.type)) throw new Error('Unsupported archive entry');
    if ((entry.mode ?? 0) & 0o7000) throw new Error('Special permission bits are not cacheable');
    bytes += entry.size;
    if (bytes > maxBytes) throw new Error('Expanded artifact exceeds local cache budget');
    if (entry.type === 'SymbolicLink') {
      const target = entry.linkpath;
      if (!target) throw new Error('Empty archive link');
      const resolved = resolve('/artifact', dirname(path), target);
      if (isAbsolute(target) || target.includes('\\') || !resolved.startsWith('/artifact/')) throw new Error('Unsafe archive link');
      links.set(path, target);
    }
    } catch (error) { failure = error instanceof Error ? error : new Error('Invalid archive'); }
  } });
  await pipeline(createReadStream(archive), createGunzip(), new Transform({ transform(chunk, _encoding, callback) {
    expandedBytes += chunk.length;
    callback(expandedBytes > maxBytes ? new Error('Expanded artifact exceeds local cache budget') : null, chunk);
  } }), parser);
  if (failure) throw failure;
  if (count === 0) throw new Error('Empty artifact archive');
  for (const path of paths) {
    const parts = path.split('/');
    for (let end = 1; end < parts.length; end++) {
      if (links.has(parts.slice(0, end).join('/'))) throw new Error('Archive writes through a symlink');
    }
  }
  for (const path of links.keys()) {
    const pending = path.split('/');
    const resolved: string[] = [];
    let followed = 0;
    while (pending.length) {
      const part = pending.shift()!;
      if (part === '.' || part === '') continue;
      if (part === '..') {
        if (!resolved.length) throw new Error('Unsafe archive link chain');
        resolved.pop(); continue;
      }
      resolved.push(part);
      const target = links.get(resolved.join('/'));
      if (target !== undefined) {
        if (++followed > 40) throw new Error('Cyclic archive link');
        resolved.pop(); pending.unshift(...target.split('/'));
      }
    }
  }
}
async function extract(archive: string, destination: string, maxBytes: number): Promise<void> {
  const target = resolve(destination);
  await mkdir(dirname(target), { recursive: true });
  await mkdir(target); // Refuse replacement of user files, even an empty directory.
  try { await tar.x({ file: archive, cwd: target, strict: true, preservePaths: false }); }
  catch (error) { await rm(target, { recursive: true, force: true }); throw error; }
}

async function archiveBound(path: string): Promise<number> {
  let bytes = 65536;
  let count = 0;
  async function visit(current: string): Promise<void> {
    if (++count > 100_000) throw new Error('Too many build files');
    const info = await lstat(current);
    bytes += 8192 + Math.ceil(info.size * 1.01);
    if (info.isDirectory()) for (const name of await readdir(current)) await visit(join(current, name));
    else if (!info.isFile() && !info.isSymbolicLink()) throw new Error('Unsupported build file');
  }
  await visit(path);
  return bytes;
}
