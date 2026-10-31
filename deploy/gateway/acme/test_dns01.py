import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('hook', Path(__file__).with_name('cloudflare_dns01.py'))
hook = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hook)

class API:
    def __init__(self):
        self.foreign = 'b' * 32
        self.records = {self.foreign: {'id': self.foreign, 'type': 'TXT', 'name': '_acme-challenge.existing-service.example.org', 'content': 'existing-unrelated-challenge'}}
        self.calls = []
        self.unknown_post = False

    def request(self, method, suffix, body=None):
        self.calls.append((method, suffix))
        if method == 'POST':
            value = dict(body, id='a' * 32)
            self.records[value['id']] = value
            if self.unknown_post:
                raise hook.HookFailure('cloudflare_outcome_unknown')
            return value
        if method == 'GET':
            return self.records.get(suffix[1:])
        if method == 'DELETE':
            return self.records.pop(suffix[1:])
        raise AssertionError('unexpected API method')

class HookTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.state = Path(self.temporary.name)
        self.state.chmod(0o700)
        self.config = {'domain': 'login.example.org', 'zone_id': 'e' * 32, 'state_directory': str(self.state), 'propagation_seconds': 60}
        self.validation = 'x' * 43
        self.api = API()

    def perform(self, phase, domain='login.example.org'):
        return hook.perform(phase, domain, self.validation, self.config, self.api, sleep=lambda _: None)

    def test_only_created_record_is_removed(self):
        created = self.perform('auth')['record_id']
        self.perform('auth')  # Confirm the same owned record rather than create twice.
        self.assertEqual(1, sum(method == 'POST' for method, _ in self.api.calls))
        self.perform('cleanup')
        self.assertIn(self.api.foreign, self.api.records)
        self.assertNotIn(created, self.api.records)
        self.assertEqual([('DELETE', '/' + created)], [item for item in self.api.calls if item[0] == 'DELETE'])

    def test_wrong_domain_or_zone_has_no_api_effect(self):
        with self.assertRaises(hook.HookFailure):
            self.perform('auth', 'existing-service.example.org')
        self.config['zone_id'] = 'not-a-zone'
        with self.assertRaises(hook.HookFailure):
            self.perform('auth')
        self.assertEqual([], self.api.calls)

    def test_cluster_a_is_separate_and_cannot_use_cluster_b_config(self):
        with self.assertRaises(hook.HookFailure):
            self.perform('auth', 'other-login.example.org')
        self.config['domain'] = 'other-login.example.org'
        self.perform('auth', 'other-login.example.org')
        self.assertEqual('_acme-challenge.other-login.example.org', self.api.records['a' * 32]['name'])
        self.perform('cleanup', 'other-login.example.org')
        self.assertIn(self.api.foreign, self.api.records)

    def test_unknown_post_is_not_repeated_or_blindly_cleaned(self):
        self.api.unknown_post = True
        with self.assertRaises(hook.HookFailure):
            self.perform('auth')
        with self.assertRaises(hook.HookFailure):
            self.perform('auth')
        with self.assertRaises(hook.HookFailure):
            self.perform('cleanup')
        self.assertEqual(1, sum(method == 'POST' for method, _ in self.api.calls))
        self.assertFalse(any(method == 'DELETE' for method, _ in self.api.calls))
        self.assertIn(self.api.foreign, self.api.records)

    def test_changed_txt_content_cannot_be_deleted(self):
        created = self.perform('auth')['record_id']
        self.api.records[created]['content'] = 'different-owner-content'
        with self.assertRaises(hook.HookFailure):
            self.perform('cleanup')
        self.assertFalse(any(method == 'DELETE' for method, _ in self.api.calls))

    def test_private_token_permissions_are_required(self):
        token = self.state / 'fake-token'
        token.write_text('synthetic-token-not-a-real-credential')
        token.chmod(0o644)
        with self.assertRaises(hook.HookFailure):
            hook.private_file(token)
        token.chmod(0o600)
        self.assertTrue(hook.private_file(token).startswith('synthetic-'))

if __name__ == '__main__':
    unittest.main()
