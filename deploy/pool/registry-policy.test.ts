import {test} from 'node:test';
import assert from 'node:assert/strict';
import {registryVictims, type Revision} from './registry-policy.ts';
const day=86400_000,now=100*day;
const item=(digest:string,patch:Partial<Revision>={}):Revision=>({repository:'app/cache',digest,tags:[],createdMs:0,usedMs:0,children:[],...patch});
test('expires stale roots even below capacity, protects keep tags and active index children',()=>{
 const rows=[item('old'),item('pin',{tags:['keep-release']}),item('index',{tags:['main'],createdMs:now,usedMs:now,children:['child']}),item('child')];
 assert.deepEqual(registryVictims(rows,now).map(r=>r.digest),['old']);
});
test('probation and newest three roots survive; old excess roots are collected',()=>{
 const rows=Array.from({length:5},(_,i)=>item(String(i),{tags:[`build-${i}`],createdMs:now-(i+2)*day,usedMs:now-2*day}));
 rows.push(item('new',{createdMs:now-100}));
 assert.deepEqual(registryVictims(rows,now).map(r=>r.digest),['3','4']);
});
test('retained cross-repository alias protects shared children; cycles terminate',()=>{
 const rows=[item('a',{children:['b']}),item('b',{children:['a']}),item('a',{repository:'other/cache',tags:['keep-main'],children:['b']})];
 assert.deepEqual(registryVictims(rows,now),[]);
});
