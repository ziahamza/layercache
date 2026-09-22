import { createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { readdir } from 'node:fs/promises';
import type { Identity } from './client.ts';

export interface BuildProps {
  projectRoot: string;
  platform: 'ios' | 'android';
  fingerprintHash: string;
  runOptions: { configuration?: string; variant?: string; scheme?: string; device?: unknown; [key: string]: unknown };
}
export interface ExpoIdentityOptions { project: string; app: string; compatibility?: string }
export interface SimulatorTarget { os: 'darwin'; arch: 'arm64' | 'x64'; xcode: string; sdk: string }

export function identityForBuild(props: BuildProps, options: ExpoIdentityOptions): Identity {
  if (!options.app) throw new Error('An app identity is required');
  if (!options.compatibility) throw new Error('Resolve the toolchain before creating a native build key');
  const configuration = props.platform === 'ios' ? props.runOptions.configuration ?? 'Debug' : props.runOptions.variant ?? 'debug';
  if (configuration !== (props.platform === 'ios' ? 'Debug' : 'debug')) throw new Error('Only development clients support native-fingerprint reuse');
  if (!/^[a-f0-9]{16,128}$/.test(props.fingerprintHash)) throw new Error('Invalid Expo fingerprint');
  return { project: options.project, compatibility: options.compatibility,
    key: JSON.stringify(['expo-dev-v1', options.app, props.platform, configuration, props.runOptions.scheme ?? '', props.fingerprintHash]) };
}

export async function fingerprintForProject(projectRoot: string): Promise<string> {
  const project = createRequire(join(projectRoot, 'package.json'));
  let fingerprint;
  try { fingerprint = project('@expo/fingerprint'); }
  catch {
    const expo = createRequire(project.resolve('expo/package.json'));
    const cli = createRequire(expo.resolve('@expo/cli/package.json'));
    fingerprint = cli('@expo/fingerprint');
  }
  // Keep exactly the provider's options: never substitute a globally installed
  // fingerprint package, download one, or silently select platform-only inputs.
  const result = await fingerprint.createFingerprintAsync(projectRoot);
  if (typeof result.hash !== 'string' || !/^[a-f0-9]{16,128}$/.test(result.hash)) throw new Error('Invalid Expo fingerprint');
  return result.hash;
}

function prepareExpoEnvironment(projectRoot: string): void {
  process.env.NODE_ENV ||= 'development';
  process.env.BABEL_ENV ||= process.env.NODE_ENV;
  Object.assign(globalThis, { __DEV__: process.env.NODE_ENV !== 'production' });
  const project = createRequire(join(projectRoot, 'package.json'));
  const expo = createRequire(project.resolve('expo/package.json'));
  const cli = createRequire(expo.resolve('@expo/cli/package.json'));
  const env = cli('@expo/env');
  // Expo 57 uses loadProjectEnv; older supported SDKs expose load. Resolve the
  // CLI's installed dependency, not a separately versioned environment parser.
  if (typeof env.loadProjectEnv === 'function') env.loadProjectEnv(projectRoot, { silent: true, mode: process.env.NODE_ENV, systemEnv: process.env });
  else if (typeof env.load === 'function') env.load(projectRoot, { silent: true });
  else throw new Error('Unsupported installed Expo environment loader');
}

export function parseSimulatorTarget(value: string): SimulatorTarget {
  const target = JSON.parse(value) as Partial<SimulatorTarget> | null;
  if (!target || target.os !== 'darwin' || !['arm64', 'x64'].includes(target.arch ?? '') ||
      typeof target.xcode !== 'string' || !/^Xcode [^\r\n]+\nBuild version [^\r\n]+$/.test(target.xcode) ||
      typeof target.sdk !== 'string' || !/^[a-zA-Z0-9.]+$/.test(target.sdk) ||
      Object.keys(target).sort().join(',') !== 'arch,os,sdk,xcode') throw new Error('Invalid simulator target');
  return target as SimulatorTarget;
}

export function simulatorCompatibility(target: SimulatorTarget): string {
  parseSimulatorTarget(JSON.stringify(target));
  const digest = createHash('sha256').update(JSON.stringify([target.os, target.arch, target.xcode, target.sdk])).digest('hex');
  return `expo-ios-toolchain-v1-${digest}`;
}

export async function detectSimulatorTarget(projectRoot: string): Promise<SimulatorTarget> {
  if (process.platform !== 'darwin') throw new Error('Simulator toolchain requires macOS');
  const execute = async (command: string, args: string[]) => promisify(execFile)(command, args, { cwd: projectRoot, timeout: 15_000, maxBuffer: 1024 ** 2 });
  const xcode = await execute('xcodebuild', ['-version']);
  const sdk = await execute('xcrun', ['--sdk', 'iphonesimulator', '--show-sdk-build-version']);
  return parseSimulatorTarget(JSON.stringify({ os: process.platform, arch: process.arch, xcode: xcode.stdout.trim(), sdk: sdk.stdout.trim() }));
}

export function verifySimulatorTarget(expected: SimulatorTarget, actual: SimulatorTarget): void {
  if (simulatorCompatibility(expected) !== simulatorCompatibility(actual)) throw new Error('Simulator toolchain differs from declared target');
}

export async function planExpoSimulator(options: {
  projectRoot: string; project: string; app: string; scheme?: string; target: SimulatorTarget;
  expectedKey?: string; expectedCompatibility?: string;
}): Promise<Identity> {
  if (!options.project) throw new Error('A project identity is required');
  if (process.platform === 'darwin') verifySimulatorTarget(options.target, await detectSimulatorTarget(options.projectRoot));
  prepareExpoEnvironment(options.projectRoot);
  const identity = identityForBuild({ projectRoot: options.projectRoot, platform: 'ios',
    fingerprintHash: await fingerprintForProject(options.projectRoot), runOptions: { configuration: 'Debug', scheme: options.scheme } },
  { ...options, compatibility: simulatorCompatibility(options.target) });
  if ((options.expectedKey && options.expectedKey !== identity.key) ||
      (options.expectedCompatibility && options.expectedCompatibility !== identity.compatibility)) throw new Error('Expo identity differs from declared identity');
  return identity;
}

export function assertSimulatorArtifact(metadata: { platforms: unknown; architectures: string[]; embeddedJavaScript: boolean }, target: SimulatorTarget): void {
  if (!Array.isArray(metadata.platforms) || metadata.platforms.length !== 1 || metadata.platforms[0] !== 'iPhoneSimulator') throw new Error('Only iOS simulator apps can be cached with Expo identity');
  const arch = target.arch === 'x64' ? 'x86_64' : 'arm64';
  if (!metadata.architectures.includes(arch)) throw new Error('Simulator app architecture differs from declared target');
  if (metadata.embeddedJavaScript) throw new Error('Embedded JavaScript cannot use development fingerprint reuse');
}

export async function validateSimulatorApp(path: string, target: SimulatorTarget): Promise<void> {
  if (process.platform !== 'darwin' || !path.endsWith('.app')) throw new Error('Simulator app validation requires macOS and an app directory');
  const execute = async (command: string, args: string[]) => promisify(execFile)(command, args, { timeout: 15_000, maxBuffer: 1024 ** 2 });
  const { stdout } = await execute('/usr/bin/plutil', ['-convert', 'json', '-o', '-', join(path, 'Info.plist')]);
  const metadata = JSON.parse(stdout) as { CFBundleSupportedPlatforms?: unknown; CFBundleExecutable?: unknown };
  if (typeof metadata.CFBundleExecutable !== 'string' || !/^[^/\\.][^/\\]*$/.test(metadata.CFBundleExecutable)) throw new Error('Invalid simulator executable');
  const architectures = await execute('/usr/bin/lipo', ['-archs', join(path, metadata.CFBundleExecutable)]);
  const files = await readdir(path, { recursive: true });
  assertSimulatorArtifact({ platforms: metadata.CFBundleSupportedPlatforms, architectures: architectures.stdout.trim().split(/\s+/),
    embeddedJavaScript: files.some(file => /(?:^|\/)main\.jsbundle$/.test(file)) }, target);
}
