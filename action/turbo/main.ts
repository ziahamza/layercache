import { pathToFileURL } from 'node:url';
import { endpointURL, exchangeTurbo, fileCommand } from '../setup/main.mjs';
import { startReport } from './reports.ts';

interface Dependencies {
  fetcher?: typeof fetch;
  log?: (value: string) => void;
  write?: (path: string, name: string, value: string) => void;
}

export async function setupTurbo(
  env: NodeJS.ProcessEnv = process.env,
  {
    fetcher = fetch,
    log = (value) => process.stdout.write(value),
    write = fileCommand,
  }: Dependencies = {},
): Promise<void> {
  const endpoint = (env['INPUT_TEAM-URL'] || '').trim();
  const project = (
    env.INPUT_PROJECT || `github.com/${env.GITHUB_REPOSITORY || ''}`
  )
    .trim()
    .toLowerCase();
  const compatibility = (env.INPUT_COMPATIBILITY || '').trim();
  const minutes = Number(env['INPUT_TTL-MINUTES'] || '30');
  const url = endpointURL(endpoint);
  if (url.protocol !== 'https:' || url.pathname !== '/')
    throw new Error('Use an HTTPS Team Cache origin');
  if (
    !env.GITHUB_REPOSITORY ||
    !project ||
    project.length > 256 ||
    /[\x00-\x20\x7f]/.test(project)
  )
    throw new Error('Invalid project identity');
  if (!/^[a-z0-9][a-z0-9_.:+@-]{0,255}$/.test(compatibility))
    throw new Error('An explicit compatibility identity is required');
  if (!Number.isInteger(minutes) || minutes < 1 || minutes > 60)
    throw new Error('Invalid credential lifetime');
  if (
    !env.GITHUB_ENV ||
    !env.GITHUB_OUTPUT ||
    !env.GITHUB_STATE ||
    !env.GITHUB_WORKSPACE
  )
    throw new Error('GitHub runner file commands are required');
  let state;
  try {
    state = await startReport(
      env.GITHUB_WORKSPACE,
      env['INPUT_WORKING-DIRECTORY'] || '.',
      { endpoint: url.origin, project, compatibility },
    );
  } catch {
    log(
      '::warning::Layer Cache Turbo summary collection could not be initialized; caching remains available.\n',
    );
  }
  const result = await exchangeTurbo({
    endpoint: url.origin,
    project,
    compatibility,
    minutes,
    env,
    fetcher,
    log,
  });
  // Export only after successful authentication. Do not alter GitHub's Actions cache environment.
  for (const [key, value] of Object.entries({
    TURBO_API: url.origin,
    TURBO_TEAM: project,
    TURBO_TOKEN: result.teamToken,
    TURBO_RUN_SUMMARY: 'true',
  }))
    write(env.GITHUB_ENV, key, value);
  if (state) write(env.GITHUB_STATE, 'layercache_turbo', JSON.stringify(state));
  write(env.GITHUB_OUTPUT, 'expires-at', result.expiresAt);
  log(
    `Turbo connected to Layer Cache. Credentials expire at ${result.expiresAt}.\n`,
  );
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  setupTurbo().catch(() => {
    process.stdout.write(
      '::error::Layer Cache authentication failed. Check the endpoint, project, compatibility, and id-token permission.\n',
    );
    process.exitCode = 1;
  });
}
