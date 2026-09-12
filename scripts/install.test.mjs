import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync, execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, existsSync, symlinkSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { createHash } from 'node:crypto';

test('installer verifies immutable version, attestations, checksum, and package before replacing a binary', () => {
  const root = mkdtempSync(join(tmpdir(), 'layercache-installer-qa-'));
  try {
    const bin = join(root, 'mock-bin');
    const release = join(root, 'release');
    const target = join(root, 'target');
    for (const directory of [bin, release, target]) mkdirSync(directory);
    const platform = `${process.platform}-${process.arch === 'x64' ? 'amd64' : process.arch}`;
    const artifact = `layercache-${platform}`;
    const archive = `${artifact}.tar.gz`;
    writeFileSync(join(release, artifact), '#!/bin/sh\ntest "$1" = help\n', { mode: 0o755 });
    execFileSync('tar', ['-czf', join(release, archive), '-C', release, artifact]);
    const digest = createHash('sha256').update(readFileSync(join(release, archive))).digest('hex');
    writeFileSync(join(release, `${archive}.sha256`), `${digest}  ${archive}\n`);
    const log = join(root, 'calls');
    writeFileSync(join(bin, 'gh'), `#!/usr/bin/env node
const fs = require('node:fs');
const path = require('node:path');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.TEST_CALLS, JSON.stringify(args)+'\\n');
if (args[0] === 'api') {
  console.log(JSON.stringify(args[1].includes('releases/tags') ? {tag_name:'v0.1.0',draft:false,immutable:process.env.TEST_MODE !== 'mutable'} : {object:{type:'commit',sha:'a'.repeat(40)}}));
} else if (args[0] === 'release' && args[1] === 'download') {
  const destination = args[args.indexOf('--dir')+1];
  for(let i=0;i<args.length;i++) if(args[i] === '--pattern') fs.copyFileSync(path.join(process.env.TEST_RELEASE,args[i+1]),path.join(destination,args[i+1]));
} else if (args[0] === 'attestation' && args[1] === 'verify') {
  if (process.env.TEST_MODE === 'unattested') process.exit(1);
} else process.exit(1);
`, { mode: 0o755 });
    const run = (mode, version = 'v0.1.0') => spawnSync('bash', [resolve('scripts/install.sh'), '--repository', 'acme/layercache', '--version', version, '--prefix', target], {
      encoding: 'utf8', env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, TEST_RELEASE: release, TEST_CALLS: log, TEST_MODE: mode },
    });
    assert.equal(run('normal').status, 0);
    assert.equal(existsSync(join(target, 'layercache')), true);
    const calls = readFileSync(log, 'utf8').trim().split('\n').map(JSON.parse);
    const verification = calls.filter(args => args[0] === 'attestation');
    assert.equal(verification.length, 2);
    for (const args of verification) {
      assert.ok(args.includes('--deny-self-hosted-runners'));
      assert.equal(args[args.indexOf('--source-ref') + 1], 'refs/tags/v0.1.0');
      assert.equal(args[args.indexOf('--source-digest') + 1], 'a'.repeat(40));
    }
    const original = readFileSync(join(target, 'layercache'), 'utf8');
    for (const mode of ['mutable', 'unattested']) {
      assert.notEqual(run(mode).status, 0);
      assert.equal(readFileSync(join(target, 'layercache'), 'utf8'), original);
    }
    assert.notEqual(run('normal', 'latest').status, 0);
    writeFileSync(join(release, `${archive}.sha256`), `${'0'.repeat(64)}  ${archive}\n`);
    assert.notEqual(run('normal').status, 0);
    writeFileSync(join(release, `${archive}.sha256`), `${digest}  ${archive}\n`);
    rmSync(join(target, 'layercache'));
    symlinkSync(join(release, artifact), join(target, 'layercache'));
    assert.notEqual(run('normal').status, 0);
  } finally { rmSync(root, { recursive: true }); }
});
