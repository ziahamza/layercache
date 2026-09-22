import { NativeCache } from '../../native/client.ts';
import { exchangeTurbo, fileCommand } from '../setup/auth.ts';
import { sourceKey } from '../../native/source.ts';
import { resolve } from 'node:path';
import { planExpoSimulator, parseSimulatorTarget, validateSimulatorApp } from '../../native/expo-identity.ts';

async function main(): Promise<void> {
  const input = (name: string) => (process.env[`INPUT_${name.toUpperCase()}`] ?? '').trim();
  const project = input('project') || `github.com/${process.env.GITHUB_REPOSITORY ?? ''}`.toLowerCase();
  const operation = input('operation') || 'restore';
  const output = (name: string, value: string) => fileCommand(process.env.GITHUB_OUTPUT, name, value);
  // Fail open only to a real build, never to a false cache hit.
  output('cache-hit', 'false');
  output('source', 'degraded');
  if (!['restore', 'save'].includes(operation)) throw new Error('Invalid operation');
  const mode = input('key-mode') || 'source';
  if (!['source', 'expo'].includes(mode)) throw new Error('Invalid key mode');
  let compatibility = input('compatibility');
  let key = input('key');
  if (mode === 'expo') {
    if (operation === 'save' && process.platform !== 'darwin') throw new Error('Simulator save requires macOS');
    const planned = await planExpoSimulator({ projectRoot: resolve(process.env.GITHUB_WORKSPACE ?? process.cwd(), input('expo-root') || '.'),
      project, app: input('expo-app'), scheme: input('expo-scheme') || undefined, target: parseSimulatorTarget(input('expo-target')),
      expectedKey: key, expectedCompatibility: compatibility });
    key = planned.key;
    compatibility = planned.compatibility;
    if (operation === 'save') await validateSimulatorApp(input('path'), parseSimulatorTarget(input('expo-target')));
  } else key ||= await sourceKey(process.env.GITHUB_WORKSPACE ?? process.cwd());
  output('key', key);
  output('compatibility', compatibility);
  const credentials = await exchangeTurbo({ endpoint: input('team-url'), project, compatibility, minutes: 60, env: process.env });
  const cache = new NativeCache({ endpoint: input('team-url'), token: credentials.teamToken, maxBytes: Number(input('max-bytes') || 5 * 1024 ** 3) });
  const result = operation === 'save'
    ? await cache.save({ project, compatibility, key }, input('path') || (() => { throw new Error('Build path required'); })())
    : await cache.restore({ project, compatibility, key }, input('path') || undefined);
  output('cache-hit', String(result.hit));
  output('source', result.source);
  output('digest', result.digest ?? '');
  output('elapsed-ms', String(Math.round(result.elapsedMs)));
  process.stdout.write(`Layer Cache native ${operation}: ${result.source}, ${result.bytes} bytes, ${Math.round(result.elapsedMs)}ms.\n`);
}
main().catch((error: unknown) => {
  // Report only fixed protocol diagnostics, never arbitrary errors containing
  // credentials, URLs, filesystem paths, or archive-controlled entry names.
  const message = error instanceof Error ? error.message : '';
  if (/^(?:GitHub OIDC returned HTTP \d{3}|Turbo OIDC exchange returned HTTP \d{3}|Team Cache returned HTTP \d{3}|Invalid artifact digest or size|Artifact integrity check failed|Artifact exceeds declared size)$/.test(message)) {
    process.stdout.write(`::warning::Layer Cache native: ${message}.\n`);
  }
  process.stdout.write('::warning::Layer Cache native operation unavailable. Run the normal build; check OIDC permissions, key, compatibility, and disk budget.\n');
});
