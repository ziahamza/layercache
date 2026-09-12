import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { lstat, readlink } from 'node:fs/promises';
import { join } from 'node:path';
import { digestFile } from './client.ts';

// Hash names, modes, and current contents, including unstaged and untracked
// source files. Ignore checkout location and gitignored build outputs/secrets.
export async function sourceKey(directory: string): Promise<string> {
  const git = (args: string[]) => execFileSync('git', ['-C', directory, ...args], { encoding: 'utf8', maxBuffer: 32 * 1024 ** 2 });
  const root = git(['rev-parse', '--show-toplevel']).trim();
  const files = execFileSync('git', ['-C', root, 'ls-files', '--cached', '--others', '--exclude-standard', '-z'], { encoding: 'utf8', maxBuffer: 32 * 1024 ** 2 });
  const hash = createHash('sha256').update('layercache-source-v1\0');
  for (const name of [...new Set(files.split('\0').filter(Boolean))].sort()) {
    const path = join(root, name);
    let value: unknown;
    try {
      const info = await lstat(path);
      if (info.isSymbolicLink()) value = [name, 'link', await readlink(path)];
      else if (info.isFile()) value = [name, 'file', info.mode & 0o111 ? 'executable' : 'regular', await digestFile(path)];
      else throw new Error('Submodules and special source files need an explicit build key');
    } catch (error) {
      if (error instanceof Error && 'code' in error && error.code === 'ENOENT') value = [name, 'deleted'];
      else throw error;
    }
    hash.update(JSON.stringify(value)).update('\0');
  }
  return hash.digest('hex');
}
