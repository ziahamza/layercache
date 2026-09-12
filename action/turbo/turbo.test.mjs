import test from 'node:test';
import assert from 'node:assert/strict';
import { setupTurbo } from './main.mjs';

const env = { 'INPUT_TEAM-URL': 'https://cache.example', INPUT_COMPATIBILITY: 'linux-amd64-node24', GITHUB_REPOSITORY: 'Acme/Widget', GITHUB_ENV: 'env', GITHUB_OUTPUT: 'output', ACTIONS_ID_TOKEN_REQUEST_URL: 'https://oidc.example/token', ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'request-fixture' };
test('native Turbo action exchanges OIDC and exports only Turbo settings', async () => {
  const requests = [], writes = [], logs = [];
  await setupTurbo(env, { fetcher: async (url, init) => {
    requests.push([url, init]);
    return { ok: true, json: async () => requests.length === 1 ? { value: 'identity-fixture' } : { teamToken: 'token-fixture', expiresAt: new Date(Date.now() + 1800_000).toISOString() } };
  }, log: value => logs.push(value), write: (...args) => writes.push(args) });
  assert.equal(JSON.parse(requests[1][1].body).project, 'github.com/acme/widget');
  assert.equal(JSON.parse(requests[1][1].body).integration, 'turbo');
  assert.deepEqual(writes.filter(([path]) => path === 'env').map(([, key]) => key), ['TURBO_API', 'TURBO_TEAM', 'TURBO_TOKEN']);
  assert.ok(logs.some(line => line.includes('::add-mask::token-fixture')));
});
test('invalid configuration makes no request or environment write', async () => {
  for (const override of [{ INPUT_COMPATIBILITY: '' }, { 'INPUT_TEAM-URL': 'http://cache.example' }, { 'INPUT_TTL-MINUTES': '61' }]) {
    await assert.rejects(setupTurbo({ ...env, ...override }, { fetcher: () => assert.fail('request'), write: () => assert.fail('write') }));
  }
});
test('authentication failure exports no credential', async () => {
  await assert.rejects(setupTurbo(env, { fetcher: async () => ({ ok: false, status: 403 }), write: () => assert.fail('write') }));
});
