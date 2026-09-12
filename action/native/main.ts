import { NativeCache } from '../../native/client.ts';
import { exchangeTurbo, fileCommand } from '../setup/auth.ts';
import { sourceKey } from '../../native/source.ts';

async function main(): Promise<void> {
  const input = (name: string) => (process.env[`INPUT_${name.toUpperCase()}`] ?? '').trim();
  const project = input('project') || `github.com/${process.env.GITHUB_REPOSITORY ?? ''}`.toLowerCase();
  const compatibility = input('compatibility');
  const key = input('key') || await sourceKey(process.env.GITHUB_WORKSPACE ?? process.cwd());
  const operation = input('operation') || 'restore';
  const output = (name: string, value: string) => fileCommand(process.env.GITHUB_OUTPUT, name, value);
  // Fail open only to a real build, never to a false cache hit.
  output('cache-hit', 'false');
  output('source', 'degraded');
  output('key', key);
  if (!['restore', 'save'].includes(operation)) throw new Error('Invalid operation');
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
main().catch(() => {
  process.stdout.write('::warning::Layer Cache native operation unavailable. Run the normal build; check OIDC permissions, key, compatibility, and disk budget.\n');
});
