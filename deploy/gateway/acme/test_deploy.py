import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('tls_deploy', Path(__file__).with_name('deploy.py'))
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)

def openssl(*args):
    subprocess.run(['openssl', *map(str, args)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

class DeploymentTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.material = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.material.cleanup)
        cls.base = Path(cls.material.name)
        openssl('req', '-new', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-days', '365', '-subj', '/CN=Offline Fixture CA', '-addext', 'basicConstraints=critical,CA:TRUE', '-addext', 'keyUsage=critical,keyCertSign,cRLSign', '-keyout', cls.base/'ca.key', '-out', cls.base/'ca.pem')
        for name, dns, usage in [('old', 'login.example.org', 'serverAuth'), ('new', 'login.example.org', 'serverAuth'), ('wrong', 'wrong.example.org', 'serverAuth'), ('mtls', 'login.example.org', 'clientAuth')]:
            folder = cls.base/name; folder.mkdir()
            openssl('req', '-new', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes', '-subj', '/CN='+dns, '-keyout', folder/'key.pem', '-out', folder/'request.pem')
            (folder/'extensions').write_text('subjectAltName=DNS:'+dns+'\nextendedKeyUsage='+usage+'\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n')
            openssl('x509', '-req', '-in', folder/'request.pem', '-CA', cls.base/'ca.pem', '-CAkey', cls.base/'ca.key', '-set_serial', str(len(name)*111), '-days', '90', '-extfile', folder/'extensions', '-out', folder/'leaf.pem')
            (folder/'fullchain.pem').write_bytes((folder/'leaf.pem').read_bytes()+(cls.base/'ca.pem').read_bytes())

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(); self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root/'source'; shutil.copytree(self.base/'new', self.source)
        self.target = self.root/'target'; self.target.mkdir(mode=0o700)
        self.state = self.root/'state'; self.state.mkdir(mode=0o700)
        shutil.copyfile(self.base/'old/fullchain.pem', self.target/'fullchain.pem')
        shutil.copyfile(self.base/'old/key.pem', self.target/'key.pem')
        (self.target/'key.pem').chmod(0o600)
        self.config = {'domain':'login.example.org','source_root':str(self.source),'source_certificate':str(self.source/'fullchain.pem'),'source_key':str(self.source/'key.pem'),'target_certificate':str(self.target/'fullchain.pem'),'target_key':str(self.target/'key.pem'),'state_directory':str(self.state),'trust_store':str(self.base/'ca.pem'),'owner_uid':os.geteuid(),'owner_gid':os.getegid(),'endpoints':[{'host':'127.0.0.1','port':19933},{'host':'127.0.0.1','port':19934}]}
        self.config['protected_paths']=[str(self.root/'master-client.crt'),str(self.root/'master-client.key')]
        for name in ('process_identity','nginx_syntax_check','reload_go','reload_nginx'):
            self.config[name] = {'argv':[name]}
        self.calls = []

    def command(self, value):
        self.calls.append(value['argv'][0])
        return b'gateway-pid-123-start-456' if value['argv'][0]=='process_identity' else b''

    def live(self, config, expected):
        self.assertEqual(expected, deployment.certificate_fingerprint(config['target_certificate']))

    def test_crypto_validation_rejects_hostname_key_usage_and_expiry(self):
        for name in ('wrong','mtls'):
            with self.assertRaises(deployment.DeploymentFailure):
                deployment.validate_candidate(self.base/name/'fullchain.pem',self.base/name/'key.pem','login.example.org',self.base/'ca.pem',0)
        with self.assertRaises(deployment.DeploymentFailure):
            deployment.validate_candidate(self.base/'new/fullchain.pem',self.base/'old/key.pem','login.example.org',self.base/'ca.pem',0)
        with self.assertRaises(deployment.DeploymentFailure):
            deployment.validate_candidate(self.base/'new/fullchain.pem',self.base/'new/key.pem','login.example.org',self.base/'ca.pem',100*86400)

    def test_atomic_ordinary_copies_reload_without_process_change(self):
        result = deployment.deploy(self.config, self.command, self.live)
        self.assertTrue(result['changed'] and result['process_preserved'])
        self.assertFalse((self.target/'fullchain.pem').is_symlink() or (self.target/'key.pem').is_symlink())
        self.assertEqual(0o600, (self.target/'key.pem').stat().st_mode&0o777)
        self.assertEqual(['process_identity','nginx_syntax_check','reload_go','reload_nginx','process_identity'], self.calls)
        self.calls.clear()
        self.assertFalse(deployment.deploy(self.config,self.command,self.live)['changed'])
        self.assertNotIn('reload_go',self.calls)

    def test_configured_public_port_7000_reloads_both_local_tls_endpoints(self):
        self.config['public_port'] = 7000
        self.config['endpoints'] = [{'host':'::1','port':7000},{'host':'127.0.0.1','port':19934}]
        checked = []
        def live(config, expected):
            checked.append(deployment.local_tls_endpoints(config))
            self.live(config, expected)
        result = deployment.deploy(self.config, self.command, live)
        self.assertTrue(result['changed'] and result['process_preserved'])
        self.assertEqual([self.config['endpoints']], checked)
        self.assertEqual(['process_identity','nginx_syntax_check','reload_go','reload_nginx','process_identity'], self.calls)
        self.assertEqual(deployment.certificate_fingerprint(self.source/'fullchain.pem'), result['leaf_sha256'])

    def assert_rejected_before_mutation(self, error):
        old_cert = (self.target/'fullchain.pem').read_bytes()
        old_key = (self.target/'key.pem').read_bytes()
        with self.assertRaisesRegex(deployment.DeploymentFailure, error):
            deployment.deploy(self.config, self.command, self.live)
        self.assertEqual(old_cert, (self.target/'fullchain.pem').read_bytes())
        self.assertEqual(old_key, (self.target/'key.pem').read_bytes())
        self.assertEqual([], self.calls)
        self.assertEqual([], list(self.state.iterdir()))

    def test_invalid_or_internal_public_ports_preserve_old_pair(self):
        for value in (None, True, False, '7000', 7000.0, [], {}, 1023, 65536, -1, 19934, 19935):
            with self.subTest(public_port=value):
                self.config['public_port'] = value
                self.assert_rejected_before_mutation('configured_public_tls_port_required')

    def test_port_boundaries_and_omitted_default_are_supported(self):
        self.assertNotIn('public_port', self.config)
        self.assertEqual({19933, 19934}, {item['port'] for item in deployment.local_tls_endpoints(self.config)})
        for value in (1024, 65535):
            with self.subTest(public_port=value):
                self.config['public_port'] = value
                self.config['endpoints'] = [{'host':'127.0.0.1','port':value},{'host':'::1','port':19934}]
                self.assertEqual(self.config['endpoints'], deployment.local_tls_endpoints(self.config))

    def test_wrong_endpoint_ports_counts_types_or_remote_hosts_preserve_old_pair(self):
        self.config['public_port'] = 7000
        local = {'host':'127.0.0.1','port':19934}
        cases = (
            None, [], [local], [local, local, {'host':'::1','port':7000}],
            [local, local], [local, {'host':'::1','port':19933}],
            [local, {'host':'::1','port':19935}], [local, {'host':'::1','port':'7000'}],
            [local, {'host':'::1','port':7000.0}], [local, {'host':'::1','port':[]}],
            [local, None], [local, []],
            [local, {'host':'203.0.113.7','port':7000}],
            [local, {'host':'login.example.org','port':7000}],
            [local, {'host':'127.0.0.2','port':7000}],
            [local, {'host':'::ffff:127.0.0.1','port':7000}],
        )
        for endpoints in cases:
            with self.subTest(endpoints=endpoints):
                self.config['endpoints'] = endpoints
                self.assert_rejected_before_mutation('both_local_tls_endpoints_required')

    def test_live_verification_rejects_remote_hosts_before_connecting(self):
        self.config['public_port'] = 7000
        self.config['endpoints'] = [{'host':'remote.example.org','port':7000},{'host':'127.0.0.1','port':19934}]
        with patch.object(deployment, 'probe_certificate') as probe:
            with self.assertRaisesRegex(deployment.DeploymentFailure, 'both_local_tls_endpoints_required'):
                deployment.verify_live(self.config, 'untrusted-fingerprint')
            probe.assert_not_called()

    def test_symlink_target_and_bad_pair_never_mutate(self):
        original = (self.target/'key.pem').read_bytes()
        (self.target/'key.pem').unlink(); (self.target/'key.pem').symlink_to(self.base/'old/key.pem')
        with self.assertRaises(deployment.DeploymentFailure):
            deployment.deploy(self.config,self.command,self.live)
        self.assertEqual(original,(self.base/'old/key.pem').read_bytes())
        self.assertEqual([],self.calls)

    def test_main_mtls_targets_are_never_overwritten(self):
        self.config['protected_paths'][0]=str(self.target/'key.pem')
        original=(self.target/'key.pem').read_bytes()
        with self.assertRaisesRegex(deployment.DeploymentFailure,'main_mtls_material_is_protected'):
            deployment.deploy(self.config,self.command,self.live)
        self.assertEqual(original,(self.target/'key.pem').read_bytes())
        self.assertEqual([],self.calls)

    def test_failed_activation_restores_old_pair_without_restart(self):
        old_cert,old_key=(self.target/'fullchain.pem').read_bytes(),(self.target/'key.pem').read_bytes()
        old_fp=deployment.certificate_fingerprint(self.target/'fullchain.pem')
        def check(config,expected):
            if expected!=old_fp:raise deployment.DeploymentFailure('fixture_effective_tls_mismatch')
            self.live(config,expected)
        with self.assertRaisesRegex(deployment.DeploymentFailure,'old_files_restored'):
            deployment.deploy(self.config,self.command,check)
        self.assertEqual(old_cert,(self.target/'fullchain.pem').read_bytes())
        self.assertEqual(old_key,(self.target/'key.pem').read_bytes())
        self.assertEqual(2,self.calls.count('reload_go'))
        self.assertEqual(2,self.calls.count('reload_nginx'))

    def test_real_tls_probe_checks_sni_chain_and_leaf_fingerprint(self):
        listener=socket.socket();listener.bind(('127.0.0.1',0));listener.listen(2)
        self.addCleanup(listener.close)
        context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);context.load_cert_chain(self.base/'new/fullchain.pem',self.base/'new/key.pem')
        error=[]
        def server():
            try:
                connection,_=listener.accept()
                with context.wrap_socket(connection,server_side=True):pass
            except Exception as value:error.append(value)
        thread=threading.Thread(target=server);thread.start()
        result=deployment.probe_certificate({'host':'127.0.0.1','port':listener.getsockname()[1]},'login.example.org',self.base/'ca.pem')
        thread.join(5)
        self.assertFalse(thread.is_alive());self.assertEqual([],error)
        self.assertEqual(deployment.certificate_fingerprint(self.base/'new/fullchain.pem'),result)

if __name__=='__main__':unittest.main()
