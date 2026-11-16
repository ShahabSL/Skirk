import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, writeFile, mkdtemp, rm, mkdir } from 'node:fs/promises';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { startHybridWatch } from './watch.mjs';
import { listenPath } from '../firestore-listen/observer.mjs';

const blobPath = process.env.SKIRK_WEBCHANNEL_BLOB_PATH;
const database = 'projects/skirk-p/databases/skirk-hybrid-probe-abcdef012345';
const document = database + '/documents/owned/down';
const updateTime = '2026-11-15T22:30:01.123456Z';
const base = {hybrid_research:true,bridge_url:'http://127.0.0.1:12345',bridge_auth:'b'.repeat(64),database,document,access_token:'owner-public-hybrid-token',duration_ms:30000,request_limit:256,event_limit:512};
function framed(value) {const raw=JSON.stringify(value);return `${Buffer.byteLength(raw)}\n${raw}`;}
const timeout = (ms, text) => {let id;return {promise:new Promise((_,reject)=>{id=setTimeout(()=>reject(new Error(text)),ms);}),clear:()=>clearTimeout(id)};};

function ownedBackend({missingTime=false,eventStatus=204,idleDelay=0}={}) {
  let initial=false,removed=false,pendingPull=null,sequence=2;
  let found;
  const documentSeen=new Promise(resolve=>{found=resolve;});
  const requests=[],events=[],controls=[];
  const reply=text=>new Response(JSON.stringify({status:200,headers:{'Content-Type':'text/plain','X-HTTP-Session-Id':'hybrid-fixture-session'},body_base64:Buffer.from(text).toString('base64')}),{status:200});
  const removal=()=>reply(framed([[++sequence,[{targetChange:{targetChangeType:'REMOVE',targetIds:[2]}}]]]));
  const fetch=async (url,options)=>{
    const route=new URL(url).pathname;
    assert.equal(new URL(url).hostname,'127.0.0.1');
    assert.equal(options.headers.Authorization,`Bearer ${base.bridge_auth}`);
    const value=JSON.parse(options.body);
    if(route==='/stop'){assert.ok(['observer_close','request_bound'].includes(value.reason));controls.push(value);return new Response(null,{status:204});}
    if(route==='/event') {events.push(value);if(value.kind==='document')found(value);return new Response(null,{status:eventStatus});}
    assert.equal(route,'/request');
    const logical=new URL('https://firestore.googleapis.com'+value.path);
    assert.equal(logical.pathname,listenPath);assert.equal(logical.searchParams.get('database'),database);
    assert.ok(!logical.href.includes(base.access_token));requests.push(value);
    if(logical.searchParams.get('TYPE')==='terminate')return reply('');
    if(!logical.searchParams.has('SID'))return reply(framed([[0,['c','hybrid-fixture-session','',8,'fixture',5000]]]));
    if(value.method==='POST') {
      if(decodeURIComponent(Buffer.from(value.body_base64,'base64').toString()).includes('removeTarget')) {removed=true;if(pendingPull){pendingPull(removal());pendingPull=null;}}
      return reply(framed([1,3,0]));
    }
    if(!initial) {initial=true;return reply(framed([[1,[{targetChange:{targetChangeType:'CURRENT',targetIds:[2]}}]],[2,[{documentChange:{document:{name:document,fields:{sealed:{bytesValue:'cHVibGlj'}},...(missingTime?{}:{updateTime})}}}]]]));}
    if(removed)return removal();
    return new Promise((resolve,reject)=>{
      let timer;
      const finish=result=>{clearTimeout(timer);pendingPull=null;resolve(result);};
      pendingPull=finish;
      if(idleDelay)timer=setTimeout(()=>finish(reply(framed([[++sequence,[{targetChange:{targetChangeType:'NO_CHANGE',targetIds:[]}}]]]))),idleDelay);
      options.signal.addEventListener('abort',()=>{clearTimeout(timer);pendingPull=null;reject(new DOMException('owned abort','AbortError'));},{once:true});
      if(options.signal.aborted){clearTimeout(timer);pendingPull=null;reject(new DOMException('owned abort','AbortError'));}
    });
  };
  return {fetch,requests,events,controls,documentSeen};
}

test('hybrid extensions require explicit finite bounds before SDK access',async()=>{
  for(const patch of [{hybrid_research:false},{duration_ms:300001},{request_limit:257},{event_limit:513},{request_limit:0}]) {
    let calls=0;await assert.rejects(startHybridWatch({...base,...patch},Buffer.alloc(0),()=>{calls++;}),/bounded hybrid/);assert.equal(calls,0);
  }
});

test('official SDK carries exact updateTime and closes all owned work', {skip:!blobPath},async()=>{
  const backend=ownedBackend();const observer=await startHybridWatch(base,await readFile(blobPath),backend.fetch);const deadline=timeout(2000,'no owned state');
  try {const event=await Promise.race([backend.documentSeen,deadline.promise]);assert.equal(event.update_time,updateTime);assert.equal(event.name,document);assert.equal(event.wire_base64,'cHVibGlj');}
  finally {deadline.clear();await observer.close();}
  const state=observer.state();assert.equal(state.error,'');assert.equal(state.stopped,true);assert.equal(state.target_remove_acknowledged,true);assert.equal(state.pending_requests,0);assert.equal(state.pending_events,0);assert.equal(state.pending_sdk_timers,0);
  const first=backend.requests[0];const form=new URLSearchParams(Buffer.from(first.body_base64,'base64').toString());assert.ok([...form.values()].join('\n').includes('Bearer '+base.access_token));
});

test('hybrid document without server updateTime is rejected, never invented',{skip:!blobPath},async()=>{
  const backend=ownedBackend({missingTime:true});const observer=await startHybridWatch(base,await readFile(blobPath),backend.fetch);
  try {const deadline=Date.now()+3000;while(!observer.state().error&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,10));assert.equal(observer.state().error,'MALFORMED_EVENT');assert.equal(backend.events.some(e=>e.kind==='document'),false);}
  finally {await observer.close();}
});

test('active request exhaustion retains failure and reserved REMOVE headroom',{skip:!blobPath},async()=>{
  const backend=ownedBackend({idleDelay:1});const observer=await startHybridWatch({...base,request_limit:16},await readFile(blobPath),backend.fetch);
  try {const deadline=Date.now()+3000;while(!observer.state().error&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,10));assert.equal(observer.state().error,'REQUEST_BOUND');}
  finally {await observer.close();}
  const state=observer.state();assert.equal(state.active_request_count,8);assert.ok(state.cleanup_request_count>0&&state.cleanup_request_count<=8);assert.ok(state.request_count<=16);assert.ok(state.rejected_request_count>=1);assert.equal(state.target_remove_acknowledged,true);assert.equal(state.stopped,true);assert.equal(state.error,'REQUEST_BOUND');assert.deepEqual(backend.controls,[{reason:'request_bound'}]);
});

test('real SDK stays active beyond a 15-second HTTP deadline using bounded empty polls',{skip:!blobPath,timeout:25000},async()=>{
  const backend=ownedBackend({idleDelay:5000});const observer=await startHybridWatch(base,await readFile(blobPath),backend.fetch);const deadline=timeout(2000,'no owned state');
  try {await Promise.race([backend.documentSeen,deadline.promise]);deadline.clear();await new Promise(resolve=>setTimeout(resolve,16000));assert.equal(observer.state().error,'');assert.ok(backend.requests.filter(r=>r.method==='GET').length>=3);}
  finally {deadline.clear();await observer.close();}
  assert.equal(observer.state().stopped,true);assert.equal(observer.state().target_remove_acknowledged,true);
});

test('native loopback HTTP watch child exits naturally with durable zero-pending result',{skip:!blobPath,timeout:12000},async()=>{
  const directory=await mkdtemp(join(tmpdir(),'skirk-hybrid-watch-'));const privateDir=join(directory,'.skirk-runs');await mkdir(privateDir,{mode:0o700});
  const backend=ownedBackend();let server,child,childDone,stderr='';
  try {
    server=createServer(async(req,res)=>{
      let raw='';for await(const chunk of req)raw+=chunk;
      try {
        const options={headers:{Authorization:req.headers.authorization},body:raw,signal:new AbortController().signal};
        const abort=new AbortController();options.signal=abort.signal;res.once('close',()=>{if(!res.writableEnded)abort.abort();});
        const response=await backend.fetch(`http://127.0.0.1:${server.address().port}${req.url}`,options);res.statusCode=response.status;res.end(await response.text());
      } catch {if(!res.destroyed){res.statusCode=502;res.end('bounded fixture failure');}}
    });
    await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
    const config={...base,bridge_url:`http://127.0.0.1:${server.address().port}`,webchannel_blob:join(privateDir,'webchannel_blob_es2018.js')};
    await writeFile(config.webchannel_blob,await readFile(blobPath),{mode:0o600});const configPath=join(privateDir,'hybrid-watch-config.json');await writeFile(configPath,JSON.stringify(config),{mode:0o600});
    child=spawn(process.execPath,[fileURLToPath(new URL('./watch.mjs',import.meta.url)),configPath],{env:{...process.env,SKIRK_FIRESTORE_HYBRID_WATCH:'1',NO_PROXY:'127.0.0.1,localhost,::1'},stdio:['ignore','ignore','pipe']});
    child.stderr.on('data',chunk=>{if(stderr.length<4096)stderr+=chunk.toString();});childDone=new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>resolve({code,signal}));});
    const waitDoc=timeout(3000,'native child did not deliver');await Promise.race([backend.documentSeen,waitDoc.promise]).finally(waitDoc.clear);
    await writeFile(join(privateDir,'stop-hybrid-watch.json'),'{}',{mode:0o600});const waitExit=timeout(6000,'native child did not join');const outcome=await Promise.race([childDone,waitExit.promise]).finally(waitExit.clear);
    assert.deepEqual(outcome,{code:0,signal:null});const state=JSON.parse(await readFile(join(privateDir,'hybrid-watch-result.json'),'utf8'));assert.equal(state.error,'');assert.equal(state.stopped,true);assert.equal(state.target_remove_acknowledged,true);assert.equal(state.pending_requests,0);assert.equal(state.pending_events,0);assert.equal(state.pending_sdk_timers,0);assert.ok(!stderr.includes(base.access_token));
  } finally {
    if(child&&child.exitCode===null&&child.signalCode===null){child.kill('SIGTERM');const term=timeout(1000,'owned child TERM timeout');try{await Promise.race([childDone,term.promise]);}catch{child.kill('SIGKILL');const kill=timeout(2000,'owned child did not reap');await Promise.race([childDone,kill.promise]).finally(kill.clear);}finally{term.clear();}}
    if(server){server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
    await rm(directory,{recursive:true,force:true});
  }
});
