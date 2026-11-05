#!/usr/bin/env python3
"""Create missing split configuration without overwriting operator files."""
from pathlib import Path
import os, secrets, shutil
root=Path(__file__).resolve().parent.parent
base=root/'deploy/manager-sensor'
for d in [base/'manager-configs',base/'sensor-configs/services']:d.mkdir(parents=True,exist_ok=True)
def private(path,content):
    try: fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    except FileExistsError:return
    with os.fdopen(fd,'w') as f:f.write(content)
private(base/'.env','ALTERHIVE_ADMIN_TOKEN='+secrets.token_urlsafe(40)+'\nALTERHIVE_SENSOR_ADMIN_TOKEN='+secrets.token_urlsafe(40)+'\n')
private(base/'manager.env','# Provider credentials for manager only.\n# ALTERHIVE_OPENAI_API_KEY=...\n')
for src,dst in [(base/'llm.yaml.example',base/'manager-configs/llm.yaml'),(root/'configs/topology.yaml',base/'sensor-configs/topology.yaml'),(root/'configs/services/ssh.yaml',base/'sensor-configs/services/ssh.yaml')]:
    if not dst.exists():shutil.copyfile(src,dst);dst.chmod(0o644)
print('Created missing configuration. Edit manager.env and manager-configs/llm.yaml; no existing files overwritten.')
print('docker compose --env-file deploy/manager-sensor/.env up -d')
