#!/usr/bin/env python3
"""Real Docker verification; only synthetic keys and a private mock provider.
Build once: scripts/build-local-image.sh alterhive:split
Run: ALTERHIVE_IMAGE=alterhive:split python3 scripts/verify-split.py
Requires Docker, Go, Python 3, ssh/expect. Finally removes only its own project.
"""
import json,os,pathlib,shutil,socket,subprocess,tempfile,time,urllib.request,urllib.error,uuid
ROOT=pathlib.Path(__file__).resolve().parent.parent
WORK=pathlib.Path(os.environ.get('VERIFY_WORK') or tempfile.mkdtemp(prefix='ah-split-'));WORK.mkdir(parents=True,exist_ok=True)
IMAGE=os.environ.get('ALTERHIVE_IMAGE','alterhive:split');PROJECT='ahverify-'+uuid.uuid4().hex[:8]
ADMIN='SYNTHETIC-MANAGER-ADMIN-'+uuid.uuid4().hex;OPS='SYNTHETIC-INGRESS-ADMIN-'+uuid.uuid4().hex
KEY='SYNTHETIC-PROVIDER-SECRET-'+uuid.uuid4().hex
results=[]
def run(args,check=True,env=None,timeout=120):
 p=subprocess.run(args,text=True,capture_output=True,env=env,timeout=timeout,cwd=ROOT)
 if check and p.returncode:raise RuntimeError(' '.join(args[:6])+'\n'+p.stdout+p.stderr)
 return p

def check(name,condition,detail=''):
 results.append({'test':name,'passed':bool(condition),'detail':detail});print(('PASS' if condition else 'FAIL')+': '+name,flush=True)
 if not condition:raise AssertionError(name+': '+str(detail))
def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
def request(base,path,body=None,token='',method=None):
 headers={'Content-Type':'application/json'}
 if token:headers['Authorization']='Bearer '+token
 req=urllib.request.Request(base+path,data=json.dumps(body).encode() if body is not None else None,headers=headers,method=method)
 try:
  with urllib.request.urlopen(req,timeout=5) as r:return r.status,json.loads(r.read())
 except urllib.error.HTTPError as e:
  raw=e.read().decode()
  try:body=json.loads(raw)
  except ValueError:body=raw
  return e.code,body
apiport,sshport,adminport=port(),port(),port();API=f'http://127.0.0.1:{apiport}';ADMINURL=f'http://127.0.0.1:{adminport}'
# Use the actual checked-in deployment, substituting only test image/config/ports.
shutil.copy(ROOT/'deploy/manager-sensor/docker-compose.yml',WORK/'base.yml')
(WORK/'manager.env').write_text('ALTERHIVE_MOCK_API_KEY='+KEY+'\n')
(WORK/'manager-configs').mkdir(exist_ok=True);(WORK/'sensor-configs/services').mkdir(parents=True,exist_ok=True)
(WORK/'manager-configs/llm.yaml').write_text('''enabled: true
active_provider: mock
providers:
  - id: mock
    type: openai
    base_url: http://mock:9999/v1
    api_key: ""
    model: mock-model
    enabled: true
''')
shutil.copy(ROOT/'configs/topology.yaml',WORK/'sensor-configs/topology.yaml');shutil.copy(ROOT/'configs/services/ssh.yaml',WORK/'sensor-configs/services/ssh.yaml')
env=dict(os.environ,ALTERHIVE_IMAGE=IMAGE,ALTERHIVE_ADMIN_TOKEN=ADMIN,ALTERHIVE_SENSOR_ADMIN_TOKEN=OPS,SENSOR_API_PUBLISH=f'127.0.0.1:{apiport}',SENSOR_SSH_PUBLISH=f'127.0.0.1:{sshport}',MANAGER_API_PUBLISH=f'127.0.0.1:{adminport}')
cfg=json.loads(run(['docker','compose','-f',str(WORK/'base.yml'),'config','--format','json'],env=env).stdout)
cfg['name']=PROJECT;cfg['services'].pop('frontend',None)
for n in cfg['networks']:cfg['networks'][n]['name']=PROJECT+'-'+n
for n in cfg['volumes']:cfg['volumes'][n]['name']=PROJECT+'-'+n
for role,s in cfg['services'].items():s['container_name']=PROJECT+'-'+role
arch=run(['docker','info','--format','{{.Architecture}}']).stdout.strip();arch='arm64' if arch in ('arm64','aarch64') else 'amd64'
goenv=dict(os.environ,CGO_ENABLED='0',GOOS='linux',GOARCH=arch)
run(['go','build','-o',str(WORK/'probe'),str(ROOT/'scripts/split-probe.go')],env=goenv)
run(['go','test','-c','-o',str(WORK/'sandbox.test'),'./internal/sandbox'],env=goenv)
for s in cfg['services'].values():s['volumes'].append({'type':'bind','source':str(WORK/'probe'),'target':'/test/probe','read_only':True})
cfg['services']['mock']={'image':IMAGE,'container_name':PROJECT+'-mock','entrypoint':['/test/probe'],'user':'10004:20000','volumes':[{'type':'bind','source':str(WORK/'probe'),'target':'/test/probe','read_only':True}],'networks':{'ops':{}},'read_only':True,'cap_drop':['ALL']}
CF=WORK/'compose.json';CF.write_text(json.dumps(cfg,indent=2));COMPOSE=['docker','compose','-p',PROJECT,'-f',str(CF)]
def execprobe(role,path,method='GET',body='',token='',uid=None,unix=False):
 args=['docker','exec']
 if uid is not None:args+=['--user',uid]
 args+=[PROJECT+'-'+role,'/test/probe','request',(unix if isinstance(unix,str) else '/run/alterhive/decisions/decision.sock' if unix else '-'),path,method,body,token]
 p=run(args,False)
 if p.returncode:return 0,p.stdout+p.stderr
 status,_,data=p.stdout.partition('\n')
 try:data=json.loads(data)
 except ValueError:pass
 return int(status),data

def ready():
 for _ in range(100):
  try:
   if request(API,'/healthz')[0]==200:return
  except Exception:pass
  time.sleep(.15)
 raise RuntimeError('sensor did not become healthy')
def count():
 code,records=execprobe('mock','http://127.0.0.1:9999/records')
 if code!=200:raise RuntimeError('mock records unavailable: '+str(records))
 return len(records or [])
def ssh(command):
 # Real terminal output; never accept target IP merely echoed by spawn.
 program='''set timeout 8
spawn -noecho ssh -T -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o PreferredAuthentications=password -o PubkeyAuthentication=no -p $env(AH_TEST_SSH_PORT) root@127.0.0.1 $env(AH_TEST_COMMAND)
expect {\n "*password: " {send "123456\\r"; exp_continue}\n eof {}\n timeout {exit 2}\n}
'''
 return run(['expect','-c',program],False,dict(os.environ,AH_TEST_SSH_PORT=str(sshport),AH_TEST_COMMAND=command),timeout=15).stdout
try:
 run(COMPOSE+['up','-d']);ready()
 check('sensor healthy through ingress Unix proxy',True)
 inspect=json.loads(run(['docker','inspect',PROJECT+'-sensor']).stdout)[0]
 check('sensor has network_mode:none',inspect['HostConfig']['NetworkMode']=='none')
 keys={s.split('=',1)[0] for s in inspect['Config']['Env']}
 check('sensor contains no provider/admin/manager credential env',not any('TOKEN' in k or 'KEY' in k or 'SECRET' in k for k in keys),sorted(keys))
 check('sensor no manager config mount',not any('manager-configs' in m['Source'] for m in inspect['Mounts']))
 status=run(['docker','exec',PROJECT+'-sensor','/test/probe','read','/proc/1/status']).stdout
 check('production sensor has all-thread application seccomp', 'Seccomp:\t2' in status and int(status.split('Seccomp_filters:\t')[1].split()[0])>=2,status)
 check('sensor has no capabilities or privilege escalation', 'CapEff:\t0000000000000000' in status and 'NoNewPrivs:\t1' in status)
 ingress_status=run(['docker','exec',PROJECT+'-ingress','/test/probe','read','/proc/1/status']).stdout
 check('production ingress egress also locked',int(ingress_status.split('Seccomp_filters:\t')[1].split()[0])>=2)
 check('sensor image contains no llm.yaml',run(['docker','exec',PROJECT+'-sensor','/test/probe','read','/app/configs/llm.yaml'],False).returncode!=0)
 # Linux package regression tests verify the exact process filter, including Go worker threads.
 p=run(['docker','run','--rm','--network','none','--user','10002:20000','--cap-drop','ALL','--security-opt','no-new-privileges:true','-v',str(WORK/'sandbox.test')+':/test/sandbox:ro','--entrypoint','/test/sandbox',IMAGE,'-test.v'],False)
 check('Linux seccomp denies IPv4/IPv6/UDP/packet/netlink; Unix allowed',p.returncode==0,p.stdout+p.stderr)
 for path in ['/api/v1/llm/providers','/api/v1/llm/active']:
  code,body=execprobe('sensor','http://manager'+path,unix=True)
  check('decision UDS excludes admin '+path,code==404,body)
 code,_=execprobe('sensor','http://manager/healthz',unix=True,uid='10003:20000')
 check('decision UDS rejects wrong peer UID',code==0)
 code,_=execprobe('sensor','http://manager/healthz',unix=True)
 check('decision UDS accepts sensor UID without token',code==200)
 code,_=execprobe('sensor','http://sensor/healthz',unix='/run/alterhive/ingress/api.sock')
 check('sensor API UDS rejects non-ingress UID',code==0)
 code,_=execprobe('sensor','http://sensor/healthz',uid='10003:20000',unix='/run/alterhive/ingress/api.sock')
 check('sensor API UDS accepts ingress UID',code==200)
 for token in ['', 'alterhive-admin-token',OPS]:
  check('manager rejects missing/default/ingress token '+str(bool(token)),request(ADMINURL,'/api/v1/llm/providers',token=token)[0]==401)
 code,body=request(ADMINURL,'/api/v1/auth/login',{'username':'admin','password':'admin'})
 check('manager rejects admin/admin',code==401)
 code,body=request(ADMINURL,'/api/v1/llm/providers',token=ADMIN)
 check('provider list contains presence flag, not key',code==200 and body['data'][0]['key_configured'] and KEY not in json.dumps(body) and body['data'][0]['api_key']=='')
 code,body=request(ADMINURL,'/api/v1/auth/login',{'username':'admin','password':ADMIN})
 check('configured manager login works',code==200 and body['data']['access_token']==ADMIN)
 before=count();code,body=request(ADMINURL,'/api/v1/llm/providers/mock/models?base_url=http://mock:9999/capture',token=ADMIN)
 check('models override cannot forward env key',not body.get('ok') and count()==before,body)
 code,body=request(ADMINURL,'/api/v1/llm/enabled',{'enabled':False},ADMIN,'PUT')
 _,after=request(ADMINURL,'/api/v1/llm/active',token=ADMIN)
 check('read-only update leaves active state unchanged',code>=400 and after['data']['enabled'] and after['data']['is_active'],body)
 check('ingress requires auth for simulation',request(API,'/api/v1/simulate/nmap',{'target':'192.168.56.60'})[0]==401)
 check('ingress has no provider API',request(API,'/api/v1/llm/providers',token=OPS)[0]==404)
 check('ingress admin/admin does not recover token',request(API,'/api/v1/auth/login',{'username':'admin','password':'admin'})[0]==401)
 mockip=json.loads(run(['docker','inspect',PROJECT+'-mock']).stdout)[0]['NetworkSettings']['Networks'][PROJECT+'-ops']['IPAddress']
 check('compromised sensor cannot reach external witness network',execprobe('sensor','http://'+mockip+':9999/')[0]==0)
 code,body=request(API,'/api/v1/simulate/nmap',{'target':'192.168.56.60'},OPS)
 check('deterministic first response',code==200 and '192.168.56.60' in body['output'] and 'MOCKLLM-RENDER' not in body['output'])
 sid=body['session_id']
 for _ in range(40):
  time.sleep(.1);code,body=request(API,'/api/v1/simulate/nmap',{'target':'192.168.56.60','session_id':sid},OPS)
  if 'MOCKLLM-RENDER' in body['output']:break
 check('next command applies async LLM result','MOCKLLM-RENDER' in body['output'],body.get('output'))
 records=execprobe('mock','http://127.0.0.1:9999/records')[1]
 check('only manager sends synthetic provider key',bool(records) and all(x['auth']=='Bearer '+KEY for x in records))
 check('provider key absent from sensor response',KEY not in json.dumps(body))
 out=ssh('nmap -sV 192.168.56.60');check('real SSH command reaches isolated sensor','Nmap scan report' in out,out)
 for _ in range(40):
  time.sleep(.2);out=ssh('nmap -sV 192.168.56.60')
  if 'MOCKLLM-RENDER' in out:break
 check('real SSH returns LLM cached result','MOCKLLM-RENDER' in out,out)
 # Restart sensor with export disabled; a new process has no in-flight context.
 cfg['services']['sensor']['environment']['ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT']='false';CF.write_text(json.dumps(cfg))
 run(COMPOSE+['up','-d','--no-deps','sensor']);ready();time.sleep(.5);before=count()
 request(API,'/api/v1/simulate/goal',{'target':'find flag at 10.71.8.50'},OPS)
 request(API,'/api/v1/simulate/nmap',{'target':'192.168.56.23'},OPS);time.sleep(1)
 check('context gate=false blocks ALL cold paths',count()==before)
 cfg['services']['sensor']['environment']['ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT']='true';CF.write_text(json.dumps(cfg));run(COMPOSE+['up','-d','--no-deps','sensor']);ready()
 run(COMPOSE+['stop','manager'])
 code,body=request(API,'/api/v1/simulate/nmap',{'target':'192.168.56.23'},OPS)
 check('manager-down new command still served',code==200 and '192.168.56.23' in body['output'])
 down_sid=body['session_id']
 run(COMPOSE+['start','manager']);time.sleep(31);before=count()
 request(API,'/api/v1/simulate/nmap',{'target':'192.168.56.23','session_id':down_sid},OPS);time.sleep(1)
 check('manager restart retries SAME command after connectivity backoff',count()>before)
 for role,extra in [('sensro',[]),('sensor',['-e','UNRECOGNIZED_CUSTOM_KEY=SYNTHETIC-DO-NOT-LOG'])]:
  p=run(['docker','run','--rm','--network','none','-e','ALTERHIVE_ROLE='+role,*extra,IMAGE],False)
  check('fail-closed startup '+role,p.returncode!=0 and 'SYNTHETIC-DO-NOT-LOG' not in p.stdout+p.stderr,p.stdout+p.stderr)
 p=run(['docker','run','--rm','--network','none','-e','ALTERHIVE_ROLE=manager','-e','ALTERHIVE_LLM_CONFIG_READ_ONLY=tru',IMAGE],False)
 check('invalid security boolean fails closed',p.returncode!=0 and 'invalid ALTERHIVE_LLM_CONFIG_READ_ONLY' in p.stdout+p.stderr)
 print('ALL SPLIT SECURITY AND FUNCTIONAL CHECKS PASSED',flush=True)
finally:
 p=run(COMPOSE+['logs','--no-color'],False);(WORK/'containers.log').write_text(p.stdout+p.stderr)
 (WORK/'results.json').write_text(json.dumps(results,indent=2))
 run(COMPOSE+['down','--volumes','--remove-orphans'],False)
 print('Evidence:',WORK,flush=True)
