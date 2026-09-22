import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync, rmSync, statSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setupBuildkit, cacheOptions, writeConfig, readConfig } from './main.ts';
import { cleanup } from './post.ts';

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'buildkit-test-'));
  const env = { INPUT_NAMESPACE: 'gitenv', INPUT_SCOPE: 'sandbox', INPUT_COMPATIBILITY: 'linux-v1', 'INPUT_TEAM-URL': 'https://cache.example', GITHUB_REPOSITORY: 'GitStartHQ/gitenv', GITHUB_OUTPUT: join(dir, 'outputs'), GITHUB_STATE: join(dir, 'state'), DOCKER_CONFIG: dir, RUNNER_TEMP: dir, ACTIONS_ID_TOKEN_REQUEST_URL: 'https://github.example/token', ACTIONS_ID_TOKEN_REQUEST_TOKEN: 'request-secret' };
  writeFileSync(join(dir, 'config.json'), JSON.stringify({ auths: { 'ghcr.io': { auth: 'original-ghcr' }, 'cache.example': { auth: 'original-cache' } }, other: true }));
  return { dir, env, dispose: () => rmSync(dir, { recursive: true, force: true }) };
}
function credential(write = true) { return `lc2.${Buffer.from(JSON.stringify({ project: 'github.com/gitstarthq/gitenv', integration: 'buildkit', capabilities: [write ? 'write' : 'read'] })).toString('base64url')}.signature`; }
function fetcher(write = true): typeof fetch { return async (_url, options) => {
  if (options?.method === 'POST') { assert.equal(JSON.parse(String(options.body)).integration, 'buildkit'); return Response.json({ teamToken: credential(write), expiresAt: new Date(Date.now() + 600_000).toISOString() }); }
  return Response.json({ value: 'identity-secret' });
}; }
const login = (dir: string, registry: string, namespace: string, token: string) => {
  assert.equal(namespace, 'gitenv'); assert.equal(token, credential(token === credential()));
  writeFileSync(join(dir, 'config.json'), JSON.stringify({ auths: { [registry]: { auth: Buffer.from(`${namespace}:${token}`).toString('base64') } } }));
};
function state(path: string) { const text = readFileSync(path, 'utf8'); return text.split('\n')[1]!; }

test('setup preserves GHCR and post restores previous registry auth', async () => {
  const f = fixture(); try {
    const logs: string[] = [];
    await setupBuildkit(f.env, { fetcher: fetcher(), login, log: text => { logs.push(text); } });
    const outputs = readFileSync(f.env.GITHUB_OUTPUT, 'utf8');
    assert.match(outputs, /enabled<<[^\n]+\ntrue\n/); assert.match(outputs, /ignore-error=true/);
    assert.equal(JSON.parse(readFileSync(join(f.dir, 'config.json'), 'utf8')).auths['ghcr.io'].auth, 'original-ghcr');
    assert.ok(logs.some(line => line.startsWith('::add-mask::lc2.')));
    cleanup({ STATE_layercache_buildkit: state(f.env.GITHUB_STATE) });
    assert.equal(JSON.parse(readFileSync(join(f.dir, 'config.json'), 'utf8')).auths['cache.example'].auth, 'original-cache');
  } finally { f.dispose(); }
});
test('read-only token does not expose cache export', async () => {
  const f = fixture(); try {
    await setupBuildkit(f.env, { fetcher: fetcher(false), login, log: () => {} });
    assert.doesNotMatch(readFileSync(f.env.GITHUB_OUTPUT, 'utf8'), /mode=max/);
  } finally { f.dispose(); }
});
test('failure is fail-open without printing service response or Docker stderr', async () => {
  for (const failure of ['exchange', 'docker']) {
    const f = fixture(); try {
      const logs: string[] = [];
      await setupBuildkit(f.env, { fetcher: failure === 'exchange' ? async () => new Response('server-secret', { status: 500 }) : fetcher(), login: () => { throw new Error('docker-secret'); }, log: text => { logs.push(text); } });
      assert.doesNotMatch(logs.join(''), /server-secret|docker-secret/);
      assert.doesNotMatch(readFileSync(f.env.GITHUB_OUTPUT, 'utf8'), /\ntrue\n/);
      assert.equal(JSON.parse(readFileSync(join(f.dir, 'config.json'), 'utf8')).auths['cache.example'].auth, 'original-cache');
    } finally { f.dispose(); }
  }
});
test('invalid namespace, scope, compatibility, ttl and URL fail before authentication', async () => {
  for (const override of [{ INPUT_NAMESPACE: '../escape' }, { INPUT_SCOPE: 'x,mode=min' }, { INPUT_COMPATIBILITY: '' }, { 'INPUT_TTL-MINUTES': '61' }, { 'INPUT_TEAM-URL': 'http://cache.example' }]) {
    const f = fixture(); try {
      await setupBuildkit({ ...f.env, ...override }, { fetcher: async () => { assert.fail('authentication must not run'); }, log: () => {} });
      assert.doesNotMatch(readFileSync(f.env.GITHUB_OUTPUT, 'utf8'), /\ntrue\n/);
    } finally { f.dispose(); }
  }
});
test('post does not clobber later unrelated login; repeated calls unwind safely', async () => {
  const f = fixture(); try {
    await setupBuildkit(f.env, { fetcher: fetcher(), login, log: () => {} });
    const first = state(f.env.GITHUB_STATE);
    const secondState = join(f.dir, 'state2');
    await setupBuildkit({ ...f.env, GITHUB_STATE: secondState }, { fetcher: fetcher(false), login, log: () => {} });
    cleanup({ STATE_layercache_buildkit: state(secondState) });
    cleanup({ STATE_layercache_buildkit: first });
    assert.equal(JSON.parse(readFileSync(join(f.dir, 'config.json'), 'utf8')).auths['cache.example'].auth, 'original-cache');
    writeFileSync(join(f.dir, 'config.json'), JSON.stringify({ auths: { 'cache.example': { auth: 'new-login' } } }));
    cleanup({ STATE_layercache_buildkit: first });
    assert.equal(JSON.parse(readFileSync(join(f.dir, 'config.json'), 'utf8')).auths['cache.example'].auth, 'new-login');
  } finally { f.dispose(); }
});
test('credential helpers remain untouched and compatibility separates tags', async () => {
  const f = fixture(); try {
    writeFileSync(join(f.dir, 'config.json'), JSON.stringify({ credsStore: 'osxkeychain' }));
    await setupBuildkit(f.env, { fetcher: async () => { assert.fail('must not authenticate'); }, log: () => {} });
    assert.equal(JSON.parse(readFileSync(join(f.dir, 'config.json'), 'utf8')).credsStore, 'osxkeychain');
    assert.notEqual(cacheOptions('cache.example', 'gitenv', 'sandbox', 'v1', true)['cache-from'], cacheOptions('cache.example', 'gitenv', 'sandbox', 'v2', true)['cache-from']);
  } finally { f.dispose(); }
});
test('atomic config replacement preserves existing credentials on filesystem failure', () => {
  const f = fixture(); try {
    const path = join(f.dir, 'config.json');
    const before = readFileSync(path, 'utf8');
    assert.throws(() => writeConfig(path, { auths: {} }, () => { throw new Error('injected rename failure'); }));
    assert.equal(readFileSync(path, 'utf8'), before);
    chmodSync(path, 0o644);
    writeConfig(path, { auths: {} });
    assert.equal(statSync(path).mode & 0o777, 0o600);
  } finally { f.dispose(); }
});
test('invalid Docker config cannot be replaced with a partial config', () => {
  const f = fixture(); try {
    for (const value of [null, [], { auths: [] }, { credHelpers: { host: 3 } }]) {
      const path = join(f.dir, 'config.json');
      writeFileSync(path, JSON.stringify(value));
      assert.throws(() => readConfig(path));
    }
  } finally { f.dispose(); }
});
test('setup rereads credentials modified during authentication', async () => {
  const f = fixture(); try {
    await setupBuildkit(f.env, { fetcher: fetcher(), login: (...args) => {
      login(...args);
      writeFileSync(join(f.dir, 'config.json'), JSON.stringify({ auths: { 'ghcr.io': { auth: 'updated-ghcr' } } }));
    }, log: () => {} });
    assert.equal(readConfig(join(f.dir, 'config.json')).auths?.['ghcr.io'] && JSON.parse(readFileSync(join(f.dir, 'config.json'), 'utf8')).auths['ghcr.io'].auth, 'updated-ghcr');
  } finally { f.dispose(); }
});
