// Root-only host maintenance. Run without --apply to inspect the plan.
import fs from 'node:fs';
import {execFileSync,spawnSync} from 'node:child_process';
import {join} from 'node:path';
import {registryVictims,type Revision} from './registry-policy.ts';

const platform='/home/hzia/platform',pool=`${platform}/data/layercache-pool`;
if(process.getuid?.()!==0)throw new Error('Run as root');
if(fs.readFileSync(`${pool}/.layercache-pool`,'utf8').trim()!=='layercache-pool-v1')throw new Error('Pool is not mounted');
const lockPath=`${pool}/control/registry.lock`;
if(!fs.existsSync(lockPath))fs.writeFileSync(lockPath,'',{flag:'wx',mode:0o600});
if(!fs.lstatSync(lockPath).isFile()||fs.lstatSync(lockPath).isSymbolicLink())throw new Error('Invalid registry maintenance lock');
fs.chownSync(lockPath,65532,65532);fs.chmodSync(lockPath,0o600);
if(process.env.LAYERCACHE_REGISTRY_LOCKED!=='1'){
 const result=spawnSync('flock',['-E','75','-n',`${pool}/control/registry.lock`,process.execPath,...process.argv.slice(1)],{env:{...process.env,LAYERCACHE_REGISTRY_LOCKED:'1'},stdio:'inherit'});
 process.exit(result.status ?? 1);
}
const root=`${pool}/registry/docker/registry/v2`;
const revisions:Revision[]=[];
const digestPattern=/^sha256:[a-f0-9]{64}$/;
function directories(path:string):string[]{try{return fs.readdirSync(path,{withFileTypes:true}).filter(e=>e.isDirectory()).map(e=>e.name);}catch(error){if(error instanceof Error && 'code' in error && error.code==='ENOENT')return [];throw error;}}
function walk(path:string,repository:string){
 if(fs.existsSync(join(path,'_manifests'))){
  if(!/^(gitenv|agent-access|parle|booker)\/[a-zA-Z0-9_./-]+$/.test(repository))return;
  const tags=new Map<string,{names:string[],createdMs:number}>();
  for(const tag of directories(join(path,'_manifests/tags'))){
   const link=join(path,'_manifests/tags',tag,'current/link');
   const digest=fs.readFileSync(link,'utf8').trim();if(!digestPattern.test(digest))throw new Error('Invalid registry digest');
   const prior=tags.get(digest)??{names:[],createdMs:0};prior.names.push(tag);prior.createdMs=Math.max(prior.createdMs,fs.statSync(link).mtimeMs);tags.set(digest,prior);
  }
  for(const hex of directories(join(path,'_manifests/revisions/sha256'))){
   const digest=`sha256:${hex}`;if(!digestPattern.test(digest))throw new Error('Invalid revision digest');
   const link=join(path,'_manifests/revisions/sha256',hex,'link');const stat=fs.statSync(link);
   const data=join(root,'blobs/sha256',hex.slice(0,2),hex,'data');if(fs.statSync(data).size>16*1024**2)throw new Error('Manifest too large for safe maintenance');
   const manifest=JSON.parse(fs.readFileSync(data,'utf8'));
   const children=(manifest.manifests??[]).map((entry:{digest:string})=>entry.digest);if(manifest.subject?.digest)children.push(manifest.subject.digest);
   if(children.some((d:string)=>!digestPattern.test(d)))throw new Error('Invalid child digest');
   const tag=tags.get(digest);revisions.push({repository,digest,tags:tag?.names??[],createdMs:Math.max(stat.birthtimeMs,tag?.createdMs??0),usedMs:stat.mtimeMs,children});
  }
  return;
 }
 for(const name of directories(path)){if(!name.startsWith('_'))walk(join(path,name),repository?`${repository}/${name}`:name);}
}
walk(`${root}/repositories`,'');
const victims=registryVictims(revisions,Date.now());
console.log(JSON.stringify({manifests:revisions.length,retire: victims.map(r=>({repository:r.repository,digest:r.digest})),apply:process.argv.includes('--apply')}));
if(process.argv.includes('--apply')){
 const password=fs.readFileSync(`${platform}/secrets/layercache/registry/password`,'utf8').trim();
 const authorization=`Basic ${Buffer.from(`layercache:${password}`).toString('base64')}`;
 for(const victim of victims){
  const response=await fetch(`http://127.0.0.1:7440/v2/${victim.repository}/manifests/${victim.digest}`,{method:'DELETE',headers:{authorization},redirect:'error',signal:AbortSignal.timeout(30000)});
  await response.body?.cancel();if(response.status!==202&&response.status!==404)throw new Error(`Registry retirement returned ${response.status}`);
 }
 // Exclusive flock has drained gateway requests. No online writer can race
 // mark-and-sweep. Restart in finally even when the collector fails.
 const docker=(args:string[])=>execFileSync('docker',args,{cwd:platform,stdio:['ignore','pipe','pipe'],maxBuffer:8*1024**2});
 docker(['compose','stop','layercache-registry']);
 try{docker(['compose','run','--rm','--no-deps','layercache-registry','/entrypoint.sh','garbage-collect','/etc/docker/registry/config.yml']);}
 finally{docker(['compose','up','-d','--no-deps','layercache-registry']);}
 console.log('Retired eligible roots and completed offline registry graph collection.');
}
