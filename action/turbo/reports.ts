import { lstat, readdir, realpath, open, constants } from 'node:fs/promises';
import { resolve, relative, join, isAbsolute } from 'node:path';

export interface ReportState {
  endpoint: string;
  project: string;
  compatibility: string;
  workspace: string;
  directory: string;
  existing: string[];
  startedAt: number;
}
export function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw new Error('Invalid object');
  return value as Record<string, unknown>;
}
function inside(root: string, path: string) {
  const part = relative(root, path);
  if (part === '..' || part.startsWith('../') || isAbsolute(part))
    throw new Error('Summary directory escapes workspace');
}
async function files(directory: string): Promise<string[]> {
  try {
    return (await readdir(directory)).filter((name) => name.endsWith('.json'));
  } catch (error) {
    if (error instanceof Error && 'code' in error && error.code === 'ENOENT')
      return [];
    throw error;
  }
}
export async function startReport(
  workspace: string,
  workingDirectory: string,
  identity: Pick<ReportState, 'endpoint' | 'project' | 'compatibility'>,
): Promise<ReportState> {
  const root = await realpath(workspace),
    directory = await realpath(resolve(root, workingDirectory));
  inside(root, directory);
  const existing = await files(join(directory, '.turbo/runs'));
  if (existing.length > 4096) throw new Error('Too many existing summaries');
  return {
    ...identity,
    workspace: root,
    directory,
    existing,
    startedAt: Date.now(),
  };
}

export async function completedSummaries(
  state: ReportState,
): Promise<Record<string, unknown>[]> {
  const root = await realpath(state.workspace);
  const directory = await realpath(state.directory);
  inside(root, directory);
  const runs = join(directory, '.turbo/runs');
  const names = await files(runs);
  if (!names.length) return [];
  inside(root, await realpath(runs));
  const old = new Set(state.existing),
    fresh = names.filter((name) => !old.has(name)).sort();
  if (fresh.length > 32) throw new Error('Too many new summaries');
  const summaries: Record<string, unknown>[] = [];
  let bytes = 0;
  for (const name of fresh) {
    const path = join(runs, name),
      metadata = await lstat(path);
    if (
      !metadata.isFile() ||
      metadata.isSymbolicLink() ||
      metadata.size > 2 * 1024 ** 2
    )
      throw new Error('Unsafe summary file');
    bytes += metadata.size;
    if (bytes > 8 * 1024 ** 2) throw new Error('Summary batch too large');
    const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW);
    let source: Record<string, unknown>;
    try {
      const actual = await file.stat();
      if (!actual.isFile() || actual.size > 2 * 1024 ** 2)
        throw new Error('Unsafe summary file');
      const buffer = Buffer.alloc(2 * 1024 ** 2 + 1);
      let length = 0;
      while (length < buffer.length) {
        const read = await file.read(
          buffer,
          length,
          buffer.length - length,
          null,
        );
        if (read.bytesRead === 0) break;
        length += read.bytesRead;
      }
      if (length > 2 * 1024 ** 2) throw new Error('Summary grew beyond limit');
      bytes += Math.max(0, length - metadata.size);
      if (bytes > 8 * 1024 ** 2) throw new Error('Summary batch too large');
      source = object(JSON.parse(buffer.subarray(0, length).toString('utf8')));
    } finally {
      await file.close();
    }
    const execution = object(source.execution);
    if (
      typeof execution.startTime !== 'number' ||
      execution.startTime < state.startedAt - 5000
    )
      throw new Error('Summary predates setup');
    if (!Array.isArray(source.tasks) || source.tasks.length > 4096)
      throw new Error('Invalid tasks');
    // Turbo summaries can contain environment and command details. Send only
    // the fields required for measurement, never the full original document.
    summaries.push({
      id: source.id,
      version: source.version,
      execution: { startTime: execution.startTime, endTime: execution.endTime },
      tasks: source.tasks.map((value) => {
        const task = object(value),
          cache = object(task.cache),
          execution = object(task.execution),
          definition = object(task.resolvedTaskDefinition);
        return {
          taskId: task.taskId,
          hash: task.hash,
          dependencies: task.dependencies,
          cache: {
            local: cache.local,
            remote: cache.remote,
            status: cache.status,
            source: cache.source,
            timeSaved: cache.timeSaved,
          },
          execution: {
            startTime: execution.startTime,
            endTime: execution.endTime,
            exitCode: execution.exitCode,
          },
          resolvedTaskDefinition: { cache: definition.cache },
        };
      }),
    });
  }
  return summaries;
}
