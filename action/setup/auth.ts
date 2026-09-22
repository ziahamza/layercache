import { appendFileSync } from 'node:fs';
import { randomUUID } from 'node:crypto';

const escape = (value: string) => value.replaceAll('%', '%25').replaceAll('\r', '%0D').replaceAll('\n', '%0A');
const defaultLog = (value: string) => { process.stdout.write(value); };
export function fileCommand(path: string | undefined, name: string, value: string): void {
  if (!path || !/^[a-zA-Z_][a-zA-Z0-9_-]*$/.test(name)) throw new Error('Invalid GitHub file command');
  const delimiter = `layercache_${randomUUID()}`;
  appendFileSync(path, `${name}<<${delimiter}\n${value}\n${delimiter}\n`, { mode: 0o600 });
}
export function endpointURL(value: string): URL {
  const url = new URL(value);
  if (url.username || url.password || url.search || url.hash ||
      (url.protocol !== 'https:' && !(url.protocol === 'http:' && ['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname)))) throw new Error('Team Cache requires HTTPS, or HTTP on loopback');
  return url;
}
export async function exchangeCapability({ endpoint, project, compatibility, minutes, env, integration = 'turbo', fetcher = fetch, log = defaultLog }: {
  endpoint: string; project: string; compatibility: string; minutes: number; env: NodeJS.ProcessEnv; integration?: 'turbo' | 'buildkit'; fetcher?: typeof fetch; log?: (value: string) => void;
}): Promise<{ teamToken: string; expiresAt: string }> {
  if (!env.ACTIONS_ID_TOKEN_REQUEST_URL || !env.ACTIONS_ID_TOKEN_REQUEST_TOKEN) throw new Error('Team Cache OIDC needs job permissions id-token: write');
  const oidc = new URL(env.ACTIONS_ID_TOKEN_REQUEST_URL);
  if (oidc.protocol !== 'https:') throw new Error('GitHub OIDC request URL must use HTTPS');
  oidc.searchParams.set('audience', `layercache:${project}`);
  const response = await fetcher(oidc, { headers: { Authorization: `Bearer ${env.ACTIONS_ID_TOKEN_REQUEST_TOKEN}` }, redirect: 'error', signal: AbortSignal.timeout(20_000) });
  if (!response.ok) throw new Error(`GitHub OIDC returned HTTP ${response.status}`);
  const identity = await response.json() as { value?: string };
  if (typeof identity.value !== 'string' || !identity.value) throw new Error('GitHub OIDC did not return a token');
  log(`::add-mask::${escape(identity.value)}\n`);
  const url = endpointURL(endpoint);
  url.pathname = `${url.pathname.replace(/\/+$/, '').replace(/\/v1$/, '')}/v1/auth/github-oidc/exchange`;
  const exchanged = await fetcher(url, { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ project, compatibility, idToken: identity.value, integration, ttlSeconds: minutes * 60 }), redirect: 'error', signal: AbortSignal.timeout(20_000) });
  if (!exchanged.ok) throw new Error(`Turbo OIDC exchange returned HTTP ${exchanged.status}`);
  const result = await exchanged.json() as { teamToken: string; expiresAt: string };
  if (typeof result.teamToken !== 'string' || !result.teamToken || !Number.isFinite(Date.parse(result.expiresAt)) ||
      Date.parse(result.expiresAt) < Date.now() + 30_000 || Date.parse(result.expiresAt) > Date.now() + 3_660_000) throw new Error('Turbo OIDC exchange returned invalid credentials');
  log(`::add-mask::${escape(result.teamToken)}\n`);
  return result;
}
export const exchangeTurbo = exchangeCapability;
