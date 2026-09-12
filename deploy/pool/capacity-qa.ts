import fs from 'node:fs';
import {execFileSync,spawn} from 'node:child_process';
import assert from 'node:assert/strict';
if(process.getuid?.()!==0)throw new Error('Run as root');
const root=fs.mkdtempSync('/tmp/layercache-pool-capacity-');
const image=`${root}/test.ext4`,mount=`${root}/mounted`;
let mounted=false;
try{
 const fd=fs.openSync(image,'wx',0o600);fs.ftruncateSync(fd,64*1024**2);fs.closeSync(fd);fs.mkdirSync(mount);
 execFileSync('mkfs.ext4',['-q','-m','0',image],{stdio:'pipe'});
 execFileSync('mount',['-o','loop,nodev,nosuid,noexec,discard',image,mount]);mounted=true;
 const fill=(name:string)=>new Promise<number|null>((resolve,reject)=>{const child=spawn('dd',['if=/dev/zero',`of=${mount}/${name}`,'bs=1M','count=48','conv=fsync','status=none'],{stdio:'ignore'});child.on('error',reject);child.on('exit',resolve);});
 const results=await Promise.all([fill('first'),fill('second')]);
 assert.ok(results.some(code=>code!==0),'concurrent writers must hit ENOSPC');
 const stats=fs.statfsSync(mount);assert.ok(stats.blocks*stats.bsize<=64*1024**2);
 const before=fs.statSync(image).blocks*512;
 fs.unlinkSync(`${mount}/first`);fs.unlinkSync(`${mount}/second`);
 execFileSync('sync',['-f',mount]);
 execFileSync('fstrim',[mount]);
 const after=fs.statSync(image).blocks*512;
 assert.ok(after<before/2,'pruning must return most allocated extents to host');
 console.log(JSON.stringify({concurrentExitCodes:results,hardBytes:64*1024**2,allocatedBefore:before,allocatedAfter:after,passed:true}));
}finally{
 if(mounted)execFileSync('umount',[mount]);
 fs.rmSync(root,{recursive:true,force:true});
}
