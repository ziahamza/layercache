import assert from 'node:assert/strict';
import fs from 'node:fs';
import {tmpdir} from 'node:os';
import {join, dirname} from 'node:path';
import {test, type TestContext} from 'node:test';
import {scanRegistry} from './registry-scan.ts';

const hex = 'a'.repeat(64);
const digest = `sha256:${hex}`;
function fixture(t: TestContext) {
  const root = fs.mkdtempSync(join(tmpdir(), 'registry-scan-'));
  t.after(() => fs.rmSync(root, {recursive: true, force: true}));
  const repo = join(root, 'repositories/gitenv/build');
  const revision = join(repo, '_manifests/revisions/sha256', hex, 'link');
  const tag = join(repo, '_manifests/tags/latest/current/link');
  const blob = join(root, 'blobs/sha256/aa', hex, 'data');
  const write = (path: string, contents: string) => {fs.mkdirSync(dirname(path), {recursive: true}); fs.writeFileSync(path, contents);};
  return {root, revision, tag, blob, write};
}

test('deleted revision and tag remnants do not prevent subsequent maintenance', t => {
  const f = fixture(t);
  fs.mkdirSync(dirname(f.revision), {recursive: true});
  fs.mkdirSync(dirname(f.tag), {recursive: true});
  assert.deepEqual(scanRegistry(f.root), []);
});

test('deleted revision alone reproduces the production ENOENT without blocking scans', t => {
  const f = fixture(t);
  fs.mkdirSync(dirname(f.revision), {recursive: true});
  assert.deepEqual(scanRegistry(f.root), []);
  assert.deepEqual(scanRegistry(f.root), []);
});

test('live tagged manifest is scanned and protects its tag', t => {
  const f = fixture(t);
  f.write(f.revision, digest); f.write(f.tag, digest); f.write(f.blob, '{}');
  const revisions = scanRegistry(f.root);
  assert.equal(revisions.length, 1);
  assert.deepEqual(revisions[0]?.tags, ['latest']);
});

test('live tag without a revision aborts rather than dropping a retained root', t => {
  const f = fixture(t); f.write(f.tag, digest);
  assert.throws(() => scanRegistry(f.root));
});

test('live revision with missing manifest blob aborts', t => {
  const f = fixture(t); f.write(f.revision, digest);
  assert.throws(() => scanRegistry(f.root));
});

test('corrupt revision link aborts', t => {
  const f = fixture(t); f.write(f.revision, `sha256:${'b'.repeat(64)}`); f.write(f.blob, '{}');
  assert.throws(() => scanRegistry(f.root));
});

test('invalid live tag digest aborts', t => {
  const f = fixture(t); f.write(f.tag, '../invalid');
  assert.throws(() => scanRegistry(f.root));
});

test('dangling symlink is corruption, not a deletion remnant', t => {
  const f = fixture(t);
  fs.mkdirSync(dirname(f.revision), {recursive: true});
  fs.symlinkSync(join(f.root, 'absent'), f.revision);
  assert.throws(() => scanRegistry(f.root), /Invalid registry link/);
});

test('live tag pointing at an empty revision directory aborts', t => {
  const f = fixture(t); f.write(f.tag, digest);
  fs.mkdirSync(dirname(f.revision), {recursive: true});
  assert.throws(() => scanRegistry(f.root), /Live tag has no revision/);
});

test('unrelated deletion remnants leave live manifest and child references intact', t => {
  const f = fixture(t);
  const child = `sha256:${'b'.repeat(64)}`;
  f.write(f.revision, digest); f.write(f.tag, digest);
  f.write(f.blob, JSON.stringify({manifests: [{digest: child}]}));
  fs.mkdirSync(join(dirname(dirname(f.revision)), 'c'.repeat(64)), {recursive: true});
  const revisions = scanRegistry(f.root);
  assert.equal(revisions.length, 1);
  assert.deepEqual(revisions[0]?.children, [child]);
});
