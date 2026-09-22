import { randomUUID, createHash } from 'node:crypto';
import { lstat, mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

// Called only under NativeCache's cross-process lock. Expo supplies no release
// callback: retain each returned path until its owning process exits. PID reuse
// is deliberately conservative (it can retain space, never delete a live app).
const owner = `${process.pid}-${randomUUID()}`;
export class Extractions {
  readonly root: string;
  constructor(root: string) { this.root = root; }
  path(key: string, digest: string): string {
    return join(this.root, `${owner}-${createHash('sha256').update(key + digest).digest('hex')}`);
  }
  async usage(): Promise<{ bytes: number; keys: Set<string> }> {
    const keys = new Set<string>();
    let info;
    try { info = await lstat(this.root); }
    catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return { bytes: 0, keys }; throw error; }
    if (!info.isDirectory() || info.isSymbolicLink()) throw new Error('Unsafe native extraction directory');
    let total = 0;
    for (const name of await readdir(this.root)) {
      const match = /^(\d+)-[a-f0-9-]{36}-[a-f0-9]{64}$/.exec(name);
      if (!match) throw new Error('Unknown native extraction entry');
      const path = join(this.root, name);
      const entry = await lstat(path);
      if (!entry.isDirectory() || entry.isSymbolicLink()) throw new Error('Unsafe native extraction entry');
      let alive = true;
      try { process.kill(Number(match[1]), 0); }
      catch (error) { if ((error as NodeJS.ErrnoException).code === 'ESRCH') alive = false; }
      if (!alive) { await rm(path, { recursive: true }); continue; }
      const marker = join(path, 'reservation');
      if (!(await lstat(marker)).isFile()) throw new Error('Unsafe native extraction reservation');
      const reserved = Number(await readFile(marker, 'utf8'));
      if (!Number.isSafeInteger(reserved) || reserved <= 0) throw new Error('Invalid native extraction reservation');
      const key = await readFile(join(path, 'key'), 'utf8');
      if (!/^native-v1-[a-f0-9]{64}$/.test(key)) throw new Error('Invalid native extraction key');
      keys.add(key);
      total += reserved;
    }
    return { bytes: total, keys };
  }
  async existing(path: string): Promise<boolean> {
    try { return (await lstat(join(path, 'artifact'))).isDirectory(); }
    catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false; throw error; }
  }
  async create(path: string, key: string, reservation: number, extract: (destination: string) => Promise<void>): Promise<string> {
    await mkdir(this.root, { recursive: true, mode: 0o700 });
    await mkdir(path, { mode: 0o700 });
    try {
      await writeFile(join(path, 'reservation'), String(reservation), { flag: 'wx', mode: 0o600 });
      await writeFile(join(path, 'key'), key, { flag: 'wx', mode: 0o600 });
      await extract(join(path, 'artifact'));
      return join(path, 'artifact');
    } catch (error) { await rm(path, { recursive: true, force: true }); throw error; }
  }
}
