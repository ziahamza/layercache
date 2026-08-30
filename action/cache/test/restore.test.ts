import { describe, expect, it, vi } from "vitest";

import {
  compatibilityForPlatform,
  runRestore,
  runSave,
  type ActionCore,
  type CacheClient,
} from "../src/action.js";

describe("restore action", () => {
  it("uses the Layer Cache v1 endpoint and publishes stock exact-hit outputs", async () => {
    const core = new FakeCore({
      endpoint: "https://cache.layercache.example/v1",
      token: "team-token",
      path: "node_modules\n.turbo",
      key: "linux-node-abc",
      "restore-keys": "linux-node-\nlinux-",
      "lookup-only": "false",
      "fail-on-cache-miss": "false",
      "public-cache-mode": "disabled",
      compatibility: "linux-amd64-schema1-node@24",
    });
    const cache: CacheClient = {
      restoreCache: vi.fn().mockResolvedValue("linux-node-abc"),
      saveCache: vi.fn(),
    };
    const env: NodeJS.ProcessEnv = {
      ACTIONS_CACHE_SERVICE_V2: "true",
      ACTIONS_RESULTS_URL: "https://results.github.example/",
      ACTIONS_CACHE_URL: "https://cache.github.example/",
      ACTIONS_RUNTIME_TOKEN: "github-token",
      GITHUB_SERVER_URL: "https://github.com",
    };

    await runRestore({ core, cache, env });

    expect(cache.restoreCache).toHaveBeenCalledWith(
      ["node_modules", ".turbo"],
      "linux-node-abc",
      ["linux-node-", "linux-"],
      { lookupOnly: false },
    );
    expect(core.outputs).toEqual({
      "cache-primary-key": "linux-node-abc",
      "cache-matched-key": "linux-node-abc",
      "cache-hit": "true",
    });
    expect(core.states).toMatchObject({
      "cache-primary-key": "linux-node-abc",
      "cache-matched-key": "linux-node-abc",
      "cache-paths": '["node_modules",".turbo"]',
    });
    expect(core.secrets).toEqual(["team-token"]);
    expect(core.failures).toEqual([]);
    expect(env.ACTIONS_CACHE_URL).toBe(
      "https://cache.layercache.example/v1/_layercache/compatibility/linux-amd64-schema1-node%4024/",
    );
    expect(env.ACTIONS_RUNTIME_TOKEN).toBe("team-token");
    expect(env.ACTIONS_CACHE_SERVICE_V2).toBeUndefined();
    expect(env.ACTIONS_RESULTS_URL).toBe("https://results.github.example/");
    expect(env.GITHUB_SERVER_URL).toBe("https://github.com");
  });

  it("derives the configured namespace from the Node job platform", () => {
    expect(compatibilityForPlatform("linux", "x64")).toBe("linux-amd64-schema1");
    expect(compatibilityForPlatform("darwin", "arm64")).toBe("darwin-arm64-schema1");
    expect(() => compatibilityForPlatform("win32", "x64")).toThrow("unsupported Node job platform");
  });

  it("rejects a non-canonical compatibility override before contacting the cache", async () => {
    const core = new FakeCore({
      endpoint: "https://cache.layercache.example/",
      token: "team-token",
      compatibility: "Linux X64",
      path: "node_modules",
      key: "key",
      "public-cache-mode": "disabled",
    });
    const cache: CacheClient = { restoreCache: vi.fn(), saveCache: vi.fn() };

    await runRestore({ core, cache, env: {} });

    expect(cache.restoreCache).not.toHaveBeenCalled();
    expect(core.failures).toEqual([
      "Layer Cache compatibility must be at most 256 bytes and use lowercase ASCII letters, digits, and -_.:+@ delimiters.",
    ]);
  });

  it("reports a restore-key hit and saves the primary key in the post action", async () => {
    const core = new FakeCore({
      endpoint: "https://cache.layercache.example/v1/",
      token: "team-token",
      path: "node_modules",
      key: "linux-node-new",
      "restore-keys": "linux-node-",
      "lookup-only": "false",
      "fail-on-cache-miss": "false",
      "public-cache-mode": "disabled",
    });
    const cache: CacheClient = {
      restoreCache: vi.fn().mockResolvedValue("linux-node-old"),
      saveCache: vi.fn().mockResolvedValue(42),
    };
    const env: NodeJS.ProcessEnv = {};

    await runRestore({ core, cache, env });
    await runSave({ core, cache, env });

    expect(core.outputs).toMatchObject({
      "cache-primary-key": "linux-node-new",
      "cache-matched-key": "linux-node-old",
      "cache-hit": "false",
    });
    expect(cache.saveCache).toHaveBeenCalledWith(["node_modules"], "linux-node-new");
    expect(core.failures).toEqual([]);
  });

  it("publishes empty miss outputs, fails when requested, and skips lookup-only save", async () => {
    const core = new FakeCore({
      endpoint: "https://cache.layercache.example/v1/",
      token: "team-token",
      path: "node_modules",
      key: "missing-key",
      "restore-keys": "",
      "lookup-only": "true",
      "fail-on-cache-miss": "true",
      "public-cache-mode": "disabled",
    });
    const cache: CacheClient = {
      restoreCache: vi.fn().mockResolvedValue(undefined),
      saveCache: vi.fn(),
    };
    const env: NodeJS.ProcessEnv = {};

    await runRestore({ core, cache, env });
    await runSave({ core, cache, env });

    expect(cache.restoreCache).toHaveBeenCalledWith(["node_modules"], "missing-key", [], { lookupOnly: true });
    expect(cache.saveCache).not.toHaveBeenCalled();
    expect(core.outputs).toEqual({
      "cache-primary-key": "missing-key",
      "cache-matched-key": "",
      "cache-hit": "",
    });
    expect(core.failures).toEqual([
      "Layer Cache did not find a matching cache entry and fail-on-cache-miss is enabled.",
    ]);
  });

  it("refuses Public Cache until verified-artifact restore exists", async () => {
    const core = new FakeCore({
      endpoint: "https://cache.layercache.example/v1/",
      token: "team-token",
      path: "node_modules",
      key: "public-key",
      "restore-keys": "",
      "lookup-only": "false",
      "fail-on-cache-miss": "false",
      "public-cache-mode": "verified-artifact",
    });
    const cache: CacheClient = {
      restoreCache: vi.fn(),
      saveCache: vi.fn(),
    };

    await runRestore({ core, cache, env: {} });

    expect(cache.restoreCache).not.toHaveBeenCalled();
    expect(core.failures).toEqual([
      "Public Cache restore is disabled in this action. A verified-artifact mode must verify signed provenance and the complete digest before extraction.",
    ]);
  });

  it("suppresses post save after an exact primary-key hit", async () => {
    const core = new FakeCore({
      endpoint: "https://cache.layercache.example/v1/",
      token: "team-token",
      path: "node_modules",
      key: "exact-key",
      "restore-keys": "",
      "lookup-only": "false",
      "fail-on-cache-miss": "false",
      "public-cache-mode": "disabled",
    });
    const cache: CacheClient = {
      restoreCache: vi.fn().mockResolvedValue("exact-key"),
      saveCache: vi.fn(),
    };
    const env: NodeJS.ProcessEnv = {};

    await runRestore({ core, cache, env });
    await runSave({ core, cache, env });

    expect(cache.saveCache).not.toHaveBeenCalled();
    expect(core.failures).toEqual([]);
  });

  it("does not report success when the cache client declines publication", async () => {
    const core = new FakeCore({
      endpoint: "https://cache.layercache.example/v1/",
      token: "team-token",
      path: "node_modules",
      key: "missed-key",
      "restore-keys": "",
      "lookup-only": "false",
      "fail-on-cache-miss": "false",
      "public-cache-mode": "disabled",
    });
    const cache: CacheClient = {
      restoreCache: vi.fn().mockResolvedValue(undefined),
      saveCache: vi.fn().mockResolvedValue(-1),
    };

    await runRestore({ core, cache, env: {} });
    await runSave({ core, cache, env: {} });

    expect(cache.saveCache).toHaveBeenCalledWith(["node_modules"], "missed-key");
    expect(core.infos).not.toContain("Layer Cache saved missed-key.");
    expect(core.warnings).toContain("Layer Cache did not publish missed-key; see the cache client diagnostics.");
    expect(core.failures).toEqual([]);
  });
});

class FakeCore implements ActionCore {
  readonly outputs: Record<string, string> = {};
  readonly states: Record<string, string> = {};
  readonly failures: string[] = [];
  readonly secrets: string[] = [];
  readonly infos: string[] = [];
  readonly warnings: string[] = [];

  constructor(private readonly inputs: Record<string, string>) {}

  getInput(name: string, options?: { required?: boolean }): string {
    const value = this.inputs[name] ?? "";
    if (options?.required && value === "") {
      throw new Error(`Input required and not supplied: ${name}`);
    }
    return value;
  }

  getMultilineInput(name: string, options?: { required?: boolean }): string[] {
    const value = this.getInput(name, options);
    return value === ""
      ? []
      : value
          .split("\n")
          .map((line) => line.trim())
          .filter(Boolean);
  }

  getBooleanInput(name: string): boolean {
    return this.getInput(name).toLowerCase() === "true";
  }

  setOutput(name: string, value: unknown): void {
    this.outputs[name] = String(value);
  }

  saveState(name: string, value: unknown): void {
    this.states[name] = String(value);
  }

  getState(name: string): string {
    return this.states[name] ?? "";
  }

  setFailed(message: string | Error): void {
    this.failures.push(String(message instanceof Error ? message.message : message));
  }

  setSecret(secret: string): void {
    this.secrets.push(secret);
  }

  info(message: string): void {
    this.infos.push(message);
  }

  warning(message: string | Error): void {
    this.warnings.push(String(message instanceof Error ? message.message : message));
  }
}
