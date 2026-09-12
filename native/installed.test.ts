import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { setup } from '../action/setup/main.mjs';
import { cleanup } from '../action/setup/post.mjs';
import { NativeCache } from './client.ts';

test('installed Go server accepts native archives and serves a fresh-worktree restore', { skip: !process.env.LAYERCACHE_SETUP_BINARY }, async () => {
  const root = await mkdtemp(join(tmpdir(), 'layercache-native-installed-'));
  const workspace = join(root, 'workspace');
  await mkdir(workspace);
  execFileSync('git', ['init', '--quiet', '-b', 'main', workspace]);
  execFileSync('git', ['-C', workspace, 'remote', 'add', 'origin', 'https://github.com/acme/widget.git']);
  execFileSync('git', ['-C', workspace, '-c', 'user.name=QA', '-c', 'user.email=qa@example.invalid', 'commit', '--quiet', '--allow-empty', '-m', 'fixture']);
  await writeFile(join(root, 'event.json'), JSON.stringify({ repository: { default_branch: 'main' } }));
  let installation: Awaited<ReturnType<typeof setup>> | undefined;
  try {
    installation = await setup({ ...process.env, INPUT_BINARY: resolve(process.env.LAYERCACHE_SETUP_BINARY!), 'INPUT_MAX-SIZE': '10485760',
      INPUT_COMPATIBILITY: 'native-qa-v1', RUNNER_TEMP: root, GITHUB_WORKSPACE: workspace, GITHUB_REPOSITORY: 'acme/widget', GITHUB_REF: 'refs/heads/main',
      GITHUB_SHA: execFileSync('git', ['-C', workspace, 'rev-parse', 'HEAD'], { encoding: 'utf8' }).trim(),
      GITHUB_EVENT_PATH: join(root, 'event.json'), GITHUB_STATE: join(root, 'state'), GITHUB_ENV: join(root, 'env'), GITHUB_PATH: join(root, 'path'), GITHUB_OUTPUT: join(root, 'output'),
    }, { log: () => {} });
    const options = { endpoint: installation.endpoint, token: installation.route.environment.TURBO_TOKEN };
    const identity = { project: 'github.com/acme/widget', compatibility: 'native-qa-v1', key: 'native-shell-qa' };
    const app = join(root, 'Native.app');
    await mkdir(app);
    await writeFile(join(app, 'binary'), 'actual-server-roundtrip');
    await new NativeCache({ ...options, cacheDir: join(root, 'producer-cache') }).save(identity, app);
    const result = await new NativeCache({ ...options, cacheDir: join(root, 'fresh-cache') }).restore(identity, join(root, 'fresh-worktree'));
    assert.equal(result.source, 'team');
    assert.equal(await readFile(join(result.path!, 'Native.app/binary'), 'utf8'), 'actual-server-roundtrip');
    assert.equal((await new NativeCache({ ...options, cacheDir: join(root, 'other-abi-cache') }).restore({ ...identity, compatibility: 'wrong-abi' })).hit, false);
  } finally {
    if (installation) cleanup({ RUNNER_TEMP: root, STATE_root: installation.root, STATE_binary: installation.binary, STATE_config: installation.config });
    await rm(root, { recursive: true, force: true });
  }
});
