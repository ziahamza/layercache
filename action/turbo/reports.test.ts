import assert from 'node:assert/strict';
import {
  mkdtemp,
  mkdir,
  writeFile,
  readFile,
  rm,
  symlink,
} from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import test from 'node:test';
import { setupTurbo } from './main.ts';
import { reportTurbo } from './post.ts';
import { startReport, completedSummaries } from './reports.ts';

test('action reports only new summaries with fresh OIDC, without environment secrets', async (t) => {
  const workspace = await mkdtemp(join(tmpdir(), 'turbo-report-'));
  t.after(() => rm(workspace, { recursive: true, force: true }));
  const runs = join(workspace, '.turbo/runs');
  await mkdir(runs, { recursive: true });
  await writeFile(join(runs, 'old.json'), '{}');
  const env: NodeJS.ProcessEnv = {
    'INPUT_TEAM-URL': 'https://cache.example',
    INPUT_COMPATIBILITY: 'linux-node24',
    GITHUB_REPOSITORY: 'acme/widget',
    GITHUB_WORKSPACE: workspace,
    GITHUB_ENV: 'env',
    GITHUB_OUTPUT: 'output',
    GITHUB_STATE: 'state',
    GITHUB_STEP_SUMMARY: join(workspace, 'step-summary.md'),
    ACTIONS_ID_TOKEN_REQUEST_URL: 'https://oidc.example/token',
    ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'request-token',
  };
  const states: Record<string, string> = {};
  const exports: Record<string, string> = {};
  await setupTurbo(env, {
    fetcher: async (url) =>
      Response.json(
        String(url).includes('oidc.example')
          ? { value: 'setup-identity' }
          : {
              teamToken: 'setup-token',
              expiresAt: new Date(Date.now() + 1800_000).toISOString(),
            },
      ),
    write: (path, name, value) => {
      if (path === 'state') states[name] = value;
      if (path === 'env') exports[name] = value;
    },
    log: () => {},
  });
  assert.equal(exports.TURBO_RUN_SUMMARY, 'true');
  const now = Date.now();
  await writeFile(
    join(runs, 'new.json'),
    JSON.stringify({
      id: 'new',
      version: '1',
      execution: { startTime: now, endTime: now + 1 },
      environmentVariables: { secret: 'do-not-send' },
      tasks: [
        {
          taskId: 'app#build',
          hash: 'hash',
          dependencies: [],
          command: 'secret-command',
          environmentVariables: { secret: 'do-not-send' },
          execution: { startTime: now, endTime: now + 1, exitCode: 0 },
          cache: { status: 'MISS' },
          resolvedTaskDefinition: { cache: true },
        },
      ],
    }),
  );
  let submitted: unknown;
  const logs: string[] = [];
  await reportTurbo(
    { ...env, STATE_layercache_turbo: states.layercache_turbo },
    {
      fetcher: async (url, init) => {
        if (String(url).includes('oidc.example'))
          return Response.json({ value: 'fresh-identity' });
        if (String(url).includes('/exchange'))
          return Response.json({
            teamToken: 'fresh-token',
            expiresAt: new Date(Date.now() + 1800_000).toISOString(),
          });
        assert.equal(
          new Headers(init?.headers).get('authorization'),
          'Bearer fresh-token',
        );
        submitted = JSON.parse(String(init?.body));
        return Response.json({
          runId: 'signed-run',
          tasks: 1,
          report: {
            eligible: 1,
            hits: 0,
            misses: 1,
            hitRate: 0,
            netEstimatedBuildTimeSaved: { milliseconds: 0, known: 1, total: 1 },
          },
        });
      },
      log: (value) => logs.push(value),
    },
  );
  assert.ok(submitted);
  assert.equal((submitted as { summaries: unknown[] }).summaries.length, 1);
  assert.ok(!JSON.stringify(submitted).includes('do-not-send'));
  assert.ok(!JSON.stringify(submitted).includes('secret-command'));
  assert.ok(logs.some((value) => value.includes('1 eligible')));
  assert.match(
    await readFile(env.GITHUB_STEP_SUMMARY!, 'utf8'),
    /0 hits \/ 1 eligible tasks/,
  );
});

test('missing summaries make no credential request and report failure remains a warning', async (t) => {
  const workspace = await mkdtemp(join(tmpdir(), 'turbo-report-'));
  t.after(() => rm(workspace, { recursive: true, force: true }));
  const state = await startReport(workspace, '.', {
    endpoint: 'https://cache.example',
    project: 'github.com/acme/widget',
    compatibility: 'linux-node24',
  });
  const logs: string[] = [];
  await reportTurbo(
    { STATE_layercache_turbo: JSON.stringify(state) },
    {
      fetcher: () => assert.fail('no request without summaries'),
      log: (value) => logs.push(value),
    },
  );
  assert.ok(logs.some((value) => value.includes('no new Turbo summaries')));
  await reportTurbo(
    { STATE_layercache_turbo: 'not-json-secret' },
    {
      fetcher: () => assert.fail('no request on invalid state'),
      log: (value) => logs.push(value),
    },
  );
  assert.ok(logs.some((value) => value.startsWith('::warning::')));
  assert.ok(!logs.join('').includes('not-json-secret'));
});

test('summary collector rejects symlink files and workspace escapes', async (t) => {
  const workspace = await mkdtemp(join(tmpdir(), 'turbo-report-'));
  t.after(() => rm(workspace, { recursive: true, force: true }));
  const state = await startReport(workspace, '.', {
    endpoint: 'https://cache.example',
    project: 'github.com/acme/widget',
    compatibility: 'linux-node24',
  });
  const runs = join(workspace, '.turbo/runs');
  await mkdir(runs, { recursive: true });
  await writeFile(join(workspace, 'secret.json'), '{}');
  await symlink(join(workspace, 'secret.json'), join(runs, 'new.json'));
  await assert.rejects(completedSummaries(state), /Unsafe summary/);
  await assert.rejects(
    startReport(workspace, '..', {
      endpoint: 'https://cache.example',
      project: 'github.com/acme/widget',
      compatibility: 'linux-node24',
    }),
    /escapes workspace/,
  );
});
