import fs from 'node:fs';
import {join} from 'node:path';
import type {Revision} from './registry-policy.ts';

const digestPattern = /^sha256:[a-f0-9]{64}$/;
function readLink(path: string): {digest: string, stat: fs.Stats} | undefined {
  // Distribution removes links but can leave their parent directories behind.
  // Only a missing link is a tombstone; errors in live references stay fatal.
  let stat: fs.Stats;
  try {stat = fs.lstatSync(path);}
  catch (error) {if (error instanceof Error && 'code' in error && error.code === 'ENOENT') return; throw error;}
  if (!stat.isFile() || stat.size > 128) throw new Error(`Invalid registry link: ${path}`);
  const digest = fs.readFileSync(path, 'utf8').trim();
  if (!digestPattern.test(digest)) throw new Error(`Invalid registry digest: ${path}`);
  return {digest, stat};
}
function directories(path: string): string[] {
  try {return fs.readdirSync(path, {withFileTypes: true}).filter(e => e.isDirectory()).map(e => e.name);}
  catch (error) {if (error instanceof Error && 'code' in error && error.code === 'ENOENT') return []; throw error;}
}

// Caller must hold the registry maintenance lock throughout scanning and pruning.
export function scanRegistry(root: string): Revision[] {
  const revisions: Revision[] = [];
  function walk(path: string, repository: string) {
    if (fs.existsSync(join(path, '_manifests'))) {
      if (!/^(gitenv|agent-access|parle|booker)\/[a-zA-Z0-9_./-]+$/.test(repository)) return;
      const tags = new Map<string, {names: string[], createdMs: number}>();
      for (const tag of directories(join(path, '_manifests/tags'))) {
        const link = join(path, '_manifests/tags', tag, 'current/link');
        const current = readLink(link);
        if (!current) continue;
        const {digest, stat} = current;
        const prior = tags.get(digest) ?? {names: [], createdMs: 0};
        prior.names.push(tag); prior.createdMs = Math.max(prior.createdMs, stat.mtimeMs); tags.set(digest, prior);
      }
      const live = new Set<string>();
      for (const hex of directories(join(path, '_manifests/revisions/sha256'))) {
        const digest = `sha256:${hex}`;
        if (!digestPattern.test(digest)) throw new Error('Invalid revision digest');
        const link = join(path, '_manifests/revisions/sha256', hex, 'link');
        const revision = readLink(link);
        if (!revision) continue;
        if (revision.digest !== digest) throw new Error(`Revision link digest mismatch: ${link}`);
        const {stat} = revision;
        const data = join(root, 'blobs/sha256', hex.slice(0, 2), hex, 'data');
        if (fs.statSync(data).size > 16 * 1024 ** 2) throw new Error('Manifest too large for safe maintenance');
        const manifest = JSON.parse(fs.readFileSync(data, 'utf8'));
        const children = (manifest.manifests ?? []).map((entry: {digest: string}) => entry.digest);
        if (manifest.subject?.digest) children.push(manifest.subject.digest);
        if (children.some((d: string) => !digestPattern.test(d))) throw new Error('Invalid child digest');
        const tag = tags.get(digest);
        revisions.push({repository, digest, tags: tag?.names ?? [], createdMs: Math.max(stat.birthtimeMs, tag?.createdMs ?? 0), usedMs: stat.mtimeMs, children});
        live.add(digest);
      }
      for (const digest of tags.keys()) if (!live.has(digest)) throw new Error(`Live tag has no revision: ${repository}@${digest}`);
      return;
    }
    for (const name of directories(path)) if (!name.startsWith('_')) walk(join(path, name), repository ? `${repository}/${name}` : name);
  }
  walk(join(root, 'repositories'), '');
  return revisions;
}
