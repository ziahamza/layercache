import { pathToFileURL } from 'node:url';
import { readConfig, writeConfig } from './main.ts';

export function cleanup(env: NodeJS.ProcessEnv = process.env, log = (text: string) => { process.stdout.write(text); }): void {
  if (!env.STATE_layercache_buildkit) return;
  try {
    const state = JSON.parse(env.STATE_layercache_buildkit) as { path: string; registry: string; installed: unknown; previous?: unknown };
    const config = readConfig(state.path);
    // Do not remove a credential installed by a later unrelated action.
    if (JSON.stringify(config.auths?.[state.registry]) !== JSON.stringify(state.installed)) return;
    if (state.previous === undefined) delete config.auths![state.registry];
    else config.auths![state.registry] = state.previous;
    writeConfig(state.path, config);
  } catch { log('::warning::Layer Cache registry credential cleanup failed.\n'); }
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) cleanup();
