import { build } from 'esbuild';
import { createRequire } from 'node:module';
import { readFile, readdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

for (const [entry, outfile, format] of [
  ['native/expo.ts', 'native/dist/expo.cjs', 'cjs'],
  ['native/client.ts', 'native/dist/client.js', 'esm'],
  ['native/cli.ts', 'native/dist/cli.js', 'esm'],
  ['action/native/main.ts', 'action/native/dist/main.cjs', 'cjs'],
] as const) {
  await build({ entryPoints: [entry], outfile, format, bundle: true, platform: 'node', target: 'node24',
    legalComments: 'eof', banner: entry === 'native/cli.ts' ? { js: '#!/usr/bin/env node' } : undefined });
}
const require = createRequire(import.meta.url);
const dependency = createRequire(require.resolve('tar/package.json'));
const notices: string[] = [];
for (const name of ['tar', '@isaacs/fs-minipass', 'chownr', 'minipass', 'minizlib', 'yallist']) {
  const directory = dirname(dependency.resolve(`${name}/package.json`));
  const license = (await readdir(directory)).find(name => /^license/i.test(name));
  if (!license) throw new Error(`Missing bundled dependency license: ${name}`);
  notices.push(`${name}\n${await readFile(join(directory, license), 'utf8')}`);
}
for (const directory of ['native/dist', 'action/native/dist']) await writeFile(join(directory, 'THIRD_PARTY_LICENSES.txt'), notices.join('\n\n'));
