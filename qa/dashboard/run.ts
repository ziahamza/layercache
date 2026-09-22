import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import { resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const binary = process.env.LAYERCACHE_BIN;
if (!binary) throw new Error('Build the CLI and set LAYERCACHE_BIN to its absolute path');
const fixture = spawn(process.execPath, [resolve('qa/dashboard/fixture.ts')], {env: process.env, stdio:['ignore','pipe','inherit']});
const cleanup = async (child: ChildProcess) => {
  if (child.exitCode !== null || child.signalCode !== null) return;
  child.kill('SIGTERM');
  for (let i=0;i<100&&child.exitCode===null&&child.signalCode===null;i++) await delay(100);
  if (child.exitCode===null&&child.signalCode===null) child.kill('SIGKILL');
};
let browser: ChildProcess | undefined;
const stop = () => { browser?.kill('SIGTERM'); fixture.kill('SIGTERM'); };
process.on('SIGINT', stop); process.on('SIGTERM', stop);
try {
  const linkFile = await new Promise<string>((resolveReady,reject) => {
    const lines = createInterface({input:fixture.stdout!});
    const timeout = setTimeout(()=>{lines.close();reject(new Error('Fixture readiness timed out'));},30000);
    fixture.once('error',reject);
    fixture.once('exit',()=>{clearTimeout(timeout);reject(new Error('Fixture exited before readiness'));});
    lines.on('line',line=>{
      if(!line.startsWith('{'))return;
      try { const info: {ready?:boolean;root?:string;linkFile?:string}=JSON.parse(line); if(info.ready&&info.linkFile){clearTimeout(timeout);lines.close();console.log(`Dashboard fixture: ${info.root}`);resolveReady(info.linkFile);} } catch(error){clearTimeout(timeout);reject(error);}
    });
  });
  browser=spawn(process.execPath,[resolve('qa/dashboard/browser.ts')],{env:{...process.env,DASHBOARD_LINK_FILE:linkFile},stdio:'inherit'});
  const code=await new Promise<number>((resolveExit,reject)=>{browser!.once('error',reject);browser!.once('exit',code=>resolveExit(code??1));});
  process.exitCode=code;
} finally {
  if(browser)await cleanup(browser);
  await cleanup(fixture);
}
