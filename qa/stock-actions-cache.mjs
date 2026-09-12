import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { isAbsolute, join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [operation, workspace, key, clientModule] = process.argv.slice(2);
if (!operation || !workspace || !key) {
  throw new Error(
    "usage: node qa/stock-actions-cache.mjs miss-save|save|restore WORKSPACE KEY [CLIENT_MODULE]",
  );
}

const defaultClientModule = new URL(
  "../action/cache/node_modules/@actions/cache/lib/cache.js",
  import.meta.url,
);
const clientURL = clientModule
  ? pathToFileURL(isAbsolute(clientModule) ? clientModule : resolve(clientModule))
  : defaultClientModule;
const { saveCache, restoreCache } = await import(clientURL.href);

process.env.GITHUB_WORKSPACE = workspace;
process.env.RUNNER_TEMP ||= join(workspace, ".runner-temp");
process.env.RUNNER_OS ||= "Linux";
process.env.GITHUB_REF ||= "refs/heads/main";
process.chdir(workspace);
await mkdir(process.env.RUNNER_TEMP, { recursive: true });

const cachePath = join(workspace, ".fixture-cache");
const valuePath = join(cachePath, "value.txt");
const declaredPaths = [".fixture-cache"];

if (operation === "miss-save") {
  await rm(cachePath, { recursive: true, force: true });
  const matched = await restoreCache(declaredPaths, key, []);
  if (matched !== undefined) {
    throw new Error(`expected a cold cache miss, restored ${matched}`);
  }
  await mkdir(cachePath, { recursive: true });
  await writeFile(valuePath, "restored by stock actions/cache\n");
  const cacheID = await saveCache(declaredPaths, key, {
    uploadConcurrency: 2,
    uploadChunkSize: 1024 * 1024,
  });
  if (cacheID < 1) {
    throw new Error(`cache was not saved after a cold miss: ${cacheID}`);
  }
  process.stdout.write(`missed-and-saved:${cacheID}\n`);
} else if (operation === "save") {
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
