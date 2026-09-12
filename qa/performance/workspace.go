package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

const fixtureRepository = "https://github.com/layercache/performance-fixture.git"

const buildScript = `import { pbkdf2Sync } from "node:crypto";
import { readFileSync, mkdirSync, writeFileSync } from "node:fs";
const { seed, iterations } = JSON.parse(readFileSync(new URL("fixture.json", import.meta.url)));
const started = performance.now();
const result = pbkdf2Sync(seed, "layercache-performance", iterations, 32, "sha256");
mkdirSync(new URL("dist", import.meta.url), { recursive: true });
writeFileSync(new URL("dist/result.txt", import.meta.url), result.toString("hex") + "\n");
console.log("executed CPU fixture in " + (performance.now() - started).toFixed(1) + "ms");
`

func createTemplate(ctx context.Context, directory string) error {
	files := map[string]string{
		"package.json":              `{"name":"layercache-performance","private":true,"packageManager":"pnpm@10.34.5"}`,
		"pnpm-workspace.yaml":       "packages:\n  - packages/*\n",
		"pnpm-lock.yaml":            "lockfileVersion: '9.0'\nsettings:\n  autoInstallPeers: true\n  excludeLinksFromLockfile: false\nimporters:\n  .: {}\n  packages/app: {}\n",
		"turbo.json":                `{"tasks":{"build":{"outputs":["dist/**"]}}}`,
		".gitignore":                "node_modules/\n.turbo/\ndist/\n",
		"packages/app/package.json": `{"name":"@layercache/performance-app","private":true,"scripts":{"build":"node build.mjs"}}`,
		"packages/app/build.mjs":    buildScript,
		"packages/app/fixture.json": `{"seed":"template","iterations":1}`,
	}
	for name, contents := range files {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return err
		}
	}
	for _, arguments := range [][]string{
		{"init", "--quiet", "-b", "main"},
		{"remote", "add", "origin", fixtureRepository},
		{"symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main"},
		{"add", "."},
		{"-c", "user.name=Layer Cache QA", "-c", "user.email=qa@layercache.dev", "commit", "--quiet", "-m", "Performance fixture"},
	} {
		if _, _, err := execute(ctx, directory, "git", arguments...); err != nil {
			return err
		}
	}
	return nil
}

func cloneWorkspace(ctx context.Context, template, directory, seed string, iterations int) error {
	if _, _, err := execute(ctx, filepath.Dir(directory), "git", "clone", "--quiet", "--no-hardlinks", template, directory); err != nil {
		return err
	}
	if _, _, err := execute(ctx, directory, "git", "remote", "set-url", "origin", fixtureRepository); err != nil {
		return err
	}
	input, err := json.Marshal(struct {
		Seed       string `json:"seed"`
		Iterations int    `json:"iterations"`
	}{seed, iterations})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "packages/app/fixture.json"), input, 0o600)
}
