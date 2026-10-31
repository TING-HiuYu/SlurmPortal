#!/usr/bin/env python3
"""Host-side scheduler entry. Certbot never receives the host Docker socket."""
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import stat
import subprocess
import sys

spec = importlib.util.spec_from_file_location('tls_deployment', Path(__file__).with_name('deploy.py'))
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)

def private_root_file(path):
    info = Path(path).lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
        raise deployment.DeploymentFailure('private_root_configuration_required')

def certbot_command(config, action):
    domain = config['domain']
    if not deployment.configured_domain(domain) or action not in ('issue','issue-only','renew','check'):
        raise deployment.DeploymentFailure('single_gateway_certbot_action_required')
    cert_name=config.get('cert_name',domain)
    if not isinstance(cert_name,str) or not re.fullmatch(r'[A-Za-z0-9_.-]{1,128}',cert_name):
        raise deployment.DeploymentFailure('single_certificate_name_required')
    root = Path(config['acme_root'])
    if not root.is_absolute():
        raise deployment.DeploymentFailure('absolute_isolated_acme_root_required')
    docker = config['runner']['type']=='docker'
    if config['runner']['type'] not in ('native','docker'):
        raise deployment.DeploymentFailure('supported_certbot_runner_required')
    folders={name:Path(config.get('directories',{}).get(name,str(root/default))) for name,default in [('config','config'),('work','work'),('logs','logs'),('challenge_state','challenge-state')]}
    if any(not value.is_absolute() for value in folders.values()):
        raise deployment.DeploymentFailure('absolute_certbot_directories_required')
    if docker:
        auth = 'python3 /hooks/cloudflare_dns01.py auth'
        cleanup = 'python3 /hooks/cloudflare_dns01.py cleanup'
        arguments = ['docker','run','--rm','--read-only','--cap-drop=ALL','--security-opt','no-new-privileges','--user','0:0','--tmpfs','/tmp:rw,nosuid,nodev']
        for host,target,readonly in [(folders['config'],'/etc/letsencrypt',False),(folders['work'],'/var/lib/letsencrypt',False),(folders['logs'],'/var/log/letsencrypt',False),(folders['challenge_state'],'/state',False),(Path(config['dns_hook']),'/hooks/cloudflare_dns01.py',True),(Path(config['dns_config']),'/run/config/dns-hook.json',True),(Path(config['new_dns_token_file']),'/run/secrets/new-dns-edit.token',True)]:
            arguments+=['--mount','type=bind,src='+str(host)+',dst='+target+(',readonly' if readonly else '')]
        arguments+=[config['runner']['image']]
        location=[]
    else:
        arguments=[config['runner']['binary']]
        auth=shlex.join(['python3',config['dns_hook'],'auth',config['dns_config']])
        cleanup=shlex.join(['python3',config['dns_hook'],'cleanup',config['dns_config']])
        location=['--config-dir',str(folders['config']),'--work-dir',str(folders['work']),'--logs-dir',str(folders['logs'])]
    common=['--non-interactive','--manual-auth-hook',auth,'--manual-cleanup-hook',cleanup,'--cert-name',cert_name]
    if action in ('renew','check'):
        return arguments+['renew','--no-random-sleep-on-renew']+(['--dry-run'] if action=='check' else [])+common+location
    contact=['--email',config['contact_email']] if config.get('contact_email') else ['--register-unsafely-without-email'] if config.get('allow_no_email') is True else None
    if contact is None:
        raise deployment.DeploymentFailure('explicit_acme_contact_choice_required')
    return arguments+['certonly','--manual','--preferred-challenges','dns','--key-type','ecdsa','--agree-tos','-d',domain]+common+location+contact

def main():
    os.umask(0o077)
    if os.geteuid()!=0 or len(sys.argv)!=3:
        raise deployment.DeploymentFailure('root_site_config_and_action_required')
    private_root_file(sys.argv[1]);config=json.loads(Path(sys.argv[1]).read_text())
    if config.get('configuration_complete') is not True or config.get('token_source_confirmed_new') is not True:
        raise deployment.DeploymentFailure('new_scoped_token_and_completed_configuration_required')
    for name in ('dns_config','new_dns_token_file','deployment_config'):
        private_root_file(config[name])
    hook=json.loads(Path(config['dns_config']).read_text())
    if hook.get('domain')!=config['domain']:
        raise deployment.DeploymentFailure('site_dns_domain_mismatch')
    root=Path(config['acme_root']);root.mkdir(mode=0o700,parents=True,exist_ok=True)
    if root.stat().st_uid!=0 or root.stat().st_mode&0o077:
        raise deployment.DeploymentFailure('private_acme_root_required')
    for name,default in [('config','config'),('work','work'),('logs','logs'),('challenge_state','challenge-state')]:
        folder=Path(config.get('directories',{}).get(name,str(root/default)));folder.mkdir(mode=0o700,parents=True,exist_ok=True)
        if not folder.is_dir() or folder.is_symlink() or folder.stat().st_uid!=0 or folder.stat().st_mode&0o022:
            raise deployment.DeploymentFailure('ordinary_acme_directory_required')
    if config['runner']['type']=='docker':
        pinned=config['runner'].get('image_id','')
        if not pinned.startswith('sha256:') or deployment.checked_run(['docker','image','inspect',config['runner']['image'],'--format','{{.Id}}']).decode().strip()!=pinned:
            raise deployment.DeploymentFailure('pinned_local_certbot_image_required')
    result=subprocess.run(certbot_command(config,sys.argv[2]),timeout=600)
    if result.returncode:
        raise deployment.DeploymentFailure('certbot_failed_no_certificate_deployment')
    if sys.argv[2]=='check':
        print(json.dumps({'domain':config['domain'],'acme_staging_check_passed':True,'certificate_deployed':False}))
        return
    candidate=json.loads(Path(config['deployment_config']).read_text())
    if candidate.get('domain')!=config['domain']:
        raise deployment.DeploymentFailure('deployment_site_domain_mismatch')
    if sys.argv[2]=='issue-only':
        metadata=deployment.validate_candidate(candidate['source_certificate'],candidate['source_key'],candidate['domain'],candidate['trust_store'],candidate.get('minimum_remaining_seconds',604800))
        print(json.dumps(dict(metadata,issued=True,certificate_deployed=False)))
    else:
        print(json.dumps(deployment.deploy(candidate)))

if __name__=='__main__':
    try:main()
    except (deployment.DeploymentFailure,OSError,ValueError,KeyError,subprocess.TimeoutExpired) as error:
        print(json.dumps({'success':False,'stage':str(error) if isinstance(error,deployment.DeploymentFailure) else 'tls_renewal_configuration_unavailable'}),file=sys.stderr)
        sys.exit(1)
