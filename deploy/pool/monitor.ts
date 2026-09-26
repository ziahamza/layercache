import { readFile, stat, writeFile, rename } from 'node:fs/promises';
import { get } from 'node:https';
import { TLSSocket } from 'node:tls';
import { execFileSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';

export interface ProjectProbe { name: string; configFile: string; configOwnerUID?: number }
export interface PublicProbe { name: string; url: string }
export interface ProbeResult {
  name: string; issues: string[];
  diagnostic?: string;
  pool?: { usedBytes: number; availableBytes: number; hardBytes: number; hostAvailableBytes: number };
  report?: { runs: number; hits: number; eligible: number; hitRate: number | null; netEstimatedBuildTimeSavedMS: number | null; timingKnown: number; timingTotal: number };
}

export async function probePublic(probe: PublicProbe): Promise<ProbeResult> {
  const result: ProbeResult = { name: probe.name, issues: [] };
  try {
    const url = new URL(probe.url);
    if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash) throw new Error('unsafe-url');
    const expires = await new Promise<number>((resolve, reject) => {
      const request = get(url, (response) => {
        const socket = response.socket;
        const expiry = socket instanceof TLSSocket ? Date.parse(socket.getPeerCertificate().valid_to) : NaN;
        if (response.statusCode !== 200 || !Number.isFinite(expiry)) {
          response.destroy();
          reject(new Error(response.statusCode !== 200 ? `http-${response.statusCode}` : 'certificate-unavailable'));
          return;
        }
        let bytes = 0;
        response.on('data', (chunk: Buffer) => { bytes += chunk.length; if (bytes > 64 * 1024) response.destroy(new Error('body-limit')); });
        response.on('error', reject);
        response.on('end', () => resolve(expiry));
      });
      const timeout = setTimeout(() => request.destroy(new Error('timeout')), 10_000);
      request.on('close', () => clearTimeout(timeout));
      request.on('error', reject);
    });
    if (expires - Date.now() < 14 * 86400_000) result.issues.push('certificate-expires-within-14-days');
  } catch (error) {
    result.issues.push('public-probe-failed');
    // Keep the public URL and raw error message out of the monitoring result.
    // Stable, bounded categories let the external runner distinguish DNS,
    // transport, TLS, and HTTP failures without disclosing request details.
    if (error instanceof Error) {
      const code = 'code' in error && typeof error.code === 'string' ? error.code : error.message;
      if (/^(?:E[A-Z0-9_]+|ERR_[A-Z0-9_]+|UNABLE_TO_VERIFY_LEAF_SIGNATURE|DEPTH_ZERO_SELF_SIGNED_CERT|CERT_HAS_EXPIRED|ERR_TLS_CERT_ALTNAME_INVALID)$/.test(code) && code.length <= 64) result.diagnostic = code;
      else if (/^http-[1-5][0-9]{2}$/.test(code) || ['timeout', 'unsafe-url', 'certificate-unavailable', 'body-limit'].includes(code)) result.diagnostic = code;
      else result.diagnostic = 'unknown';
    }
  }
  return result;
}

function record(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('invalid-object');
  return value as Record<string, unknown>;
}
function number(value: unknown, minimum = 0): number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < minimum) throw new Error('invalid-number');
  return value;
}
async function json(url: URL, headers: Record<string, string>) {
  const response = await fetch(url, { headers, redirect: 'error', signal: AbortSignal.timeout(10_000) });
  if (!response.ok || !response.body) { await response.body?.cancel(); throw new Error('request'); }
  const chunks: Uint8Array[] = []; let size = 0;
  for await (const chunk of response.body) {
    size += chunk.byteLength;
    if (size > 64 * 1024) throw new Error('response-too-large');
    chunks.push(chunk);
  }
  return record(JSON.parse(Buffer.concat(chunks).toString('utf8')));
}

export interface MaintenanceObservation { result: string; exitCode: number; finishedAt: number }
export function assessMaintenance(observation: MaintenanceObservation, previousSuccess: number | null, now = Date.now()) {
  const issues: string[] = [];
  const lastSuccessAt = observation.result === 'success' && observation.exitCode === 0 && Number.isFinite(observation.finishedAt) && observation.finishedAt > 0 && observation.finishedAt <= now
    ? observation.finishedAt : previousSuccess;
  if (observation.result !== 'success' || ![0, 75].includes(observation.exitCode)) issues.push('registry-maintenance-failed');
  if (lastSuccessAt === null) issues.push('registry-maintenance-never-observed');
  else if (now - lastSuccessAt > 12 * 3600_000) issues.push('registry-maintenance-stale');
  return { issues, lastSuccessAt };
}

export async function probeProject(endpoint: string, project: ProjectProbe): Promise<ProbeResult> {
  const result: ProbeResult = { name: project.name, issues: [] };
  try {
    const metadata = await stat(project.configFile);
    if (!metadata.isFile() || (metadata.mode & 0o077) !== 0 || metadata.uid !== (project.configOwnerUID ?? process.getuid?.())) throw new Error('protected-file');
    const target = new URL(endpoint);
    if (target.protocol !== 'https:' && !(target.protocol === 'http:' && ['127.0.0.1', '[::1]', 'localhost'].includes(target.hostname))) throw new Error('unsafe-endpoint');
    if (target.username || target.password || target.search || target.hash || target.pathname !== '/') throw new Error('unsafe-endpoint');
    if (metadata.size > 1024 * 1024) throw new Error('configuration-too-large');
    const config = record(JSON.parse(await readFile(project.configFile, 'utf8')));
    if (typeof config.localToken !== 'string' || !config.localToken || typeof config.projectId !== 'string' || !config.projectId) throw new Error('invalid-config');
    const headers = { Authorization: `Bearer ${config.localToken}`, 'X-LayerCache-Project': config.projectId };
    const status = await json(new URL('/v1/status', endpoint), headers);
    if (status.running !== true) result.issues.push('project-not-running');
    if (status.cloudMaintenanceHealthy !== true) result.issues.push('cloud-maintenance-unhealthy');
    try {
      const pool = record(status.storagePool);
      const hardBytes = number(pool.hardBytes, 1);
      const filesystemBytes = number(pool.filesystemBytes, 1);
      const usedBytes = number(pool.usedBytes);
      const availableBytes = number(pool.availableBytes);
      const reserveBytes = number(pool.reserveBytes, 1);
      const hostAvailableBytes = number(pool.hostAvailableBytes);
      const hostReserveBytes = number(pool.hostReserveBytes, 1);
      result.pool = { usedBytes, availableBytes, hardBytes, hostAvailableBytes };
      if (filesystemBytes > hardBytes || usedBytes > hardBytes) result.issues.push('pool-hard-cap-invalid');
      if (pool.available !== true || availableBytes <= reserveBytes) result.issues.push('pool-reserve-exhausted');
      else if (availableBytes - reserveBytes < 2 * 1024 ** 3) result.issues.push('pool-reserve-near');
      if (pool.hostProbeHealthy !== true) result.issues.push('host-capacity-unknown');
      if (hostAvailableBytes <= hostReserveBytes) result.issues.push('host-reserve-exhausted');
      else if (hostAvailableBytes - hostReserveBytes < 2 * 1024 ** 3) result.issues.push('host-reserve-near');
    } catch { result.issues.push('pool-status-invalid'); }
    try {
      const reportURL = new URL('/v1/reports', endpoint);
      const now = Date.now();
      reportURL.searchParams.set('from', new Date(now - 86400_000).toISOString());
      reportURL.searchParams.set('to', new Date(now).toISOString());
      const report = await json(reportURL, headers);
      const estimate = record(report.netEstimatedBuildTimeSaved);
      const known = number(estimate.known), total = number(estimate.total);
      const eligible = number(report.eligible), hits = number(report.hits);
      if (hits > eligible || known > total) throw new Error('invalid-report');
      result.report = { runs: number(report.runs), hits, eligible, hitRate: eligible ? hits / eligible : null,
        netEstimatedBuildTimeSavedMS: known > 0 ? number(estimate.milliseconds, -Number.MAX_VALUE) : null,
        timingKnown: known, timingTotal: total };
    } catch { result.issues.push('report-unavailable'); }
  } catch {
    result.issues.push('project-probe-failed');
  }
  return result;
}

async function protectedJSON(path: string) {
  const metadata = await stat(path);
  if (!metadata.isFile() || metadata.uid !== process.getuid?.() || (metadata.mode & 0o077) !== 0 || metadata.size > 1024 * 1024) throw new Error('protected-config-invalid');
  return record(JSON.parse(await readFile(path, 'utf8')));
}
function text(value: unknown): string {
  if (typeof value !== 'string' || !value || value.length > 4096) throw new Error('invalid-string');
  return value;
}
function label(value: unknown): string {
  const name = text(value);
  if (!/^[a-zA-Z0-9_-]{1,64}$/.test(name)) throw new Error('invalid-name');
  return name;
}

async function main() {
  const args = process.argv.slice(2);
  const checks: ProbeResult[] = [];
  let delivery = 'unconfigured-local-journal-only';
  let stateFile: string | undefined;
  let lastMaintenanceSuccessAt: number | null = null;
  if (args.length === 2 && args[0] === '--public') {
    checks.push(await probePublic({ name: 'public-health', url: args[1]! }));
    delivery = 'github-actions-or-caller';
  } else if (args.length === 4 && args[0] === '--config' && args[2] === '--state') {
    if (process.getuid?.() !== 0) throw new Error('root-required');
    const config = await protectedJSON(args[1]!);
    stateFile = resolve(args[3]!);
    try {
      const previous = await protectedJSON(stateFile);
      if (previous.lastMaintenanceSuccessAt !== null) lastMaintenanceSuccessAt = number(previous.lastMaintenanceSuccessAt);
    } catch (error) {
      if (!(error instanceof Error && 'code' in error && error.code === 'ENOENT')) throw error;
    }
    const projects = config.projects, publicChecks = config.publicChecks;
    if (!Array.isArray(projects) || !projects.length || projects.length > 32 || !Array.isArray(publicChecks) || publicChecks.length > 16) throw new Error('invalid-checks');
    const endpoint = text(config.endpoint);
    for (const item of projects) {
      const project = record(item);
      const configOwnerUID = project.configOwnerUID === undefined ? 0 : number(project.configOwnerUID);
      if (!Number.isSafeInteger(configOwnerUID)) throw new Error('invalid-owner');
      checks.push(await probeProject(endpoint, { name: label(project.name), configFile: text(project.configFile), configOwnerUID }));
    }
    for (const item of publicChecks) {
      const check = record(item);
      checks.push(await probePublic({ name: label(check.name), url: text(check.url) }));
    }
    const unit = text(config.maintenanceUnit);
    if (!/^[a-zA-Z0-9_-]+\.service$/.test(unit)) throw new Error('invalid-unit');
    try {
      const output = execFileSync('systemctl', ['show', unit, '--property=LoadState,Result,ExecMainStatus,ExecMainExitTimestamp'], {
        encoding: 'utf8', timeout: 10_000, maxBuffer: 8192, stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, LC_ALL: 'C', TZ: 'UTC' },
      });
      const fields = Object.fromEntries(output.trim().split('\n').map(line => { const index = line.indexOf('='); return [line.slice(0, index), line.slice(index + 1)]; }));
      if (fields.LoadState !== 'loaded') throw new Error('unit-unavailable');
      const maintenance = assessMaintenance({ result: fields.Result ?? '', exitCode: Number(fields.ExecMainStatus), finishedAt: Date.parse(fields.ExecMainExitTimestamp ?? '') }, lastMaintenanceSuccessAt);
      lastMaintenanceSuccessAt = maintenance.lastSuccessAt;
      checks.push({ name: 'registry-maintenance', issues: maintenance.issues });
    } catch { checks.push({ name: 'registry-maintenance', issues: ['registry-maintenance-status-unavailable'] }); }
  } else throw new Error('usage');
  const report = { checkedAt: new Date().toISOString(), healthy: checks.every(check => check.issues.length === 0), delivery, lastMaintenanceSuccessAt, checks };
  if (stateFile) {
    // One bounded latest snapshot, atomically replaced. The installer owns the
    // parent directory; no append-only history or secret-bearing responses.
    const temporary = `${stateFile}.tmp`;
    await writeFile(temporary, JSON.stringify(report) + '\n', { mode: 0o600 });
    await rename(temporary, stateFile);
  }
  console.log(JSON.stringify(report));
  if (!report.healthy) process.exitCode = 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(() => { console.error('LayerCache monitor could not complete; check protected configuration and service availability.'); process.exitCode = 1; });
}
