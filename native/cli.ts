import { NativeCache, environmentOptions } from './client.ts';
import type { Identity, Options } from './client.ts';
import { sourceKey } from './source.ts';
import { resolve } from 'node:path';
import { detectSimulatorTarget, parseSimulatorTarget, planExpoSimulator } from './expo-identity.ts';

// Deliberately uses the same key and transport as CI. No checkout path or runner
// identity is included, so compatible worktrees can share outputs.
async function main(): Promise<void> {
  const [operation, ...args] = process.argv.slice(2);
  if (operation === 'key' && args.length === 0) { console.log(await sourceKey(process.cwd())); return; }
  if (operation === 'expo-target' && args.length === 0) { console.log(JSON.stringify(await detectSimulatorTarget(process.cwd()))); return; }
  const values: Record<string, string> = {};
  for (let index = 0; index < args.length; index += 2) {
    const flag = args[index];
    const value = args[index + 1];
    const flags = operation === 'expo-key' ? ['--project', '--app', '--root', '--scheme', '--target'] : ['--project', '--compatibility', '--key', '--path', '--cache-dir', '--max-bytes'];
    if (!flag || !flags.includes(flag) || !value || flag in values) throw new Error('Invalid arguments');
    values[flag] = value;
  }
  if (operation === 'expo-key') {
    const projectRoot = resolve(values['--root'] ?? '.');
    const target = values['--target'] ? parseSimulatorTarget(values['--target']) : await detectSimulatorTarget(projectRoot);
    console.log(JSON.stringify(await planExpoSimulator({ projectRoot, project: values['--project'] ?? '', app: values['--app'] ?? '', scheme: values['--scheme'], target })));
    return;
  }
  const identity: Identity = { project: values['--project'] ?? '', compatibility: values['--compatibility'] ?? '', key: values['--key'] ?? '' };
  const options: Options = { endpoint: process.env.LAYER_CACHE_URL, token: process.env.LAYER_CACHE_TOKEN,
    cacheDir: values['--cache-dir'], maxBytes: values['--max-bytes'] ? Number(values['--max-bytes']) : undefined };
  const cache = new NativeCache(environmentOptions(options));
  if (operation === 'restore') console.log(JSON.stringify(await cache.restore(identity, values['--path'])));
  else if (operation === 'save' && values['--path']) console.log(JSON.stringify(await cache.save(identity, values['--path'])));
  else throw new Error('Use restore or save --project ID --compatibility TOOLCHAIN --key KEY --path OUTPUT');
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
