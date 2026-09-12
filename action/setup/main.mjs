import { appendFileSync, chmodSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { createServer } from 'node:net';
import { dirname, isAbsolute, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { endpointURL, exchangeTurbo, fileCommand } from './auth.ts';
export { endpointURL, exchangeTurbo, fileCommand } from './auth.ts';

const actionDir = dirname(fileURLToPath(import.meta.url));
const commandEscape = value => String(value).replaceAll('%', '%25').replaceAll('\r', '%0D').replaceAll('\n', '%0A');
const defaultLog = value => { process.stdout.write(value); };
export const mask = (value, log = defaultLog) => { if (value) log(`::add-mask::${commandEscape(value)}\n`); };
async function availablePort() {
  const listener = createServer();
  await new Promise((resolve, reject) => { listener.once('error', reject); listener.listen(0, '127.0.0.1', resolve); });
  const port = listener.address().port;
  await new Promise((resolve, reject) => listener.close(error => error ? reject(error) : resolve()));
  return port;
}
export async function setup(env = process.env, { log = defaultLog } = {}) {
  const input = name => (env[`INPUT_${name.toUpperCase()}`] || '').trim();
  const minutes = Number(input('ttl-minutes') || '60');
  if (!Number.isInteger(minutes) || minutes < 1 || minutes > 60) throw new Error('ttl-minutes must be an integer from 1 to 60');
  const maxSize = input('max-size') || '5368709120';
  if (!/^[1-9][0-9]*$/.test(maxSize) || !Number.isSafeInteger(Number(maxSize))) throw new Error('max-size must be positive integer bytes');
  if (!env.RUNNER_TEMP || !isAbsolute(env.RUNNER_TEMP) || !env.GITHUB_WORKSPACE) throw new Error('GitHub runner paths are required');
  const repository = (env.GITHUB_REPOSITORY || '').toLowerCase();
  if (!/^[a-z0-9_.-]+\/[a-z0-9_.-]+$/.test(repository)) throw new Error('Invalid GitHub repository identity');
  const project = input('project') || `github.com/${repository}`;
  if (!project || project.length > 256 || /[\x00-\x20\x7f]/.test(project)) throw new Error('project must be a nonempty identity without whitespace or control characters');
  const event = JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8'));
  const defaultBranch = event.repository?.default_branch;
  if (typeof defaultBranch !== 'string' || !defaultBranch) throw new Error('GitHub event is missing the default branch');
  let compatibility = input('compatibility');
  const root = mkdtempSync(join(realpathSync(env.RUNNER_TEMP), 'layercache-job-'));
  chmodSync(root, 0o700);
  const config = join(root, 'config.json');
  fileCommand(env.GITHUB_STATE, 'root', root);
  let binary = input('binary');
  let started = false;
  const childEnv = { ...env, GH_TOKEN: input('github-token') || env.GH_TOKEN || '' };
  const execute = (file, args) => execFileSync(file, args, { env: childEnv, cwd: env.GITHUB_WORKSPACE, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 120_000 });
  try {
    if (binary) {
      if (!isAbsolute(binary)) throw new Error('binary must be an absolute path');
      binary = realpathSync(binary);
      if (/[\r\n]/.test(binary)) throw new Error('binary path must be one line');
    } else {
      execute('bash', [join(actionDir, '../../scripts/install.sh'), '--repository', input('repository'), '--version', input('version'), '--prefix', join(root, 'bin')]);
      binary = join(root, 'bin/layercache');
    }
    fileCommand(env.GITHUB_STATE, 'binary', binary);
    fileCommand(env.GITHUB_STATE, 'config', config);
    const endpoint = `http://127.0.0.1:${await availablePort()}`;
    const args = ['setup', '--config', config, '--data-dir', join(root, 'data'), '--listen', new URL(endpoint).host,
      '--project', project, '--actions-repository', repository,
      '--actions-ref', env.GITHUB_REF, '--actions-default-ref', `refs/heads/${defaultBranch}`,
      '--max-size', maxSize, '--non-interactive', '--json'];
    if (compatibility) args.push('--compatibility-id', compatibility);
    // Let the CLI detect libc/runtime/toolchain ABI once. The same identity must
    // scope Local Cache, the OIDC exchange, and all exported credentials.
    execute(binary, args);
    compatibility = JSON.parse(readFileSync(config, 'utf8')).compatibilityId;
    if (!input('compatibility')) args.push('--compatibility-id', compatibility);
    const teamURL = input('team-url');
    let team = null;
    if (teamURL) {
      endpointURL(teamURL);
      try {
        team = input('team-token') ? { teamToken: input('team-token') } : await exchangeTurbo({ endpoint: teamURL, project, compatibility, minutes, env, log });
        mask(team.teamToken, log);
        const tokenFile = join(root, 'team-token');
        writeFileSync(tokenFile, `${team.teamToken}\n`, { mode: 0o600, flag: 'wx' });
        args.push('--team-url', teamURL, '--team-token-file', tokenFile);
      } catch {
        team = null;
        log('::warning::Team Cache authentication unavailable; this job uses Local Cache. Check id-token permission and the Team Cache project.\n');
      }
    }
    if (team) execute(binary, args);
    execute(binary, ['start', '--config', config, '--json']);
    started = true;
    const route = JSON.parse(execute(binary, ['vm-route', 'issue', '--config', config, '--endpoint', endpoint,
      '--integration', 'all', '--ttl', `${minutes}m`, '--actions-repository', repository,
      '--actions-ref', env.GITHUB_REF, '--actions-default-ref', `refs/heads/${defaultBranch}`,
      '--actions-source-commit', env.GITHUB_SHA, '--json']));
    for (const [key, value] of Object.entries(route.environment)) {
      if (key.endsWith('TOKEN')) mask(value, log);
      // GitHub's own Actions environment remains under runner control. The cache
      // action accepts the scoped endpoint/token explicitly, including in post.
      if (key.startsWith('TURBO_')) fileCommand(env.GITHUB_ENV, key, value);
    }
    fileCommand(env.GITHUB_ENV, 'LAYER_CACHE_CONFIG', config);
    fileCommand(env.GITHUB_ENV, 'LAYER_CACHE_EXPIRES_AT', route.expiresAt);
    appendFileSync(env.GITHUB_PATH, `${dirname(binary)}\n`);
    for (const [key, value] of Object.entries({ binary, config, endpoint, project, compatibility,
      'actions-token': route.environment.ACTIONS_RUNTIME_TOKEN, 'expires-at': route.expiresAt })) fileCommand(env.GITHUB_OUTPUT, key, value);
    log(`Layer Cache ready. Job credentials expire at ${route.expiresAt}.\n`);
    if (team?.expiresAt) log(`Turbo Team Cache credentials expire at ${team.expiresAt}. Rerun setup before that time for longer jobs.\n`);
    return { root, binary, config, endpoint, route };
  } catch (error) {
    if (started) {
      try { execute(binary, ['stop', '--config', config, '--json']); }
      catch {
        log('::warning::Setup failed and could not confirm daemon shutdown; job data was preserved for post cleanup.\n');
        throw error;
      }
    }
    rmSync(root, { recursive: true, force: true });
    throw error;
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  setup().catch(error => {
    const reason = error.status !== undefined || error.stderr !== undefined
      ? 'A setup command failed. Verify the immutable release and available disk space.'
      : error.message;
    process.stdout.write(`::error::${commandEscape(`Layer Cache setup failed: ${reason}`)}\n`);
    process.exitCode = 1;
  });
}
