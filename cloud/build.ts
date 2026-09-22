import { build } from 'esbuild';
await build({entryPoints:['cloud/app.ts'],outfile:'internal/portal/assets/app.js',bundle:true,minify:true,target:'es2022',platform:'browser'});
