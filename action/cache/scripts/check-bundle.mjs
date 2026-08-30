import { createHash } from "node:crypto";
import { readdir, readFile } from "node:fs/promises";
import { relative, resolve, sep } from "node:path";
import { spawnSync } from "node:child_process";

const root = resolve(import.meta.dirname, "..");
const dist = resolve(root, "dist");
const before = await snapshot(dist);

if (Object.keys(before).length === 0) {
  throw new Error("action/cache/dist is missing; checked-in bundles are required");
}

const bundle = spawnSync("pnpm", ["bundle"], {
  cwd: root,
  encoding: "utf8",
  shell: process.platform === "win32",
  stdio: "inherit",
});
if (bundle.error) {
  throw bundle.error;
}
if (bundle.status !== 0) {
  process.exit(bundle.status ?? 1);
}

const after = await snapshot(dist);
if (JSON.stringify(after) !== JSON.stringify(before)) {
  const paths = [...new Set([...Object.keys(before), ...Object.keys(after)])].filter(
    (path) => before[path] !== after[path],
  );
  throw new Error(`checked-in action bundle is stale: ${paths.join(", ")}`);
}

process.stdout.write(`Verified ${Object.keys(after).length} reproducible action bundle files.\n`);

async function snapshot(directory) {
  const files = await walk(directory);
  const result = {};
  for (const file of files) {
    const contents = await readFile(file);
    const path = relative(directory, file).split(sep).join("/");
    result[path] = createHash("sha256").update(contents).digest("hex");
  }
  return result;
}

async function walk(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const files = [];
  for (const entry of entries.sort((left, right) => left.name.localeCompare(right.name))) {
    const path = resolve(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...(await walk(path)));
    } else if (entry.isFile()) {
      files.push(path);
    } else {
      throw new Error(`action bundle contains unsupported filesystem entry: ${path}`);
    }
  }
  return files;
}
