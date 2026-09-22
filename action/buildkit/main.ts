import { execFileSync } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync, rmSync, renameSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { endpointURL, exchangeCapability, fileCommand } from '../setup/auth.ts';

type DockerConfig = { auths?: Record<string, unknown>; credsStore?: string; credHelpers?: Record<string, string> };
export function writeConfig(path: string, config: DockerConfig, rename = renameSync): void {
  const temporary = `${path}.layercache-${randomUUID()}`;
  try {
    writeFileSync(temporary, JSON.stringify(config), { mode: 0o600, flag: 'wx' });
    rename(temporary, path);
  } finally { rmSync(temporary, { force: true }); }
}
export function readConfig(path: string): DockerConfig {
  try {
    const value: unknown = JSON.parse(readFileSync(path, 'utf8'));
    const object = (item: unknown): item is Record<string, unknown> => item !== null && typeof item === 'object' && !Array.isArray(item);
    if (!object(value) || (value.auths !== undefined && !object(value.auths)) ||
        (value.credsStore !== undefined && typeof value.credsStore !== 'string') ||
        (value.credHelpers !== undefined && (!object(value.credHelpers) || Object.values(value.credHelpers).some(item => typeof item !== 'string')))) throw new Error('Invalid Docker config');
    return value as DockerConfig;
  }
  catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return {}; throw error; }
}
export function cacheOptions(registry: string, namespace: string, scope: string, compatibility: string, writable: boolean) {
  const tag = createHash('sha256').update(compatibility).digest('hex');
  const ref = `${registry}/${namespace}/${scope}:${tag}`;
  return { 'cache-from': `type=registry,ref=${ref}`, 'cache-to': writable ? `type=registry,ref=${ref},mode=max,image-manifest=true,oci-mediatypes=true,ignore-error=true` : '' };
}
export async function setupBuildkit(env: NodeJS.ProcessEnv = process.env, deps: {
  fetcher?: typeof fetch; log?: (text: string) => void; login?: (directory: string, registry: string, namespace: string, token: string) => void;
} = {}): Promise<void> {
  const log = deps.log || ((text: string) => { process.stdout.write(text); });
  const output = (key: string, value: string) => fileCommand(env.GITHUB_OUTPUT, key, value);
  for (const key of ['registry', 'cache-from', 'cache-to', 'expires-at']) output(key, '');
  output('enabled', 'false');
  let temporary: string | undefined;
  try {
    const url = endpointURL(env['INPUT_TEAM-URL'] || '');
    const project = (env.INPUT_PROJECT || `github.com/${env.GITHUB_REPOSITORY || ''}`).toLowerCase();
    const namespace = env.INPUT_NAMESPACE || '';
    const scope = env.INPUT_SCOPE || '';
    const compatibility = env.INPUT_COMPATIBILITY || '';
    const minutes = Number(env['INPUT_TTL-MINUTES'] || '30');
    if (url.protocol !== 'https:' || url.pathname !== '/' || !env.GITHUB_REPOSITORY ||
        !/^github\.com\/[a-z0-9_.-]+\/[a-z0-9_.-]+$/.test(project) ||
        !/^[a-z0-9][a-z0-9-]{0,62}$/.test(namespace) || !/^[a-z0-9][a-z0-9_.-]{0,127}$/.test(scope) ||
        !/^[a-z0-9][a-z0-9_.:+@-]{0,255}$/.test(compatibility) ||
        !Number.isInteger(minutes) || minutes < 1 || minutes > 60 || !env.GITHUB_STATE) throw new Error('Invalid inputs');
    const directory = env.DOCKER_CONFIG || join(homedir(), '.docker');
    const path = join(directory, 'config.json');
    let config = readConfig(path);
    // GitHub-hosted runners use file-backed auth. Never interfere with a user's
    // global credential store or registry helper on a self-hosted runner.
    if (config.credsStore || config.credHelpers?.[url.host]) throw new Error('Credential helper unsupported');
    const result = await exchangeCapability({ endpoint: url.origin, project, compatibility, minutes, env, integration: 'buildkit', ...(deps.fetcher ? { fetcher: deps.fetcher } : {}), log });
    const parts = result.teamToken.split('.');
    if (parts.length !== 3 || parts[0] !== 'lc2') throw new Error('Invalid capability');
    // This is the credential received directly over authenticated HTTPS, not an
    // unverified user-supplied JWT. The registry independently enforces writes.
    const claims = JSON.parse(Buffer.from(parts[1]!, 'base64url').toString('utf8')) as { project?: string; integration?: string; capabilities?: string[] };
    if (claims.project !== project || claims.integration !== 'buildkit' || !Array.isArray(claims.capabilities)) throw new Error('Wrong capability scope');
    temporary = mkdtempSync(join(env.RUNNER_TEMP || tmpdir(), 'layercache-docker-login-'));
    const login = deps.login || ((dir, registry, username, token) => {
      execFileSync('docker', ['--config', dir, 'login', registry, '--username', username, '--password-stdin'], { input: token, stdio: ['pipe', 'pipe', 'pipe'], timeout: 30_000, maxBuffer: 64 * 1024 });
    });
    login(temporary, url.host, namespace, result.teamToken);
    const installed = readConfig(join(temporary, 'config.json')).auths?.[url.host];
    if (!installed) throw new Error('Login did not persist credential');
    config = readConfig(path);
    if (config.credsStore || config.credHelpers?.[url.host]) throw new Error('Credential helper unsupported');
    const previous = config.auths?.[url.host];
    // Save cleanup before installing credentials. Multiple invocations unwind in
    // reverse post order, restoring only their own registry entry.
    fileCommand(env.GITHUB_STATE, 'layercache_buildkit', JSON.stringify({ path, registry: url.host, installed, previous }));
    config.auths = { ...config.auths, [url.host]: installed };
    mkdirSync(directory, { recursive: true, mode: 0o700 });
    writeConfig(path, config);
    const options = cacheOptions(url.host, namespace, scope, compatibility, claims.capabilities.includes('write') || claims.capabilities.includes('admin'));
    for (const [key, value] of Object.entries({ ...options, registry: url.host, 'expires-at': result.expiresAt, enabled: 'true' })) output(key, value);
  } catch {
    log('::warning::Layer Cache BuildKit cache unavailable; continuing without its registry cache. Check inputs, id-token permission, and Docker authentication configuration.\n');
  } finally { if (temporary) rmSync(temporary, { recursive: true, force: true }); }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await setupBuildkit();
