// Isolated real Local and Team Cache fixtures. Secrets stay in a mode-0700
// temporary directory; printed output contains no credentials.
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { chmodSync, closeSync, existsSync, mkdtempSync, openSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { setTimeout as delay } from 'node:timers/promises';

const binary = process.env.LAYERCACHE_BIN;
if (!binary) throw new Error('Set LAYERCACHE_BIN to an absolute built CLI path');
const root = mkdtempSync(join(tmpdir(), 'layercache-browser-fixture.'));
chmodSync(root, 0o700);
const processes: ChildProcess[] = [];
let stopping = false;
process.on('SIGINT', () => { stopping = true; });
process.on('SIGTERM', () => { stopping = true; });
type Config = { listen: string; localToken: string; projectId: string };
async function port(): Promise<number> {
  const server = createServer();
  await new Promise<void>((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Missing allocated port');
  await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  return address.port;
}
function launch(args: string[], name: string, pipeOutput = false): ChildProcess {
  const log = openSync(join(root, `${name}.log`), 'w', 0o600);
  const child = spawn(binary!, args, {stdio: ['ignore', pipeOutput ? 'pipe' : log, log]});
  closeSync(log);
  processes.push(child);
  return child;
}
async function setup(name: string, project: string, role = 'local', team?: Config): Promise<{path: string; config: Config}> {
  const path = join(root, `${name}.json`);
  const endpoint = `127.0.0.1:${await port()}`;
  const args = ['setup', '--config', path, '--data-dir', join(root, `${name}-cache`), '--listen', endpoint, '--role', role, '--project', project, '--max-size', '1073741824', '--non-interactive', '--json'];
  if (team) {
    const secret = join(root, `${name}-team-token`);
    writeFileSync(secret, team.localToken, {mode: 0o600});
    args.push('--team-url', `http://${team.listen}`, '--team-token-file', secret);
  }
  const result = spawnSync(binary!, args, {encoding: 'utf8'});
  if (result.error || result.status !== 0) throw new Error(`Fixture setup ${name} failed: ${result.error?.message ?? result.stderr}`);
  const config: Config = JSON.parse(readFileSync(path, 'utf8'));
  const child = launch(['serve', '--config', path], name);
  for (let attempt = 0; attempt < 100; attempt++) {
    if (child.exitCode !== null) throw new Error(`Runtime ${name} exited; inspect its fixture log`);
    try { const response = await fetch(`http://${endpoint}/healthz`, {signal: AbortSignal.timeout(200)}); if (response.ok) return {path, config}; } catch {}
    await delay(50);
  }
  throw new Error(`Runtime ${name} did not start`);
}
try {
  const team = await setup('team', 'github.com/example/console', 'team');
  const first = await setup('console', 'github.com/example/console', 'local', team.config);
  const second = await setup('api', 'github.com/example/api');
  for (const target of [team.config, first.config]) {
    const response = await fetch(`http://${target.listen}/v8/artifacts/dashboard-qa?teamId=${encodeURIComponent(target.projectId)}`, {
      method: 'PUT', body: 'QA artifact\n'.repeat(100000), signal: AbortSignal.timeout(5000),
      headers: {Authorization: `Bearer ${target.localToken}`, 'Content-Type': 'application/octet-stream', 'X-Artifact-Duration': '1500', 'X-LayerCache-Compatibility': 'linux-amd64-dashboard-qa'},
    });
    if (!response.ok) throw new Error(`Artifact seed failed with HTTP ${response.status}`);
    await response.arrayBuffer();
  }
  const dashboard = launch(['dashboard', '--config', first.path, '--config', second.path], 'dashboard', true);
  if (!dashboard.stdout) throw new Error('Dashboard stdout missing');
  let link = '';
  const lines = createInterface({input: dashboard.stdout});
  const timeout = setTimeout(() => lines.close(), 15000);
  for await (const line of lines) { if (/^http:\/\/127\.0\.0\.1:\d+\/#/.test(line.trim())) {link=line.trim();break;} }
  clearTimeout(timeout);
  if (!link) throw new Error('Dashboard did not print its link; inspect fixture log');
  writeFileSync(join(root, 'link'), link, {mode: 0o600});
  console.log(JSON.stringify({ready:true,root,linkFile:join(root,'link')}));
  console.log(`Dashboard link is in ${join(root, 'link')}; create ${join(root, 'stop')} to stop fixture processes.`);
  while (!stopping && !existsSync(join(root, 'stop'))) await delay(200);
} finally {
  for (const child of processes.reverse()) {
    if (child.exitCode !== null || child.signalCode !== null) continue;
    child.kill('SIGTERM');
    for (let i = 0; i < 50 && child.exitCode === null && child.signalCode === null; i++) await delay(100);
    if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
  }
}
