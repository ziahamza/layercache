import assert from 'node:assert/strict';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createInterface } from 'node:readline';
import { setTimeout as delay } from 'node:timers/promises';

// Exercise the public CLI and Turbo protocol against the real local cloud
// service. Only GitHub is emulated by qa/cloud/fixture.ts.
const binary = process.env.LAYERCACHE_BIN;
const turbo = process.env.TURBO_BIN;
if (!binary || !path.isAbsolute(binary) || !turbo || !path.isAbsolute(turbo)) {
  throw new Error('Set absolute LAYERCACHE_BIN and TURBO_BIN paths');
}

const root = fs.mkdtempSync(path.join(os.tmpdir(), 'layercache-cloud-turbo-'));
const output = process.env.QA_OUTPUT || fs.mkdtempSync(path.join(os.tmpdir(), 'layercache-cloud-turbo-evidence-'));
fs.mkdirSync(output, { recursive: true });
const children = new Set<ChildProcess>();
const source = path.resolve('qa/fixtures/turbo');
const counter = path.join(root, 'executions');
type Workspace = { path: string; config: string; cache: string; listen: string; projectId: string; teamToken: string };
type Browser = { cookie: string; csrf: string };
type Report = { runId: string; eligible: number; hits: number; misses: number; degraded: boolean; bytes: { downloaded: number; uploaded: number }; outcomes: { source: string }[] };
type Summary = { tasks: { cache: { status: string; source?: string } }[] };

function command(executable: string, args: string[], cwd = process.cwd(), env = process.env): string {
  const result = spawnSync(executable, args, { cwd, env, encoding: 'utf8', timeout: 60_000, maxBuffer: 4 << 20 });
  assert.equal(result.status, 0, `${path.basename(executable)} ${args.slice(0, 2).join(' ')} failed: ${result.error || result.stderr || result.stdout}`);
  return result.stdout;
}
function sha256(filename: string): string {
  return createHash('sha256').update(fs.readFileSync(filename)).digest('hex');
}
async function stop(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  child.kill('SIGTERM');
  for (let i = 0; i < 50 && child.exitCode === null && child.signalCode === null; i++) await delay(100);
  if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
}
async function ready(origin: string): Promise<void> {
  for (let i = 0; i < 100; i++) {
    try {
      if ((await fetch(origin + '/healthz', { signal: AbortSignal.timeout(200) })).ok) return;
    } catch { /* Wait for the service to bind. */ }
    await delay(100);
  }
  throw new Error(`service did not become healthy at ${origin}`);
}
async function fixtureOrigin(child: ChildProcess): Promise<string> {
  const lines = createInterface({ input: child.stdout! });
  return await new Promise<string>((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('cloud fixture readiness timed out')), 30_000);
    child.once('exit', () => { clearTimeout(timeout); reject(new Error('cloud fixture exited before readiness')); });
    lines.on('line', line => {
      try {
        const message = JSON.parse(line) as { ready?: boolean; origin?: string };
        if (message.ready && message.origin) {
          clearTimeout(timeout);
          lines.close();
          resolve(message.origin);
        }
      } catch { /* Ignore non-JSON fixture output. */ }
    });
  });
}
function cookie(response: Response, name: string): string {
  const value = response.headers.getSetCookie().map(value => value.split(';', 1)[0]).find(value => value.startsWith(name + '='));
  assert.ok(value, `missing ${name} cookie`);
  return value;
}
async function login(origin: string, user: 'alice' | 'bob'): Promise<Browser> {
  const begin = await fetch(origin + '/auth/github', { redirect: 'manual' });
  assert.equal(begin.status, 302);
  const stateCookie = cookie(begin, 'layercache_oauth');
  const authorize = await fetch(begin.headers.get('location')!);
  assert.equal(authorize.status, 200);
  const page = await authorize.text();
  const escaped = page.match(new RegExp(`<a href="([^"]+)">Sign in as ${user}</a>`))?.[1];
  assert.ok(escaped, `GitHub fixture omitted ${user}`);
  const callback = await fetch(escaped.replaceAll('&amp;', '&'), { headers: { Cookie: stateCookie }, redirect: 'manual' });
  assert.equal(callback.status, 303);
  const sessionCookie = cookie(callback, 'layercache_session');
  const response = await fetch(origin + '/api/session', { headers: { Cookie: sessionCookie } });
  assert.equal(response.status, 200);
  const session = await response.json() as { user: { login: string }; csrfToken: string };
  assert.equal(session.user.login, user);
  return { cookie: sessionCookie, csrf: session.csrfToken };
}
async function api<T>(origin: string, browser: Browser, method: string, route: string, body: object | undefined, expected: number): Promise<T> {
  const response = await fetch(origin + route, {
    method,
    headers: { Cookie: browser.cookie, ...(method === 'GET' ? {} : { Origin: origin, 'X-CSRF-Token': browser.csrf, 'Content-Type': 'application/json' }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const contents = await response.text();
  assert.equal(response.status, expected, `${method} ${route}: ${contents.slice(0, 300)}`);
  return contents ? JSON.parse(contents) as T : undefined as T;
}
function makeWorkspace(template: string, destination: string): void {
  command('git', ['clone', '--quiet', '--no-hardlinks', template, destination], root);
  command('git', ['remote', 'set-url', 'origin', 'https://github.com/acme/web'], destination);
  assert.equal(fs.existsSync(path.join(destination, '.turbo')), false, 'workspace inherited Turbo cache');
  assert.equal(fs.existsSync(path.join(destination, 'packages/app/dist')), false, 'workspace inherited build output');
}
function makeTemplate(): string {
  const template = path.join(root, 'template');
  fs.mkdirSync(path.join(template, 'packages/app'), { recursive: true });
  for (const file of ['package.json', 'pnpm-workspace.yaml', 'pnpm-lock.yaml', 'turbo.json', 'packages/app/package.json', 'packages/app/build.mjs']) {
    fs.copyFileSync(path.join(source, file), path.join(template, file));
  }
  fs.writeFileSync(path.join(template, '.gitignore'), 'node_modules/\n.turbo/\ndist/\n');
  command('git', ['init', '--quiet', '-b', 'main'], template);
  command('git', ['add', '.'], template);
  command('git', ['-c', 'user.name=Layer Cache QA', '-c', 'user.email=qa@layercache.dev', 'commit', '--quiet', '-m', 'Cloud Turbo fixture'], template);
  return template;
}
function connect(origin: string, user: 'alice' | 'bob', project: string): Workspace {
  const directory = path.join(root, user);
  fs.mkdirSync(directory);
  const gh = path.join(directory, 'gh');
  fs.writeFileSync(gh, `#!/bin/sh\nprintf 'fixture-github-${user}\\n'\n`, { mode: 0o700 });
  const config = path.join(directory, 'connected.json');
  const env = { ...process.env, PATH: directory + path.delimiter + process.env.PATH, XDG_CACHE_HOME: path.join(directory, 'xdg-cache'), XDG_CONFIG_HOME: path.join(directory, 'xdg-config') };
  const result = JSON.parse(command(binary!, ['connect', '--cloud', origin, '--project', project, '--github-cli', '--config', config, '--json'], directory, env)) as { connected: boolean; project: string };
  assert.equal(result.connected, true);
  assert.equal(result.project, project);
  const saved = JSON.parse(fs.readFileSync(config, 'utf8')) as { projectId: string; teamUrl: string; dataDir: string; listen: string; teamToken: string };
  assert.equal(saved.projectId, project);
  assert.equal(saved.teamUrl, origin);
  assert.ok(saved.teamToken);
  assert.equal(fs.statSync(config).mode & 0o077, 0, 'connected config is not owner-only');
  return { path: directory, config, cache: saved.dataDir, listen: saved.listen, projectId: saved.projectId, teamToken: saved.teamToken };
}
async function localDaemon(workspace: Workspace): Promise<ChildProcess> {
  const log = fs.openSync(path.join(workspace.path, 'daemon.log'), 'w', 0o600);
  const child = spawn(binary!, ['serve', '--config', workspace.config], { stdio: ['ignore', log, log] });
  fs.closeSync(log);
  children.add(child);
  await ready('http://' + workspace.listen);
  return child;
}
function runTurbo(workspace: Workspace, checkout: string): { report: Report; summary: Summary; output: string } {
  const result = spawnSync(binary!, ['run', '--config', workspace.config, '--', turbo!, 'run', 'build', '--cache=remote:rw', '--summarize'], {
    cwd: checkout, env: { ...process.env, EXECUTION_COUNTER: counter }, encoding: 'utf8', timeout: 90_000, maxBuffer: 4 << 20,
  });
  assert.equal(result.status, 0, `Turbo failed: ${result.error || result.stderr || result.stdout}`);
  assert.doesNotMatch(result.stderr, /bypassing Layer Cache|local daemon is unavailable|Run Summary reconciliation failed/);
  const runId = result.stderr.match(/^Layer Cache run: (run-\S+)$/m)?.[1];
  assert.ok(runId, 'CLI omitted Layer Cache run ID');
  const report = JSON.parse(command(binary!, ['report', '--config', workspace.config, '--run', runId, '--json'], checkout)) as Report;
  const summaries = fs.readdirSync(path.join(checkout, '.turbo/runs')).filter(name => name.endsWith('.json'));
  assert.equal(summaries.length, 1, 'expected one native Turbo summary');
  const summary = JSON.parse(fs.readFileSync(path.join(checkout, '.turbo/runs', summaries[0]), 'utf8')) as Summary;
  assert.equal(summary.tasks.length, 1, 'expected one native Turbo task');
  return { report, summary, output: result.stdout + result.stderr };
}
function executions(): number {
  return fs.existsSync(counter) ? fs.readFileSync(counter, 'utf8').trim().split('\n').length : 0;
}

let fixture: ChildProcess | undefined;
let passed = false;
try {
  fixture = spawn(process.execPath, [path.resolve('qa/cloud/fixture.ts')], { stdio: ['ignore', 'pipe', 'inherit'], env: process.env });
  children.add(fixture);
  const origin = await fixtureOrigin(fixture);
  const alice = await login(origin, 'alice');
  const bob = await login(origin, 'bob');
  const team = await api<{ id: string }>(origin, alice, 'POST', '/api/teams', { name: 'Cache beta' }, 201);
  const project = await api<{ id: string }>(origin, alice, 'POST', `/api/teams/${team.id}/projects`, { name: 'Web', repository: 'acme/web' }, 201);
  const invitation = await api<{ id: string }>(origin, alice, 'POST', `/api/teams/${team.id}/invitations`, { login: 'bob', role: 'reader' }, 201);
  await api<void>(origin, bob, 'POST', `/api/invitations/${invitation.id}/accept`, undefined, 204);

  const producer = connect(origin, 'alice', project.id);
  const consumer = connect(origin, 'bob', project.id);
  assert.notEqual(producer.cache, consumer.cache, 'users share a Local Cache directory');
  const template = makeTemplate();
  const coldCheckout = path.join(root, 'checkout-alice');
  const warmCheckout = path.join(root, 'checkout-bob');
  makeWorkspace(template, coldCheckout);
  makeWorkspace(template, warmCheckout);
  assert.equal(executions(), 0);
  const aliceDaemon = await localDaemon(producer);
  const cold = runTurbo(producer, coldCheckout);
  assert.equal(executions(), 1, 'cold Turbo task did not execute once');
  assert.equal(cold.summary.tasks[0].cache.status.toUpperCase(), 'MISS');
  assert.equal(cold.report.eligible, 1);
  assert.equal(cold.report.misses, 1);
  assert.equal(cold.report.hits, 0);
  assert.equal(cold.report.degraded, false);
  assert.ok(cold.report.bytes.uploaded > 0, 'cold build did not upload cache bytes');
  const coldOutput = path.join(coldCheckout, 'packages/app/dist/result.txt');
  assert.equal(fs.readFileSync(coldOutput, 'utf8'), 'layercache turbo fixture\n', 'cold task wrote unexpected output');
  const coldDigest = sha256(coldOutput);
  await stop(aliceDaemon);
  assert.equal(fs.existsSync(path.join(warmCheckout, 'packages/app/dist')), false, 'consumer inherited producer output');
  assert.equal(fs.existsSync(path.join(warmCheckout, '.turbo')), false, 'consumer inherited native Turbo cache');
  assert.deepEqual(fs.readdirSync(consumer.cache), ['.layercache-owned.json'], 'consumer Local Cache contains cache data before restore');
  const bobDaemon = await localDaemon(consumer);
  const warm = runTurbo(consumer, warmCheckout);
  assert.equal(executions(), 1, 'warm Turbo task executed rather than restoring');
  assert.equal(warm.summary.tasks[0].cache.status.toUpperCase(), 'HIT');
  assert.equal(warm.summary.tasks[0].cache.source?.toUpperCase(), 'REMOTE');
  assert.equal(warm.report.eligible, 1);
  assert.equal(warm.report.hits, 1);
  assert.equal(warm.report.misses, 0);
  assert.equal(warm.report.degraded, false);
  assert.equal(warm.report.outcomes[0]?.source, 'teamCache', 'warm source was not Team Cache');
  assert.ok(warm.report.bytes.downloaded > 0, 'warm build did not download cache bytes');
  const restoredOutput = path.join(warmCheckout, 'packages/app/dist/result.txt');
  assert.equal(sha256(restoredOutput), coldDigest, 'restored bytes differ from cold output');
  await stop(bobDaemon);
  const status = () => fetch(origin + '/v1/status', { headers: { Authorization: 'Bearer ' + consumer.teamToken } });
  assert.equal((await status()).status, 200, 'invited reader lost project access before removal');
  await api<void>(origin, alice, 'DELETE', `/api/teams/${team.id}/members/2`, undefined, 204);
  assert.equal((await status()).status, 401, 'previously issued reader capability survived removal');

  const evidence = {
    cloud: 'loopback fixture', github: 'emulated', users: ['alice', 'bob'],
    project: project.id, separateLocalCaches: producer.cache !== consumer.cache,
    producerStoppedBeforeRestore: true, executions: executions(), removedReaderDenied: true,
    cold: { runId: cold.report.runId, turbo: cold.summary.tasks[0].cache, misses: cold.report.misses, uploadedBytes: cold.report.bytes.uploaded },
    warm: { runId: warm.report.runId, turbo: warm.summary.tasks[0].cache, hits: warm.report.hits, source: warm.report.outcomes[0].source, downloadedBytes: warm.report.bytes.downloaded },
    restoredSHA256: coldDigest, restoredOutputBytes: fs.statSync(restoredOutput).size,
  };
  const evidencePath = path.join(output, 'turbo-restore.json');
  fs.writeFileSync(evidencePath, JSON.stringify(evidence, null, 2) + '\n', { mode: 0o600 });
  console.log(JSON.stringify({ passed: true, evidence: evidencePath, ...evidence }));
  passed = true;
} finally {
  await Promise.all([...children].reverse().map(stop));
  if (passed) fs.rmSync(root, { recursive: true, force: true });
}
