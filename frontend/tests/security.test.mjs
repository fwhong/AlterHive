import assert from 'node:assert/strict';
import { test } from 'node:test';
import { build } from 'vite';

const storage = () => {
  const values = new Map();
  return {getItem:key=>values.get(key)??null,setItem:(key,value)=>values.set(key,value),removeItem:key=>values.delete(key)};
};
globalThis.localStorage = storage();
globalThis.sessionStorage = storage();
globalThis.window = {location:{pathname:'/status',href:''}};
// Exercise the real TypeScript clients through the production bundler, no extra test dependency.
const bundle = await build({configFile:false,logLevel:'silent',
 build:{write:false,minify:false,lib:{entry:'tests/client-entry.ts',formats:['es']}}
});
const code=(Array.isArray(bundle)?bundle[0]:bundle).output.find(x=>x.type==='chunk').code;
const client=await import('data:text/javascript;base64,'+Buffer.from(code).toString('base64'));
const requests=[];
const adapter=async config=>{requests.push(config);return {data:{ok:true,data:[]},status:200,statusText:'OK',headers:{},config}};
client.api.defaults.adapter=adapter;client.managerApi.defaults.adapter=adapter;

test('manager never falls back to session token',async()=>{
 client.saveSession('SYNTHETIC-SESSION-TOKEN',{});
 await assert.rejects(()=>client.fetchLLMActive(),/manager.*token/i);
 assert.equal(requests.length,0);
});
test('two planes use distinct credentials and URLs',async()=>{
 client.setManagerToken('SYNTHETIC-MANAGER-TOKEN');
 await client.fetchLLMActive();await client.fetchSystemInfo();
 const [manager,session]=requests.slice(-2);
 assert.equal(manager.baseURL,'/manager');assert.equal(manager.headers.Authorization,'Bearer SYNTHETIC-MANAGER-TOKEN');
 assert.equal(session.baseURL,'');assert.equal(session.headers.Authorization,'Bearer SYNTHETIC-SESSION-TOKEN');
});
test('test/models requests contain no credential or endpoint override',async()=>{
 await client.testLLMProvider('mock');await client.fetchProviderModels('mock');
 for(const request of requests.slice(-2)) {
  assert.equal(request.params,undefined);assert.equal(request.data,undefined);
  assert.ok(!request.url.includes('?'));assert.equal(request.baseURL,'/manager');
 }
});
test('manager auth failure does not log out session plane',async()=>{
 client.managerApi.defaults.adapter=async()=>{throw {response:{status:401}}};
 await assert.rejects(()=>client.fetchLLMActive());
 assert.equal(client.getManagerToken(),'');assert.equal(client.getToken(),'SYNTHETIC-SESSION-TOKEN');
 assert.equal(window.location.href,'');
});
test('logout clears both credentials',()=>{
 client.setManagerToken('SYNTHETIC-MANAGER-TOKEN');client.clearSession();
 assert.equal(client.getToken(),'');assert.equal(client.getManagerToken(),'');
});
