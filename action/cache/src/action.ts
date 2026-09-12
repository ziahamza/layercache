import type { DownloadOptions, UploadOptions } from "@actions/cache";

import {
  VerifiedCacheClient,
  VerifiedCacheInputError,
  VerifiedCacheSafetyError,
  validateCacheRequest,
} from "./verified-cache.js";

const statePrimaryKey = "cache-primary-key";
const stateMatchedKey = "cache-matched-key";
const statePaths = "cache-paths";

class RuntimeTokenUnavailableError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RuntimeTokenUnavailableError";
  }
}

export interface ActionCore {
  getInput(name: string, options?: { required?: boolean }): string;
  getMultilineInput(name: string, options?: { required?: boolean }): string[];
  getBooleanInput(name: string, options?: { required?: boolean }): boolean;
  getIDToken(audience?: string): Promise<string>;
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
    const fallbackToken = core.getInput("token");
    const publicCacheMode = core.getInput("public-cache-mode") || "disabled";
    const compatibility = core.getInput("compatibility") || compatibilityForPlatform(process.platform, process.arch);
    validateV1Configuration(endpoint, publicCacheMode, compatibility);
    const recipeDigest = core.getInput("public-recipe-digest");
    const platform = core.getInput("public-platform") || platformForRuntime(process.platform, process.arch);
    const toolchain = core.getInput("public-toolchain") || "actions/cache@6.2.0";
    const builder = core.getInput("public-builder");

    const paths = core.getMultilineInput("path", { required: true });
    const primaryKey = core.getInput("key", { required: true });
    const restoreKeys = core.getMultilineInput("restore-keys");
    validateCacheRequest(paths, primaryKey, restoreKeys);
    const lookupOnly = core.getBooleanInput("lookup-only");
    const failOnCacheMiss = core.getBooleanInput("fail-on-cache-miss");

    let restoreClient = cache;
    let publicTarget = "";
    if (publicCacheMode === "verified") {
      const repository = canonicalGitHubRepository(requiredEnvironment(env, "GITHUB_REPOSITORY"));
      const commit = requiredEnvironment(env, "GITHUB_SHA");
      publicTarget = actionsWorkflowTarget(
        repository,
        requiredEnvironment(env, "GITHUB_WORKFLOW_REF"),
        requiredEnvironment(env, "GITHUB_JOB"),
      );
      restoreClient = new VerifiedCacheClient({
        env,
        trustKey: core.getInput("public-trust-key", { required: true }),
        lookupTimeoutMilliseconds: parseLookupTimeoutMilliseconds(core.getInput("lookup-timeout-seconds")),
        identity: {
          repository,
          ref: requiredEnvironment(env, "GITHUB_REF"),
          commit,
          compatibility,
          recipeDigest: requiredValue(recipeDigest, "public-recipe-digest"),
          target: publicTarget,
          platform,
          inputs: [{ name: "compatibility", value: compatibility }],
          toolchain,
          builder: requiredValue(builder, "public-builder"),
        },
      });
    }

    core.setOutput("cache-primary-key", primaryKey);
    core.saveState(statePrimaryKey, primaryKey);
    core.saveState(statePaths, JSON.stringify(paths));

    let token: string;
    try {
      token = await resolveRuntimeToken(core, endpoint, core.getInput("project"), fallbackToken, {
        compatibility,
        recipeDigest,
        target: publicTarget,
        platform,
        toolchain,
        builder,
      });
    } catch (error) {
      if (!(error instanceof RuntimeTokenUnavailableError)) {
        throw error;
      }
      core.setOutput("cache-matched-key", "");
      core.setOutput("cache-hit", "");
      core.warning(`Layer Cache authentication is unavailable; continuing as a cache miss: ${error.message}`);
      if (failOnCacheMiss) {
        core.setFailed("Layer Cache authentication was unavailable and fail-on-cache-miss is enabled.");
      }
      return;
    }
    configureV1Process(endpoint, token, publicCacheMode, env, compatibility);

    let matchedKey: string | undefined;
    let restoreUnavailable = false;
    try {
      matchedKey = await restoreClient.restoreCache(paths, primaryKey, restoreKeys, { lookupOnly });
    } catch (error) {
      if (error instanceof VerifiedCacheInputError || error instanceof VerifiedCacheSafetyError) {
        throw error;
      }
      core.warning(`Layer Cache could not restore an entry and will continue as a cache miss: ${errorMessage(error)}`);
      matchedKey = undefined;
      restoreUnavailable = true;
    }
    if (matchedKey === undefined) {
      core.setOutput("cache-matched-key", "");
      core.setOutput("cache-hit", "");
      if (restoreUnavailable) {
        if (failOnCacheMiss) {
          core.setFailed("Layer Cache restore was unavailable and fail-on-cache-miss is enabled.");
        }
        return;
      }
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

    const endpoint = core.getInput("endpoint", { required: true });
    const compatibility = core.getInput("compatibility") || compatibilityForPlatform(process.platform, process.arch);
    const publicCacheMode = core.getInput("public-cache-mode") || "disabled";
    const publicTarget = publicCacheMode === "verified"
      ? actionsWorkflowTarget(
        canonicalGitHubRepository(requiredEnvironment(env, "GITHUB_REPOSITORY")),
        requiredEnvironment(env, "GITHUB_WORKFLOW_REF"),
        requiredEnvironment(env, "GITHUB_JOB"),
      )
      : "";
    const token = await resolveRuntimeToken(core, endpoint, core.getInput("project"), core.getInput("token"), {
      compatibility,
      recipeDigest: core.getInput("public-recipe-digest"),
      target: publicTarget,
      platform: core.getInput("public-platform") || platformForRuntime(process.platform, process.arch),
      toolchain: core.getInput("public-toolchain") || "actions/cache@6.2.0",
      builder: core.getInput("public-builder"),
    });
    configureV1Process(endpoint, token, publicCacheMode, env, compatibility);

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
  validateV1Configuration(endpoint, publicCacheMode, compatibilityOverride);
  if (token.trim() === "") {
    throw new Error("Layer Cache token is required.");
  }

  const url = validatedEndpoint(endpoint);
  if (!url.pathname.endsWith("/")) {
    url.pathname += "/";
  }
  const compatibility =
    compatibilityOverride === ""
      ? compatibilityForPlatform(process.platform, process.arch)
      : compatibilityOverride;
  url.pathname += `_layercache/compatibility/${encodeURIComponent(compatibility)}/`;

  env.ACTIONS_CACHE_URL = url.toString();
  env.ACTIONS_RUNTIME_TOKEN = token;
  delete env.ACTIONS_CACHE_SERVICE_V2;
}

function validateV1Configuration(
  endpoint: string,
  publicCacheMode: string,
  compatibilityOverride = "",
): void {
  if (publicCacheMode !== "disabled" && publicCacheMode !== "verified") {
    throw new Error("Layer Cache public-cache-mode must be disabled or verified.");
  }
  validatedEndpoint(endpoint);
  const compatibility =
    compatibilityOverride === ""
      ? compatibilityForPlatform(process.platform, process.arch)
      : compatibilityOverride;
  validateCompatibility(compatibility);
}

interface OIDCExchangeIdentity {
  compatibility: string;
  recipeDigest: string;
  target: string;
  platform: string;
  toolchain: string;
  builder: string;
}

async function resolveRuntimeToken(
  core: ActionCore,
  endpoint: string,
  project: string,
  fallbackToken: string,
  identity: OIDCExchangeIdentity,
): Promise<string> {
  const endpointURL = validatedEndpoint(endpoint);
  if (fallbackToken !== "") {
    core.setSecret(fallbackToken);
  }
  if (project !== "") {
    try {
      const idToken = await core.getIDToken(`layercache:${project}`);
      const exchangeURL = new URL(endpointURL);
      const basePath = exchangeURL.pathname.replace(/\/+$/, "");
      exchangeURL.pathname = `${basePath.endsWith("/v1") ? basePath : `${basePath}/v1`}/auth/github-oidc/exchange`;
      const response = await fetch(exchangeURL, {
        method: "POST",
		redirect: "error",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          project,
          compatibility: identity.compatibility,
          idToken,
          recipeDigest: identity.recipeDigest,
          target: identity.target,
          platform: identity.platform,
          toolchain: identity.toolchain,
          builder: identity.builder,
        }),
        signal: AbortSignal.timeout(20_000),
      });
      if (!response.ok) {
        throw new Error(`OIDC exchange returned HTTP ${response.status}`);
      }
      const exchanged = await response.json() as { teamToken?: unknown; expiresAt?: unknown };
      if (typeof exchanged.teamToken !== "string" || exchanged.teamToken === "" ||
          typeof exchanged.expiresAt !== "string" || Date.parse(exchanged.expiresAt) <= Date.now() + 60_000) {
        throw new Error("OIDC exchange returned an invalid or nearly expired token");
      }
      core.setSecret(exchanged.teamToken);
      return exchanged.teamToken;
    } catch (error) {
      if (fallbackToken === "") {
        throw new RuntimeTokenUnavailableError(
          `Layer Cache could not exchange the GitHub OIDC identity: ${errorMessage(error)}`,
        );
      }
      core.warning(`Layer Cache OIDC exchange failed; using the project-scoped token fallback: ${errorMessage(error)}`);
    }
  }
  if (fallbackToken === "") {
    throw new Error("Set project for GitHub OIDC authentication or supply a project-scoped Layer Cache token.");
  }
  return fallbackToken;
}

function validatedEndpoint(endpoint: string): URL {
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
  return url;
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

export function platformForRuntime(platform: string, architecture: string): string {
  const operatingSystem = platform === "linux" ? "linux" : platform === "darwin" ? "darwin" : "";
  const cpuArchitecture =
    architecture === "x64" || architecture === "amd64"
      ? "amd64"
      : architecture === "arm64"
        ? "arm64"
        : "";
  if (operatingSystem === "" || cpuArchitecture === "") {
    throw new Error(`Layer Cache has an unsupported Public Cache platform ${platform}/${architecture}.`);
  }
  return `${operatingSystem}/${cpuArchitecture}`;
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

function requiredEnvironment(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} is required in verified Public Cache mode.`);
  }
  return value;
}

function canonicalGitHubRepository(value: string): string {
  const [owner, repository, extra] = value.split("/");
  const validOwner = owner !== undefined &&
    /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/.test(owner);
  const validRepository = repository !== undefined && repository.length <= 100 && repository !== "." &&
    repository !== ".." && /^[A-Za-z0-9._-]+$/.test(repository);
  if (!validOwner || !validRepository || extra !== undefined) {
    throw new Error("GITHUB_REPOSITORY must use the GitHub owner/repository format.");
  }
  return `${owner}/${repository}`.toLowerCase();
}

function actionsWorkflowTarget(repository: string, workflowRef: string, job: string): string {
  const prefix = `${repository}/`;
  if (workflowRef.length <= prefix.length ||
      workflowRef.slice(0, prefix.length).toLowerCase() !== prefix.toLowerCase()) {
    throw new Error("GITHUB_WORKFLOW_REF must match GITHUB_REPOSITORY.");
  }
  const workflowIdentity = workflowRef.slice(prefix.length);
  const separator = workflowIdentity.lastIndexOf("@");
  if (separator <= 0 || separator === workflowIdentity.length - 1) {
    throw new Error("GITHUB_WORKFLOW_REF must identify one workflow file and ref.");
  }
  const workflowPath = workflowIdentity.slice(0, separator);
  const reference = workflowIdentity.slice(separator + 1);
  if (!/^\.github\/workflows\/[A-Za-z0-9][A-Za-z0-9._/-]*\.ya?ml$/.test(workflowPath) ||
      workflowPath.includes("..") || workflowPath.includes("//") ||
      !/^refs\/(?:heads|tags|pull)\/[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$/.test(reference) ||
      reference.includes("..") || reference.includes("//") || reference.endsWith("/") || reference.endsWith(".") ||
      !/^[A-Za-z_][A-Za-z0-9_-]{0,99}$/.test(job)) {
    throw new Error("GITHUB_WORKFLOW_REF and GITHUB_JOB must identify one safe workflow job.");
  }
  const target = `${workflowPath}#${job}`;
  if (target.length > 512) {
    throw new Error("GITHUB_WORKFLOW_REF and GITHUB_JOB are too long.");
  }
  return target;
}

function parseLookupTimeoutMilliseconds(value: string): number {
  if (value === "") {
    return 2_000;
  }
  if (!/^[0-9]+$/.test(value)) {
    throw new Error("Layer Cache lookup-timeout-seconds must be an integer from 1 to 3600.");
  }
  const seconds = Number(value);
  if (!Number.isSafeInteger(seconds) || seconds < 1 || seconds > 3600) {
    throw new Error("Layer Cache lookup-timeout-seconds must be an integer from 1 to 3600.");
  }
  return seconds * 1_000;
}

function requiredValue(value: string, input: string): string {
  if (value === "") {
    throw new Error(`Input required and not supplied: ${input}`);
  }
  return value;
}
