import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { execFileSync } from 'node:child_process';
import provider from './expo.ts';
import { identityForBuild, simulatorCompatibility, parseSimulatorTarget, verifySimulatorTarget, planExpoSimulator, detectSimulatorTarget, assertSimulatorArtifact } from './expo-identity.ts';
import type { SimulatorTarget } from './expo-identity.ts';

const target: SimulatorTarget = { os: 'darwin', arch: 'arm64', xcode: 'Xcode 26.0\nBuild version 17A5241e', sdk: '23A5240e' };
const project = 'github.com/example/mobile';

test('Expo explicit build-cache false bypasses both restore and upload', async () => {
  const props = { projectRoot: '/not-a-project', platform: 'ios' as const, fingerprintHash: 'a'.repeat(40), runOptions: { buildCache: false } };
  const options = { project, app: 'mobile' };
  assert.equal(await provider.resolveBuildCache(props, options), null);
  assert.equal(await provider.uploadBuildCache({ ...props, buildPath: '/not-an-app' }, options), null);
});

test('simulator validation rejects physical apps, wrong architecture and embedded Release JavaScript', () => {
  const debug = { platforms: ['iPhoneSimulator'], architectures: ['arm64'], embeddedJavaScript: false };
  assert.doesNotThrow(() => assertSimulatorArtifact(debug, target));
  assert.throws(() => assertSimulatorArtifact({ ...debug, platforms: ['iPhoneOS'] }, target), /simulator/);
  assert.throws(() => assertSimulatorArtifact({ ...debug, architectures: ['x86_64'] }, target), /architecture/);
  assert.throws(() => assertSimulatorArtifact({ ...debug, embeddedJavaScript: true }, target), /Embedded/);
});

test('simulator compatibility preserves the existing provider identity and isolates every target component', () => {
  const old = createHash('sha256').update(JSON.stringify(['darwin', 'arm64', target.xcode, target.sdk])).digest('hex');
  assert.equal(simulatorCompatibility(target), `expo-ios-toolchain-v1-${old}`);
  assert.deepEqual(parseSimulatorTarget(JSON.stringify(target)), target);
  for (const changed of [{ ...target, arch: 'x64' as const }, { ...target, sdk: '23B1' }, { ...target, xcode: 'Xcode 26.1\nBuild version 17B1' }]) {
    assert.throws(() => verifySimulatorTarget(target, changed), /differs/);
  }
  for (const invalid of [null, { ...target, os: 'linux' }, { ...target, arch: 'amd64' }, { ...target, sdk: '' }, { ...target, xcode: '26' }, { ...target, extra: true }]) {
    assert.throws(() => parseSimulatorTarget(JSON.stringify(invalid)), /Invalid/);
  }
});

test('provider and CI planner share the installed fingerprint implementation, options, and development identity', async t => {
  const root = await mkdtemp(join(tmpdir(), 'layercache-expo-identity-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const actualTarget = process.platform === 'darwin' ? await detectSimulatorTarget(root) : target;
  const apps = [join(root, 'first-worktree'), join(root, 'fresh-worktree')];
  for (const app of apps) {
    const dependency = join(app, 'node_modules/@expo/fingerprint');
    await mkdir(dependency, { recursive: true });
    await writeFile(join(app, 'package.json'), '{}');
    await writeFile(join(app, 'native-input'), 'native-one');
    await writeFile(join(dependency, 'package.json'), '{"main":"index.cjs"}');
    for (const name of ['expo', '@expo/cli', '@expo/env']) {
      await mkdir(join(app, 'node_modules', name), { recursive: true });
      await writeFile(join(app, 'node_modules', name, 'package.json'), '{"main":"index.cjs"}');
    }
    await writeFile(join(app, 'node_modules/@expo/env/index.cjs'), `exports.loadProjectEnv = (root, options) => {
      if (!options.silent || options.mode !== process.env.NODE_ENV || options.systemEnv !== process.env) throw Error('Environment options drift');
      if (!process.env.NODE_ENV || !process.env.BABEL_ENV) throw Error('Missing Expo environment defaults');
    };`);
    await writeFile(join(dependency, 'index.cjs'), `exports.createFingerprintAsync = async function(root, options) {
      if (options !== undefined) throw Error('Provider options drift');
      return {hash: require('node:crypto').createHash('sha1').update(require('node:fs').readFileSync(require('node:path').join(root, 'native-input'))).digest('hex')};
    };`);
  }
  const projectRoot = apps[0]!;
  const options = { projectRoot, project, app: 'mobile', target: actualTarget };
  const planned = await planExpoSimulator(options);
  const fingerprintHash = await provider.calculateFingerprintHash({ projectRoot });
  assert.ok(fingerprintHash);
  const props = { projectRoot, platform: 'ios' as const, fingerprintHash, runOptions: {} };
  assert.deepEqual(planned, identityForBuild(props, { project, app: 'mobile', compatibility: simulatorCompatibility(actualTarget) }));
  assert.deepEqual(await planExpoSimulator({ ...options, projectRoot: apps[1]! }), planned);
  await writeFile(join(projectRoot, 'App.tsx'), 'changed javascript does not enter this fingerprint');
  assert.deepEqual(await planExpoSimulator(options), planned);
  const scheme = await planExpoSimulator({ ...options, scheme: 'Mobile' });
  assert.notEqual(scheme.key, planned.key);
  assert.deepEqual(scheme, identityForBuild({ ...props, runOptions: { scheme: 'Mobile' } }, { project, app: 'mobile', compatibility: planned.compatibility }));
  assert.throws(() => identityForBuild({ ...props, runOptions: { configuration: 'Release' } }, { project, app: 'mobile', compatibility: planned.compatibility }), /development/);
  await assert.rejects(planExpoSimulator({ ...options, expectedCompatibility: 'another-target' }), /differs/);
  await assert.rejects(planExpoSimulator({ ...options, app: '' }), /app identity/);
  await assert.rejects(planExpoSimulator({ ...options, project: '' }), /project identity/);
  await writeFile(join(projectRoot, 'native-input'), 'native-two');
  await assert.rejects(planExpoSimulator({ ...options, expectedKey: planned.key }), /differs/);
  assert.notEqual((await planExpoSimulator(options)).key, planned.key);
});

test('Expo action fail-opens with cache-hit=false before attempting unsupported simulator saves', { skip: process.platform === 'darwin' }, async t => {
  const root = await mkdtemp(join(tmpdir(), 'layercache-expo-action-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const output = join(root, 'output');
  const stdout = execFileSync(process.execPath, ['action/native/main.ts'], { encoding: 'utf8', env: {
    ...process.env, GITHUB_OUTPUT: output, INPUT_OPERATION: 'save', 'INPUT_KEY-MODE': 'expo',
  } });
  const { readFile } = await import('node:fs/promises');
  assert.match(await readFile(output, 'utf8'), /cache-hit<<[^\n]+\nfalse\n/);
  assert.match(stdout, /Run the normal build/);
});
