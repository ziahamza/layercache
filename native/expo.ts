import { readdir, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { NativeCache, environmentOptions } from './client.ts';
import type { Options } from './client.ts';
import { identityForBuild, fingerprintForProject, detectSimulatorTarget, simulatorCompatibility, validateSimulatorApp } from './expo-identity.ts';
import type { BuildProps } from './expo-identity.ts';
export { identityForBuild } from './expo-identity.ts';
export type { BuildProps } from './expo-identity.ts';

export interface ProviderOptions extends Options {
  project: string;
  compatibility?: string;
  app: string;
}
async function withToolchain(props: BuildProps, options: ProviderOptions): Promise<ProviderOptions> {
  if (options.compatibility) return options;
  const execute = async (command: string, args: string[]) => promisify(execFile)(command, args, { cwd: props.projectRoot, timeout: 15_000, maxBuffer: 1024 ** 2 });
  let parts: string[];
  if (props.platform === 'ios') {
    return { ...options, compatibility: simulatorCompatibility(await detectSimulatorTarget(props.projectRoot)) };
  } else {
    const java = await execute('java', ['-version']);
    const serial = typeof props.runOptions.device === 'string' ? ['-s', props.runOptions.device] : [];
    const abi = props.runOptions.allArch ? 'all' : (await execute('adb', [...serial, 'shell', 'getprop', 'ro.product.cpu.abilist'])).stdout.trim();
    if (!abi) throw new Error('Cannot identify Android target ABI');
    parts = [java.stdout.trim(), java.stderr.trim(), abi];
  }
  const digest = createHash('sha256').update(JSON.stringify([process.platform, process.arch, ...parts])).digest('hex');
  return { ...options, compatibility: `expo-${props.platform}-toolchain-v1-${digest}` };
}
function client(options: ProviderOptions): NativeCache {
  return new NativeCache(environmentOptions(options));
}
const warn = () => console.warn('Layer Cache native reuse unavailable; Expo will build normally. Check credentials, compatibility, cache budget, and development configuration.');
const provider = {
  async calculateFingerprintHash(props: { projectRoot: string }): Promise<string | null> {
    try {
      return await fingerprintForProject(props.projectRoot);
    } catch { warn(); return null; }
  },
  async resolveBuildCache(props: BuildProps, options: ProviderOptions): Promise<string | null> {
    if (props.runOptions.buildCache === false) return null;
    const destination = join(props.projectRoot, '.expo', 'layercache', randomUUID());
    try {
      const resolved = await withToolchain(props, options);
      const result = await client(resolved).restore(identityForBuild(props, resolved), destination);
      if (!result.hit) return null;
      const names = await readdir(destination);
      const name = names[0];
      if (names.length !== 1 || !name || !name.endsWith(props.platform === 'ios' ? '.app' : '.apk')) throw new Error('Unexpected native artifact');
      if (props.platform === 'ios' && process.platform === 'darwin') await validateSimulatorApp(join(destination, name), await detectSimulatorTarget(props.projectRoot));
      console.log(`Layer Cache: ${result.source} hit, ${Math.round(result.elapsedMs)}ms, ${result.bytes} bytes. Native compilation skipped; JS still comes from Metro.`);
      return join(destination, name);
    } catch {
      await rm(destination, { recursive: true, force: true });
      warn(); return null;
    }
  },
  async uploadBuildCache(props: BuildProps & { buildPath: string }, options: ProviderOptions): Promise<string | null> {
    if (props.runOptions.buildCache === false) return null;
    try {
      if (!props.buildPath.endsWith(props.platform === 'ios' ? '.app' : '.apk')) throw new Error('Only simulator apps and development APKs are supported');
      if (props.platform === 'ios' && process.platform === 'darwin') await validateSimulatorApp(props.buildPath, await detectSimulatorTarget(props.projectRoot));
      const resolved = await withToolchain(props, options);
      const result = await client(resolved).save(identityForBuild(props, resolved), props.buildPath);
      console.log(`Layer Cache: native build cached, ${Math.round(result.elapsedMs)}ms, ${result.bytes} bytes.`);
      return result.path ?? null;
    } catch { warn(); return null; }
  },
};
export default provider;
