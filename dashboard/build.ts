import { build } from "esbuild";
await build({entryPoints: ["dashboard/app.ts"], outfile: "internal/dashboard/assets/app.js", bundle: true, minify: true, target: "es2022", platform: "browser"});
