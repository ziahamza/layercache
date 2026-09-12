import { describe, expect, it, vi } from "vitest";

const verifiedIdentities = vi.hoisted(() => [] as Array<{ repository: string; target: string }>);

vi.mock("../src/verified-cache.js", async () => {
  const actual = await vi.importActual<typeof import("../src/verified-cache.js")>("../src/verified-cache.js");
  return {
    ...actual,
    VerifiedCacheClient: class {
      constructor(options: { identity: { repository: string; target: string } }) {
        verifiedIdentities.push({
          repository: options.identity.repository,
          target: options.identity.target,
        });
      }

      async restoreCache(): Promise<string | undefined> {
        throw new actual.VerifiedCacheSafetyError(
          [new Error("restore failed"), new Error("rollback failed")],
          "/tmp/layercache-preserved-recovery",
        );
      }

      async saveCache(): Promise<number> {
        return -1;
      }
    },
  };
});

import { runRestore, type ActionCore, type CacheClient } from "../src/action.js";

describe("verified restore failure policy", () => {
  it("hard-fails when a verified restore cannot roll back safely", async () => {
    const core = new SafetyCore({
      endpoint: "https://cache.layercache.example/",
      token: "team-token",
      path: "node_modules",
      key: "verified-key",
      "public-cache-mode": "verified",
      "public-trust-key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
      "public-recipe-digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "public-platform": "linux/amd64",
      "public-toolchain": "actions/cache@6.2.0",
      "public-builder": "layercache-public-builder-v1",
      compatibility: "linux-amd64-schema1",
    });
    const cache: CacheClient = { restoreCache: vi.fn(), saveCache: vi.fn() };

    await runRestore({
      core,
      cache,
      env: {
        GITHUB_REPOSITORY: "Acme/Widget",
        GITHUB_REF: "refs/heads/main",
        GITHUB_SHA: "0123456789abcdef0123456789abcdef01234567",
        GITHUB_JOB: "public-cache",
        GITHUB_WORKFLOW_REF: "Acme/Widget/.github/workflows/public-cache.yml@refs/heads/main",
      },
    });

    expect(core.failures).toEqual([
      "Actions cache restore rollback was incomplete; recovery data remains at /tmp/layercache-preserved-recovery.",
    ]);
    expect(core.warnings).toEqual([]);
    expect(verifiedIdentities).toEqual([{
      repository: "acme/widget",
      target: ".github/workflows/public-cache.yml#public-cache",
    }]);
  });

  it("rejects a malformed GitHub repository before authentication or restore", async () => {
    const core = new SafetyCore({
      endpoint: "https://cache.layercache.example/",
      project: "github.com/acme/widget",
      path: "node_modules",
      key: "verified-key",
      "public-cache-mode": "verified",
      "public-trust-key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
      "public-recipe-digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "public-platform": "linux/amd64",
      "public-toolchain": "actions/cache@6.2.0",
      "public-builder": "layercache-public-builder-v1",
      compatibility: "linux-amd64-schema1",
    });
    const cache: CacheClient = { restoreCache: vi.fn(), saveCache: vi.fn() };

    await runRestore({
      core,
      cache,
      env: {
        GITHUB_REPOSITORY: "acme/widget/extra",
        GITHUB_REF: "refs/heads/main",
        GITHUB_SHA: "0123456789abcdef0123456789abcdef01234567",
        GITHUB_JOB: "public-cache",
        GITHUB_WORKFLOW_REF: "acme/widget/.github/workflows/public-cache.yml@refs/heads/main",
      },
    });

    expect(core.failures).toEqual(["GITHUB_REPOSITORY must use the GitHub owner/repository format."]);
    expect(core.warnings).toEqual([]);
    expect(cache.restoreCache).not.toHaveBeenCalled();
  });
});

class SafetyCore implements ActionCore {
  readonly failures: string[] = [];
  readonly warnings: string[] = [];
  private readonly states = new Map<string, string>();

  constructor(private readonly inputs: Record<string, string>) {}

  getInput(name: string, options?: { required?: boolean }): string {
    const value = this.inputs[name] ?? "";
    if (options?.required === true && value === "") {
      throw new Error(`Input required and not supplied: ${name}`);
    }
    return value;
  }

  getMultilineInput(name: string, options?: { required?: boolean }): string[] {
    return this.getInput(name, options).split("\n").filter(Boolean);
  }

  getBooleanInput(name: string): boolean {
    return this.getInput(name) === "true";
  }

  async getIDToken(): Promise<string> {
    throw new Error("OIDC should not be used with a fallback token");
  }

  setOutput(): void {}

  saveState(name: string, value: unknown): void {
    this.states.set(name, String(value));
  }

  getState(name: string): string {
    return this.states.get(name) ?? "";
  }

  setFailed(message: string | Error): void {
    this.failures.push(message instanceof Error ? message.message : message);
  }

  setSecret(): void {}

  info(): void {}

  warning(message: string | Error): void {
    this.warnings.push(message instanceof Error ? message.message : message);
  }
}
