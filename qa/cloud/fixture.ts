import { createServer } from 'node:http';
import { createServer as createTCPServer } from 'node:net';
import { createHash, randomBytes } from 'node:crypto';
import { mkdtempSync, mkdirSync, statfsSync, writeFileSync, openSync, closeSync } from 'node:fs';
import { spawn, spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { setTimeout as delay } from 'node:timers/promises';
const binary=process.env.LAYERCACHE_BIN;
if(!binary)throw new Error('Set LAYERCACHE_BIN to the built CLI');
const root=mkdtempSync(join(tmpdir(),'layercache-cloud-qa.'));
const users:Record<string,{id:number;login:string}>={alice:{id:1,login:'alice'},bob:{id:2,login:'bob'}};
const repositoryIDs:Record<string,number>={'acme/web':123,'acme/no-admin':124,'acme/zero-id':0};
const codes=new Map<string,{user:string;challenge:string;redirect:string}>();
const github=createServer(async(req,res)=>{
  const url=new URL(req.url!,'http://fixture');res.setHeader('Content-Type','application/json');
  if(url.pathname==='/authorize'){
    const redirect=url.searchParams.get('redirect_uri')!,state=url.searchParams.get('state')!,challenge=url.searchParams.get('code_challenge')!;
    if(url.searchParams.get('code_challenge_method')!=='S256'){res.writeHead(400);res.end('{}');return;}
    res.setHeader('Content-Type','text/html');
    const links=Object.keys(users).map(user=>{const code=randomBytes(16).toString('hex');codes.set(code,{user,challenge,redirect});const target=new URL(redirect);target.searchParams.set('state',state);target.searchParams.set('code',code);return `<a href="${target.href.replaceAll('&','&amp;')}">Sign in as ${user}</a>`;});
    res.end('<!doctype html><html lang="en"><title>Fake GitHub for QA</title><h1>GitHub test identity</h1>'+links.join('<br>')+'</html>');return;
  }
  if(url.pathname==='/token'){
    let body='';for await(const chunk of req)body+=chunk;const form=new URLSearchParams(body),code=form.get('code')!,identity=codes.get(code);codes.delete(code);
    if(!identity||form.get('client_secret')!=='fixture-oauth-secret'||form.get('redirect_uri')!==identity.redirect||createHash('sha256').update(form.get('code_verifier')||'').digest('base64url')!==identity.challenge){res.writeHead(403);res.end('{}');return;}
    res.end(JSON.stringify({access_token:'fixture-github-'+identity.user}));return;
  }
  const user=users[(req.headers.authorization||'').replace('Bearer fixture-github-','')];
  if(!user){res.writeHead(401);res.end('{}');return;}
  if(url.pathname==='/user'){res.end(JSON.stringify(user));return;}
  if(url.pathname.startsWith('/repos/')){const repo=url.pathname.slice('/repos/'.length);if(!(repo in repositoryIDs)){res.writeHead(404);res.end('{}');return;}res.end(JSON.stringify({id:repositoryIDs[repo],full_name:repo,default_branch:'main',permissions:{admin:repo!=='acme/no-admin'}}));return;}
  if(url.pathname==='/users/bob'){res.end(JSON.stringify(users.bob));return;}
  res.writeHead(404);res.end('{}');
});
await new Promise<void>(resolve=>github.listen(0,'127.0.0.1',resolve));
const githubAddress=github.address();if(!githubAddress||typeof githubAddress==='string')throw new Error('GitHub address unavailable');
const githubOrigin=`http://127.0.0.1:${githubAddress.port}`;
const allocator=createTCPServer();await new Promise<void>(resolve=>allocator.listen(0,'127.0.0.1',resolve));const address=allocator.address();if(!address||typeof address==='string')throw new Error('No free port');await new Promise<void>((resolve,reject)=>allocator.close(error=>error?reject(error):resolve()));
const listen=`127.0.0.1:${address.port}`,origin='http://'+listen;
const template=join(root,'template.json');
const setup=spawnSync(binary,['setup','--config',template,'--data-dir',join(root,'template-data'),'--max-size','8388608','--non-interactive','--json'],{encoding:'utf8'});
if(setup.status!==0)throw new Error('Fixture setup failed: '+setup.stderr);
writeFileSync(join(root,'oauth-secret'),'fixture-oauth-secret',{mode:0o600});writeFileSync(join(root,'session-key'),randomBytes(32),{mode:0o600});
const pool=join(root,'pool');mkdirSync(pool,{mode:0o700});writeFileSync(join(pool,'.layercache-pool'),'layercache-pool-v1\n',{mode:0o600});const space=statfsSync(pool);
// Tests use an isolated directory on the host filesystem, not a physically bounded volume.
const config=join(root,'cloud.json');writeFileSync(config,JSON.stringify({listen,origin,dataDir:join(pool,'cloud-data'),githubClientId:'fixture-client',githubClientSecretFile:join(root,'oauth-secret'),sessionKeyFile:join(root,'session-key'),projectTemplateFile:template,storagePool:{path:pool,maxBytes:space.blocks*space.bsize},githubApiUrl:githubOrigin,githubAuthorizeUrl:githubOrigin+'/authorize',githubTokenUrl:githubOrigin+'/token'}),{mode:0o600});
const log=openSync(join(root,'cloud.log'),'w',0o600);const child=spawn(binary,['serve-cloud','--config',config],{stdio:['ignore',log,log]});closeSync(log);
let stop=false;process.on('SIGINT',()=>{stop=true;});process.on('SIGTERM',()=>{stop=true;});
try{
  let ready=false;
  for(let i=0;i<150;i++){if(child.exitCode!==null)throw new Error(`Cloud exited; inspect ${join(root,'cloud.log')}`);try{const response=await fetch(origin+'/healthz',{signal:AbortSignal.timeout(200)});if(response.ok){ready=true;break;}}catch{}await delay(100);}
  if(!ready)throw new Error('Cloud readiness timeout');
  console.log(JSON.stringify({ready:true,root,origin}));
  while(!stop)await delay(100);
}finally{
  child.kill('SIGTERM');for(let i=0;i<100&&child.exitCode===null&&child.signalCode===null;i++)await delay(100);if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL');
  github.closeAllConnections();await new Promise<void>(resolve=>github.close(()=>resolve()));
}
