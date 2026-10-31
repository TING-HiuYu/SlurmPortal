import importlib.util
from pathlib import Path
import unittest

spec=importlib.util.spec_from_file_location('renewal',Path(__file__).with_name('renew.py'))
renewal=importlib.util.module_from_spec(spec);spec.loader.exec_module(renewal)

class Commands(unittest.TestCase):
    def configuration(self,runner):
        return {'domain':'login.example.org','cert_name':'existing-lineage','acme_root':'/private/site','runner':runner,'dns_hook':'/private/hooks/cloudflare_dns01.py','dns_config':'/private/site/dns.json','new_dns_token_file':'/private/new-token','allow_no_email':True}

    def test_native_renewal_selects_only_existing_lineage_and_scoped_hooks(self):
        config=self.configuration({'type':'native','binary':'/usr/bin/certbot'})
        config['directories']={'config':'/etc/letsencrypt'}
        args=renewal.certbot_command(config,'renew')
        self.assertEqual('/usr/bin/certbot',args[0]);self.assertEqual('renew',args[1])
        self.assertEqual('existing-lineage',args[args.index('--cert-name')+1])
        self.assertEqual('/etc/letsencrypt',args[args.index('--config-dir')+1])
        self.assertNotIn('-d',args)
        self.assertIn('--manual-auth-hook',args);self.assertIn('--manual-cleanup-hook',args)

    def test_container_does_not_receive_host_docker_socket_or_token_environment(self):
        args=renewal.certbot_command(self.configuration({'type':'docker','image':'certbot/certbot:v5.8.0'}),'issue-only')
        text=' '.join(args)
        self.assertNotIn('docker.sock',text);self.assertNotIn('--privileged',args);self.assertNotIn('--env',args)
        self.assertIn('dst=/run/secrets/new-dns-edit.token,readonly',text)
        self.assertEqual('login.example.org',args[args.index('-d')+1])
        self.assertEqual(1,args.count('-d'))

    def test_check_is_staging_only_and_invalid_domain_or_name_is_rejected(self):
        config=self.configuration({'type':'native','binary':'/usr/bin/certbot'})
        self.assertIn('--dry-run',renewal.certbot_command(config,'check'))
        for field,value in [('domain','*.example.org'),('cert_name','../../different')]:
            changed=dict(config);changed[field]=value
            with self.assertRaises(renewal.deployment.DeploymentFailure):renewal.certbot_command(changed,'renew')

if __name__=='__main__':unittest.main()
