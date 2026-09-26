import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { createServer as createSecureServer } from 'node:https';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { spawnSync, spawn, execFileSync } from 'node:child_process';
import { probeProject, assessMaintenance, probePublic } from './monitor.ts';

test('authenticated project probe reports maintenance failure without exposing credentials', async (t) => {
  const dir = await mkdtemp(join(tmpdir(), 'layercache-monitor-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const configFile = join(dir, 'config.json');
  await writeFile(configFile, JSON.stringify({ projectId: 'team-test', localToken: 'private-token' }), { mode: 0o600 });
  const server = createServer((request, response) => {
    assert.equal(request.headers.authorization, 'Bearer private-token');
    assert.equal(request.headers['x-layercache-project'], 'team-test');
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({ running: true, cloudMaintenanceHealthy: false }));
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  const result = await probeProject(`http://127.0.0.1:${address.port}`, { name: 'test', configFile });
  assert.ok(result.issues.includes('cloud-maintenance-unhealthy'));
  assert.ok(!JSON.stringify(result).includes('private-token'));
});

test('public probe refuses plaintext and reports TLS connection failures without leaking URL queries', async () => {
  assert.deepEqual(await probePublic({ name: 'plaintext', url: 'http://127.0.0.1/healthz' }), { name: 'plaintext', issues: ['public-probe-failed'], diagnostic: 'unsafe-url' });
  const offline = await probePublic({ name: 'offline', url: 'https://127.0.0.1:1/healthz?secret=hidden' });
  assert.deepEqual(offline, { name: 'offline', issues: ['public-probe-failed'], diagnostic: 'unsafe-url' });
  assert.ok(!JSON.stringify(offline).includes('hidden'));
  const refused = await probePublic({ name: 'refused', url: 'https://127.0.0.1:1/healthz' });
  assert.deepEqual(refused, { name: 'refused', issues: ['public-probe-failed'], diagnostic: 'ECONNREFUSED' });
});

test('external probe CLI exits nonzero with bounded safe JSON on unhealthy endpoint', () => {
  const result = spawnSync(process.execPath, ['deploy/pool/monitor.ts', '--public', 'http://127.0.0.1/?secret=hidden'], { encoding: 'utf8' });
  assert.equal(result.status, 1);
  const report = JSON.parse(result.stdout);
  assert.equal(report.healthy, false);
  assert.equal(report.delivery, 'github-actions-or-caller');
  assert.deepEqual(report.checks[0].issues, ['public-probe-failed']);
  assert.equal(report.checks[0].diagnostic, 'unsafe-url');
  assert.ok(!result.stdout.includes('hidden'));
});

test('installer defaults to a read-only plan with bounded timer and state', () => {
  const result = spawnSync(process.execPath, ['deploy/pool/install-monitor.ts', '/etc/layercache/monitor.json'], { encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /OnUnitActiveSec=5min/);
  assert.match(result.stdout, /StateDirectory=layercache-monitor/);
  assert.match(result.stdout, /NoNewPrivileges=true/);
  assert.match(result.stdout, /Dry run/);
});

test('external probe verifies trusted HTTPS health and warns before certificate expiry', async (t) => {
  const dir = await mkdtemp(join(tmpdir(), 'layercache-monitor-tls-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const cert = join(dir, 'cert.pem'), key = join(dir, 'key.pem');
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-subj', '/CN=localhost', '-addext', 'subjectAltName=IP:127.0.0.1', '-keyout', key, '-out', cert], { stdio: 'ignore' });
  const server = createSecureServer({ key: await readFile(key), cert: await readFile(cert) }, (_request, response) => response.end('ok'));
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const address = server.address(); assert.ok(address && typeof address !== 'string');
  const child = spawn(process.execPath, ['deploy/pool/monitor.ts', '--public', `https://127.0.0.1:${address.port}/healthz`], { env: { ...process.env, NODE_EXTRA_CA_CERTS: cert }, stdio: ['ignore', 'pipe', 'pipe'] });
  let output = ''; child.stdout.on('data', chunk => { output += chunk; });
  const code = await new Promise<number | null>((resolve) => child.on('close', resolve));
  assert.equal(code, 1);
  assert.deepEqual(JSON.parse(output).checks[0].issues, ['certificate-expires-within-14-days']);
});

test('failed collection and stale successful collection both raise issues; skips do not renew success', () => {
  const now = Date.parse('2026-09-22T12:00:00Z');
  assert.deepEqual(assessMaintenance({ result: 'exit-code', exitCode: 1, finishedAt: now }, null, now).issues, ['registry-maintenance-failed', 'registry-maintenance-never-observed']);
  assert.deepEqual(assessMaintenance({ result: 'success', exitCode: 0, finishedAt: now - 13 * 3600_000 }, null, now).issues, ['registry-maintenance-stale']);
  const skipped = assessMaintenance({ result: 'success', exitCode: 75, finishedAt: now }, now - 13 * 3600_000, now);
  assert.deepEqual(skipped.issues, ['registry-maintenance-stale']);
  assert.equal(skipped.lastSuccessAt, now - 13 * 3600_000);
  assert.deepEqual(assessMaintenance({ result: 'success', exitCode: 0, finishedAt: now }, null, now).issues, []);
});

test('project status catches exhausted capacity and preserves unknown ROI rather than claiming savings', async (t) => {
  const dir = await mkdtemp(join(tmpdir(), 'layercache-monitor-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const configFile = join(dir, 'config.json');
  await writeFile(configFile, JSON.stringify({ projectId: 'team-test', localToken: 'private-token' }), { mode: 0o600 });
  const server = createServer((request, response) => {
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify(request.url?.startsWith('/v1/reports') ? {
      runs: 3, hits: 0, eligible: 0, hitRate: 0,
      netEstimatedBuildTimeSaved: { milliseconds: 0, known: 0, total: 3, confidence: 'unknown' },
    } : {
      running: true, cloudMaintenanceHealthy: true, storageBackend: 'postgresql+s3',
      storagePool: { available: false, hardBytes: 48 * 1024 ** 3, filesystemBytes: 47 * 1024 ** 3,
        usedBytes: 40 * 1024 ** 3, availableBytes: 7 * 1024 ** 3, reserveBytes: 10 * 1024 ** 3,
        hostProbeHealthy: true, hostAvailableBytes: 200 * 1024 ** 3, hostReserveBytes: 10 * 1024 ** 3 },
    }));
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const address = server.address(); assert.ok(address && typeof address !== 'string');
  const result = await probeProject(`http://127.0.0.1:${address.port}`, { name: 'test', configFile });
  assert.ok(result.issues.includes('pool-reserve-exhausted'));
  assert.equal(result.report?.netEstimatedBuildTimeSavedMS, null);
  assert.equal(result.report?.eligible, 0);
  assert.equal(result.report?.hitRate, null);
});

test('project probes reject oversized responses and credentials readable by other users', async (t) => {
  const dir = await mkdtemp(join(tmpdir(), 'layercache-monitor-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const configFile = join(dir, 'config.json');
  await writeFile(configFile, JSON.stringify({ projectId: 'team-test', localToken: 'private-token' }), { mode: 0o600 });
  const server = createServer((_request, response) => response.end('x'.repeat(70 * 1024)));
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const address = server.address(); assert.ok(address && typeof address !== 'string');
  const endpoint = `http://127.0.0.1:${address.port}`;
  assert.deepEqual((await probeProject(endpoint, { name: 'oversized', configFile })).issues, ['project-probe-failed']);
  const publicConfig = join(dir, 'public.json');
  await writeFile(publicConfig, JSON.stringify({ projectId: 'team-test', localToken: 'private-token' }), { mode: 0o644 });
  assert.deepEqual((await probeProject(endpoint, { name: 'insecure', configFile: publicConfig })).issues, ['project-probe-failed']);
  assert.deepEqual((await probeProject(endpoint, { name: 'wrong-owner', configFile, configOwnerUID: 99999 })).issues, ['project-probe-failed']);
});
