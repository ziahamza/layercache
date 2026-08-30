import type { DownloadOptions, UploadOptions } from "@actions/cache";

const statePrimaryKey = "cache-primary-key";
const stateMatchedKey = "cache-matched-key";
const statePaths = "cache-paths";

export interface ActionCore {
  getInput(name: string, options?: { required?: boolean }): string;
  getMultilineInput(name: string, options?: { required?: boolean }): string[];
  getBooleanInput(name: string, options?: { required?: boolean }): boolean;
  setOutput(name: string, value: unknown): void;
  saveState(name: string, value: unknown): void;
  getState(name: string): string;
  setFailed(message: string | Error): void;
  setSecret(secret: string): void;
  info(message: string): void;
  warning(message: string | Error): void;
}

export interface CacheClient {
  restoreCache(
    paths: string[],
    primaryKey: string,
    restoreKeys?: string[],
    options?: DownloadOptions,
  ): Promise<string | undefined>;
  saveCache(paths: string[], key: string, options?: UploadOptions): Promise<number>;
}

export interface ActionDependencies {
  core: ActionCore;
  cache: CacheClient;
  env: NodeJS.ProcessEnv;
}

export async function runRestore({ core, cache, env }: ActionDependencies): Promise<void> {
  try {
    const endpoint = core.getInput("endpoint", { required: true });
    const token = core.getInput("token", { required: true });
    const publicCacheMode = core.getInput("public-cache-mode") || "disabled";
    const compatibility = core.getInput("compatibility");
    configureV1Process(endpoint, token, publicCacheMode, env, compatibility);
    core.setSecret(token);

    const paths = core.getMultilineInput("path", { required: true });
    const primaryKey = core.getInput("key", { required: true });
    const restoreKeys = core.getMultilineInput("restore-keys");
    const lookupOnly = core.getBooleanInput("lookup-only");
    const failOnCacheMiss = core.getBooleanInput("fail-on-cache-miss");

    core.setOutput("cache-primary-key", primaryKey);
    core.saveState(statePrimaryKey, primaryKey);
    core.saveState(statePaths, JSON.stringify(paths));

    const matchedKey = await cache.restoreCache(paths, primaryKey, restoreKeys, { lookupOnly });
    if (matchedKey === undefined) {
      core.setOutput("cache-matched-key", "");
      core.setOutput("cache-hit", "");
      if (failOnCacheMiss) {
        core.setFailed("Layer Cache did not find a matching cache entry and fail-on-cache-miss is enabled.");
      } else {
        core.info("Layer Cache did not find a matching cache entry.");
      }
      return;
    }

    core.saveState(stateMatchedKey, matchedKey);
    core.setOutput("cache-matched-key", matchedKey);
    core.setOutput("cache-hit", matchedKey === primaryKey ? "true" : "false");
    core.info(`Layer Cache restored ${matchedKey}.`);
  } catch (error) {
    core.setFailed(errorMessage(error));
  }
}

export async function runSave({ core, cache, env }: ActionDependencies): Promise<void> {
  try {
    const endpoint = core.getInput("endpoint", { required: true });
    const token = core.getInput("token", { required: true });
    const publicCacheMode = core.getInput("public-cache-mode") || "disabled";
    const compatibility = core.getInput("compatibility");
    configureV1Process(endpoint, token, publicCacheMode, env, compatibility);
    core.setSecret(token);

    if (core.getBooleanInput("lookup-only")) {
      core.info("Layer Cache lookup-only mode skips post-job publication.");
      return;
    }

    const primaryKey = core.getState(statePrimaryKey);
    if (primaryKey === "") {
      core.info("Layer Cache has no primary key state to publish.");
      return;
    }
    if (core.getState(stateMatchedKey) === primaryKey) {
      core.info(`Layer Cache already has an exact entry for ${primaryKey}; skipping publication.`);
      return;
    }

    const paths = parseSavedPaths(core.getState(statePaths));
    const cacheId = await cache.saveCache(paths, primaryKey);
    if (cacheId < 0) {
      core.warning(`Layer Cache did not publish ${primaryKey}; see the cache client diagnostics.`);
      return;
    }
    core.info(`Layer Cache saved ${primaryKey}.`);
  } catch (error) {
    core.warning(`Layer Cache could not save the entry: ${errorMessage(error)}`);
  }
}

export function configureV1Process(
  endpoint: string,
  token: string,
  publicCacheMode: string,
  env: NodeJS.ProcessEnv,
  compatibilityOverride = "",
): void {
  if (publicCacheMode !== "disabled") {
    throw new Error(
      "Public Cache restore is disabled in this action. A verified-artifact mode must verify signed provenance and the complete digest before extraction.",
    );
  }
  if (token.trim() === "") {
    throw new Error("Layer Cache token is required.");
  }

  let url: URL;
  try {
    url = new URL(endpoint);
  } catch {
    throw new Error("Layer Cache endpoint must be an absolute HTTP or HTTPS URL.");
  }
  const loopback = url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]";
  if (url.protocol !== "https:" && !(url.protocol === "http:" && loopback)) {
    throw new Error("Layer Cache endpoint must use HTTPS, except for a loopback endpoint.");
  }
  if (url.username !== "" || url.password !== "" || url.search !== "" || url.hash !== "") {
    throw new Error("Layer Cache endpoint cannot contain credentials, a query, or a fragment.");
  }
  if (!url.pathname.endsWith("/")) {
    url.pathname += "/";
  }
  const compatibility =
    compatibilityOverride === ""
      ? compatibilityForPlatform(process.platform, process.arch)
      : compatibilityOverride;
  validateCompatibility(compatibility);
  url.pathname += `_layercache/compatibility/${encodeURIComponent(compatibility)}/`;

  env.ACTIONS_CACHE_URL = url.toString();
  env.ACTIONS_RUNTIME_TOKEN = token;
  delete env.ACTIONS_CACHE_SERVICE_V2;
}

export function compatibilityForPlatform(platform: string, architecture: string): string {
  const operatingSystem = platform === "linux" ? "linux" : platform === "darwin" ? "darwin" : "";
  const cpuArchitecture =
    architecture === "x64" || architecture === "amd64"
      ? "amd64"
      : architecture === "arm64"
        ? "arm64"
        : "";
  if (operatingSystem === "" || cpuArchitecture === "") {
    throw new Error(`Layer Cache has an unsupported Node job platform ${platform}/${architecture}.`);
  }
  return `${operatingSystem}-${cpuArchitecture}-schema1`;
}

function validateCompatibility(identity: string): void {
  if (!/^[a-z0-9][a-z0-9_.:+@-]{0,255}$/.test(identity)) {
    throw new Error(
      "Layer Cache compatibility must be at most 256 bytes and use lowercase ASCII letters, digits, and -_.:+@ delimiters.",
    );
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function parseSavedPaths(state: string): string[] {
  if (state === "") {
    throw new Error("cache paths were not saved by the restore action");
  }
  const parsed: unknown = JSON.parse(state);
  if (!Array.isArray(parsed) || parsed.length === 0 || parsed.some((path) => typeof path !== "string" || path === "")) {
    throw new Error("saved cache paths are invalid");
  }
  return parsed;
}
