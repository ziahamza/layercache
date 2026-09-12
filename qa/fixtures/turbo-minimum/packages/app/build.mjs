import { appendFile, mkdir, writeFile } from "node:fs/promises";

await mkdir(new URL("./dist/", import.meta.url), { recursive: true });
await writeFile(
  new URL("./dist/result.txt", import.meta.url),
  "layercache turbo minimum fixture\n",
);

if (process.env.EXECUTION_COUNTER) {
  await appendFile(process.env.EXECUTION_COUNTER, "executed\n");
}
