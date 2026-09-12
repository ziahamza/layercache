import { readdir, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createRequire } from 'node:module';
import { NativeCache, environmentOptions } from './client.ts';
import type { Identity, Options } from './client.ts';

export interface ProviderOptions extends Options {
  project: string;
  compatibility?: string;
  app: string;
}
export interface BuildProps {
  projectRoot: string;
  platform: 'ios' | 'android';
  fingerprintHash: string;
  runOptions: { configuration?: string; variant?: string; scheme?: string; device?: unknown; [key: string]: unknown };
}
export function identityForBuild(props: BuildProps, options: ProviderOptions): Identity {
  if (!options.app) throw new Error('An app identity is required');
  if (!options.compatibility) throw new Error('Resolve the toolchain before creating a native build key');
  // Debug clients load current JS from Metro. Release binaries embed JS and need
  // an exact source key or an explicit repack operation, neither inferred here.
  const configuration = props.platform === 'ios' ? props.runOptions.configuration ?? 'Debug' : props.runOptions.variant ?? 'debug';
  if (configuration !== (props.platform === 'ios' ? 'Debug' : 'debug')) throw new Error('Only development clients support native-fingerprint reuse');
  if (!/^[a-f0-9]{16,128}$/.test(props.fingerprintHash)) throw new Error('Invalid Expo fingerprint');
  return { project: options.project, compatibility: options.compatibility,
    key: JSON.stringify(['expo-dev-v1', options.app, props.platform, configuration, props.runOptions.scheme ?? '', props.fingerprintHash]) };
}
async function withToolchain(props: BuildProps, options: ProviderOptions): Promise<ProviderOptions> {
  if (options.compatibility) return options;
  const execute = async (command: string, args: string[]) => promisify(execFile)(command, args, { cwd: props.projectRoot, timeout: 15_000, maxBuffer: 1024 ** 2 });
  let parts: string[];
  if (props.platform === 'ios') {
    const xcode = await execute('xcodebuild', ['-version']);
    const sdk = await execute('xcrun', ['--sdk', 'iphonesimulator', '--show-sdk-build-version']);
    parts = [xcode.stdout.trim(), sdk.stdout.trim()];
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
      const project = createRequire(join(props.projectRoot, 'package.json'));
      let fingerprint;
      try { fingerprint = project('@expo/fingerprint'); }
      catch {
        // pnpm does not expose Expo's transitive fingerprint dependency to the
        // app. Use the SDK's pinned copy rather than silently disabling caching
        // or downloading a different fingerprint implementation at runtime.
        const expo = createRequire(project.resolve('expo/package.json'));
        const cli = createRequire(expo.resolve('@expo/cli/package.json'));
        fingerprint = cli('@expo/fingerprint');
      }
      const result = await fingerprint.createFingerprintAsync(props.projectRoot);
      if (typeof result.hash !== 'string' || !/^[a-f0-9]{16,128}$/.test(result.hash)) throw new Error('Invalid Expo fingerprint');
      return result.hash;
    } catch { warn(); return null; }
  },
  async resolveBuildCache(props: BuildProps, options: ProviderOptions): Promise<string | null> {
    const destination = join(props.projectRoot, '.expo', 'layercache', randomUUID());
    try {
      const resolved = await withToolchain(props, options);
      const result = await client(resolved).restore(identityForBuild(props, resolved), destination);
      if (!result.hit) return null;
      const names = await readdir(destination);
      const name = names[0];
      if (names.length !== 1 || !name || !name.endsWith(props.platform === 'ios' ? '.app' : '.apk')) throw new Error('Unexpected native artifact');
      console.log(`Layer Cache: ${result.source} hit, ${Math.round(result.elapsedMs)}ms, ${result.bytes} bytes. Native compilation skipped; JS still comes from Metro.`);
      return join(destination, name);
    } catch {
      await rm(destination, { recursive: true, force: true });
      warn(); return null;
    }
  },
  async uploadBuildCache(props: BuildProps & { buildPath: string }, options: ProviderOptions): Promise<string | null> {
    try {
      if (!props.buildPath.endsWith(props.platform === 'ios' ? '.app' : '.apk')) throw new Error('Only simulator apps and development APKs are supported');
      const resolved = await withToolchain(props, options);
      const result = await client(resolved).save(identityForBuild(props, resolved), props.buildPath);
      console.log(`Layer Cache: native build cached, ${Math.round(result.elapsedMs)}ms, ${result.bytes} bytes.`);
      return result.path ?? null;
    } catch { warn(); return null; }
  },
};
export default provider;
