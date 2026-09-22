import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { createHash, randomBytes } from 'node:crypto';
import { mkdtemp, mkdir, readFile, readdir, rm, stat, symlink, writeFile, chmod, utimes, copyFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { NativeCache, artifactKey } from './client.ts';
import provider, { identityForBuild } from './expo.ts';
import { detectSimulatorTarget, validateSimulatorApp } from './expo-identity.ts';

const identity = { project: 'github.com/example/app', compatibility: 'xcode26-iossim-arm64-debug', key: 'sources-123' };
async function fixture(t: { after: (fn: () => Promise<void>) => void }) {
  const root = await mkdtemp(join(tmpdir(), 'layercache-native-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const app = join(root, 'Client.app');
  await mkdir(app);
  await writeFile(join(app, 'binary'), 'native code');
  await chmod(join(app, 'binary'), 0o755);
  return { root, app, cache: new NativeCache({ cacheDir: join(root, 'cache') }) };
}

test('fresh worktree restores local artifact, executable modes and internal symlinks', async t => {
  const { root, app, cache } = await fixture(t);
  await symlink('binary', join(app, 'Current'));
  const saved = await cache.save(identity, app);
  const restored = await new NativeCache({ cacheDir: cache.root }).restore(identity, join(root, 'new-worktree', 'build'));
  assert.equal(restored.source, 'local');
  assert.equal(restored.digest, saved.digest);
  assert.equal(await readFile(join(restored.path!, 'Client.app/Current'), 'utf8'), 'native code');
  assert.equal((await stat(join(restored.path!, 'Client.app/binary'))).mode & 0o777, 0o755);
  await assert.rejects(cache.restore(identity, restored.path), /EEXIST/);
});

test('project, compatibility and source key each isolate artifacts', async t => {
  const { cache, app } = await fixture(t);
  await cache.save(identity, app);
  for (const changed of [{ ...identity, project: 'another-project' }, { ...identity, compatibility: 'xcode27' }, { ...identity, key: 'new-js' }]) {
    assert.notEqual(artifactKey(changed), artifactKey(identity));
    assert.equal((await cache.restore(changed)).hit, false);
  }
});

test('corrupt local archive becomes a miss and is removed', async t => {
  const { cache, app } = await fixture(t);
  const saved = await cache.save(identity, app);
  await writeFile(saved.path!, 'corrupt');
  assert.equal((await cache.restore(identity)).hit, false);
  assert.deepEqual(await readdir(cache.root), []);
});

test('archive cannot escape destination through a symlink', async t => {
  const { app, cache } = await fixture(t);
  await symlink('../../outside', join(app, 'escape'));
  await assert.rejects(cache.save(identity, app), /Unsafe archive link/);
  assert.deepEqual(await readdir(cache.root), []);
});

test('archive rejects symlink chains whose lexical targets look internal', async t => {
  const { app, cache } = await fixture(t);
  await symlink('.', join(app, 'alias'));
  await symlink('alias/../../outside', join(app, 'escape'));
  await assert.rejects(cache.save(identity, app), /Unsafe archive link chain/);
});

test('concurrent worktree operations serialize and both restore verified artifacts', async t => {
  const { root, app, cache } = await fixture(t);
  await cache.save(identity, app);
  const results = await Promise.all([
    cache.restore(identity, join(root, 'first')),
    new NativeCache({ cacheDir: cache.root }).restore(identity, join(root, 'second')),
  ]);
  assert.ok(results.every(result => result.hit));
  assert.equal(results[0]!.digest, results[1]!.digest);
});

test('bounded cache evicts least recently used archives without deleting unrelated files', async t => {
  const { root, app } = await fixture(t);
  await writeFile(join(app, 'binary'), randomBytes(200_000));
  const cache = new NativeCache({ cacheDir: join(root, 'cache'), maxBytes: 750_000 });
  await cache.save(identity, app);
  const second = { ...identity, key: 'second' };
  const savedSecond = await cache.save(second, app);
  await utimes(savedSecond.path!, new Date(1), new Date(1));
  await writeFile(join(cache.root, 'user-file'), 'preserve');
  await cache.save({ ...identity, key: 'third' }, app);
  // Three fit within the conservative staging reservation for this budget.
  await cache.save({ ...identity, key: 'fourth' }, app);
  assert.equal((await cache.restore(second)).hit, false);
  assert.equal(await readFile(join(cache.root, 'user-file'), 'utf8'), 'preserve');
  const sizes = await Promise.all((await readdir(cache.root)).filter(p => p.endsWith('.tgz')).map(async p => (await stat(join(cache.root, p))).size));
  assert.ok(sizes.reduce((a, b) => a + b, 0) <= cache.maxBytes);
});

test('native cache prunes idle archives below its capacity', async t => {
  const { root, app } = await fixture(t);
  const cache = new NativeCache({ cacheDir: join(root, 'cache'), maxAgeMs: 1000 });
  await cache.save(identity, app);
  const archive = join(cache.root, `${artifactKey(identity)}.tgz`);
  await utimes(archive, new Date(1), new Date(1));
  await cache.save({ ...identity, key: 'fresh' }, app);
  assert.equal((await cache.restore(identity)).hit, false);
  assert.equal((await cache.restore({ ...identity, key: 'fresh' })).hit, true);
});

test('team upload and restore stream through independent local caches with digest verification', async t => {
  const { root, app } = await fixture(t);
  const objects = new Map<string, Buffer>();
  let corrupt = false;
  const server = createServer(async (request, response) => {
    assert.equal(request.headers.authorization, 'Bearer test-token');
    assert.equal(request.headers['x-layercache-compatibility'], identity.compatibility);
    const key = request.url!;
    if (request.method === 'PUT') {
      const chunks: Buffer[] = [];
      for await (const chunk of request) chunks.push(chunk);
      objects.set(key, Buffer.concat(chunks)); response.writeHead(200).end(); return;
    }
    const bytes = objects.get(key);
    if (!bytes) { response.writeHead(404).end(); return; }
    response.writeHead(200, { 'Content-Length': bytes.length, 'x-layercache-digest': `sha256:${createHash('sha256').update(bytes).digest('hex')}` });
    response.end(corrupt ? Buffer.alloc(bytes.length) : bytes);
  });
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { server.closeAllConnections(); await new Promise<void>(resolve => server.close(() => resolve())); });
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  const options = { endpoint: `http://127.0.0.1:${address.port}`, token: 'test-token' };
  await new NativeCache({ ...options, cacheDir: join(root, 'producer') }).save(identity, app);
  const consumer = new NativeCache({ ...options, cacheDir: join(root, 'consumer') });
  const restored = await consumer.restore(identity, join(root, 'restored'));
  assert.equal(restored.source, 'team');
  assert.equal(await readFile(join(restored.path!, 'Client.app/binary'), 'utf8'), 'native code');
  corrupt = true;
  await assert.rejects(new NativeCache({ ...options, cacheDir: join(root, 'corrupt') }).restore(identity), /integrity/);
  assert.deepEqual(await readdir(join(root, 'corrupt')), []);
});

test('invalid config and oversized builds are rejected', async t => {
  const { root, app } = await fixture(t);
  assert.throws(() => new NativeCache({ endpoint: 'http://example.com' }), /HTTPS/);
  assert.throws(() => new NativeCache({ maxBytes: -1 }), /positive/);
  assert.throws(() => artifactKey({ ...identity, compatibility: '' }), /compatibility/);
  await assert.rejects(new NativeCache({ cacheDir: join(root, 'small'), maxBytes: 10 }).save(identity, app), /budget/);
});

test('Expo provider reuses Debug clients across worktrees but refuses Release fingerprint reuse', async t => {
  const { root, app } = await fixture(t);
  await mkdir(join(root, 'worktree-one'));
  await mkdir(join(root, 'worktree-two'));
  if (process.platform === 'darwin') {
    // A real Mach-O makes the provider's macOS architecture validation run;
    // this transport fixture does not claim to be a launchable simulator app.
    // Apple's system tools can use arm64e rather than the runner's arm64 ABI.
    // The running Node executable necessarily supports this process architecture.
    await copyFile(process.execPath, join(app, 'SimulatorFixture'));
    await writeFile(join(app, 'Info.plist'), '<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleExecutable</key><string>SimulatorFixture</string><key>CFBundleSupportedPlatforms</key><array><string>iPhoneSimulator</string></array></dict></plist>');
    // Keep fixture failures visible instead of swallowing them through fail-open.
    await validateSimulatorApp(app, await detectSimulatorTarget(root));
  }
  const options = { project: identity.project, compatibility: identity.compatibility, app: 'mobile', cacheDir: join(root, 'cache') };
  const props = { projectRoot: join(root, 'worktree-one'), platform: 'ios' as const, fingerprintHash: 'a'.repeat(40), runOptions: {} };
  assert.ok(await provider.uploadBuildCache({ ...props, buildPath: app }, options));
  const restored = await provider.resolveBuildCache({ ...props, projectRoot: join(root, 'worktree-two') }, options);
  assert.ok(restored?.endsWith('.app'));
  assert.equal(await readFile(join(restored!, 'binary'), 'utf8'), 'native code');
  assert.throws(() => identityForBuild({ ...props, runOptions: { configuration: 'Release' } }, options), /development/);
});

test('Expo provider resolves the SDK fingerprint package in a pnpm-style dependency layout', async t => {
  const { root } = await fixture(t);
  const projectRoot = join(root, 'sdk-app');
  const expo = join(projectRoot, 'node_modules/expo');
  const cli = join(expo, 'node_modules/@expo/cli');
  const fingerprint = join(cli, 'node_modules/@expo/fingerprint');
  await mkdir(fingerprint, { recursive: true });
  await writeFile(join(projectRoot, 'package.json'), '{}');
  await writeFile(join(expo, 'package.json'), '{"name":"expo"}');
  await writeFile(join(cli, 'package.json'), '{"name":"@expo/cli"}');
  await writeFile(join(fingerprint, 'package.json'), '{"name":"@expo/fingerprint","main":"index.cjs"}');
  await writeFile(join(fingerprint, 'index.cjs'), `exports.createFingerprintAsync = async () => ({hash: '${'b'.repeat(40)}'});`);
  assert.equal(await provider.calculateFingerprintHash({ projectRoot }), 'b'.repeat(40));
});
