import { pathToFileURL } from 'node:url';
import { appendFile } from 'node:fs/promises';
import { endpointURL, exchangeTurbo } from '../setup/main.mjs';
import { completedSummaries, object, type ReportState } from './reports.ts';

interface Dependencies {
  fetcher?: typeof fetch;
  log?: (value: string) => void;
}
export async function reportTurbo(
  env: NodeJS.ProcessEnv = process.env,
  {
    fetcher = fetch,
    log = (value) => process.stdout.write(value),
  }: Dependencies = {},
): Promise<void> {
  if (!env.STATE_layercache_turbo) return;
  try {
    if (env.STATE_layercache_turbo.length > 256 * 1024)
      throw new Error('State too large');
    const value = object(JSON.parse(env.STATE_layercache_turbo));
    for (const field of [
      'endpoint',
      'project',
      'compatibility',
      'workspace',
      'directory',
    ])
      if (typeof value[field] !== 'string' || !value[field])
        throw new Error('Invalid report state');
    if (
      typeof value.startedAt !== 'number' ||
      !Number.isFinite(value.startedAt) ||
      value.startedAt > Date.now() ||
      Date.now() - value.startedAt > 24 * 3600_000 ||
      !Array.isArray(value.existing) ||
      value.existing.length > 4096 ||
      value.existing.some((name) => typeof name !== 'string')
    )
      throw new Error('Invalid report state');
    const state = value as unknown as ReportState;
    const endpoint = endpointURL(state.endpoint);
    if (endpoint.protocol !== 'https:' || endpoint.pathname !== '/')
      throw new Error('Invalid endpoint');
    const summaries = await completedSummaries(state);
    if (!summaries.length) {
      log(
        'Layer Cache: no new Turbo summaries; no timing or hit-rate claim recorded.\n',
      );
      return;
    }
    // The setup token may have expired during a long build. A fresh OIDC
    // exchange resolves to the same signed workflow attempt and check/job ID.
    const auth = await exchangeTurbo({
      endpoint: endpoint.origin,
      project: state.project,
      compatibility: state.compatibility,
      minutes: 5,
      env,
      fetcher,
      log,
    });
    const response = await fetcher(new URL('/v1/reports/turbo', endpoint), {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${auth.teamToken}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ summaries }),
      redirect: 'error',
      signal: AbortSignal.timeout(20_000),
    });
    if (!response.ok || !response.body) {
      await response.body?.cancel();
      throw new Error('Report rejected');
    }
    const chunks: Uint8Array[] = [];
    let bytes = 0;
    for await (const chunk of response.body) {
      bytes += chunk.length;
      if (bytes > 64 * 1024) throw new Error('Report response too large');
      chunks.push(chunk);
    }
    const result = object(JSON.parse(Buffer.concat(chunks).toString('utf8')));
    if (result.report === null) {
      log('Layer Cache: no eligible Turbo tasks in the submitted summaries.\n');
      return;
    }
    const report = object(result.report),
      estimate = object(report.netEstimatedBuildTimeSaved);
    for (const key of ['eligible', 'hits', 'misses'])
      if (
        typeof report[key] !== 'number' ||
        !Number.isSafeInteger(report[key]) ||
        (report[key] as number) < 0
      )
        throw new Error('Invalid report');
    const savings =
      typeof estimate.known === 'number' &&
      estimate.known > 0 &&
      typeof estimate.milliseconds === 'number' &&
      Number.isFinite(estimate.milliseconds)
        ? `${estimate.milliseconds} ms estimated build time saved`
        : 'build time saved unknown';
    const line = `${report.hits} hits / ${report.eligible} eligible tasks, ${report.misses} misses; ${savings}.`;
    log(`Layer Cache Turbo: ${line}\n`);
    if (env.GITHUB_STEP_SUMMARY) {
      await appendFile(
        env.GITHUB_STEP_SUMMARY,
        `\n### Layer Cache Turbo\n\n${line}\n\nEstimates are task/critical-path wall time, not CPU savings.\n`,
        { mode: 0o600 },
      );
    }
  } catch {
    log(
      '::warning::Layer Cache Turbo report unavailable. Build result is unchanged; savings are unknown to this step.\n',
    );
  }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href)
  void reportTurbo();
