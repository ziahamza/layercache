import { execFileSync } from 'node:child_process';
import { lstatSync, realpathSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { pathToFileURL } from 'node:url';

export function cleanup(env = process.env) {
  const root = env.STATE_root;
  if (!root) return;
  let info;
  try { info = lstatSync(root); } catch (error) { if (error.code === 'ENOENT') return; throw error; }
  if (!info.isDirectory() || info.isSymbolicLink() || dirname(root) !== realpathSync(env.RUNNER_TEMP) ||
      !root.split('/').at(-1).startsWith('layercache-job-') || env.STATE_config !== join(root, 'config.json')) {
    throw new Error('Refusing cleanup outside the owned job directory');
  }
  // Do not delete live runtime files if shutdown cannot be confirmed.
  execFileSync(env.STATE_binary, ['stop', '--config', env.STATE_config, '--json'], { stdio: 'pipe', timeout: 30_000 });
  rmSync(root, { recursive: true });
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { cleanup(); } catch { process.stdout.write('::warning::Layer Cache cleanup could not confirm shutdown; job data was preserved.\n'); }
}
