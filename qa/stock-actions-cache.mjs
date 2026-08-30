import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { saveCache, restoreCache } from "../action/cache/node_modules/@actions/cache/lib/cache.js";

const [operation, workspace, key] = process.argv.slice(2);
if (!operation || !workspace || !key) {
  throw new Error("usage: node qa/stock-actions-cache.mjs save|restore WORKSPACE KEY");
}

process.env.GITHUB_WORKSPACE = workspace;
process.env.RUNNER_TEMP ||= join(workspace, ".runner-temp");
process.env.RUNNER_OS ||= "Linux";
process.env.GITHUB_REF ||= "refs/heads/main";
process.chdir(workspace);
await mkdir(process.env.RUNNER_TEMP, { recursive: true });

const cachePath = join(workspace, ".fixture-cache");
const valuePath = join(cachePath, "value.txt");
const declaredPaths = [".fixture-cache"];

if (operation === "save") {
  await mkdir(cachePath, { recursive: true });
  await writeFile(valuePath, "restored by stock actions/cache\n");
  const cacheID = await saveCache(declaredPaths, key, {
    uploadConcurrency: 2,
    uploadChunkSize: 1024 * 1024,
  });
  if (cacheID < 1) {
    throw new Error(`cache was not saved: ${cacheID}`);
  }
  process.stdout.write(`saved:${cacheID}\n`);
} else if (operation === "restore") {
  await rm(cachePath, { recursive: true, force: true });
  const matched = await restoreCache(declaredPaths, key, []);
  const value = await readFile(valuePath, "utf8");
  if (matched !== key || value !== "restored by stock actions/cache\n") {
    throw new Error(`unexpected restore: key=${matched} value=${JSON.stringify(value)}`);
  }
  process.stdout.write(`restored:${matched}\n`);
} else {
  throw new Error(`unknown operation ${operation}`);
}
