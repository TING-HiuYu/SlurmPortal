#!/usr/bin/env python3
"""Certbot hook: one configured hostname and zone, owned TXT IDs only."""
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import stat
import sys
import time
import urllib.error
import urllib.request

CONFIG = '/run/config/dns-hook.json'

class HookFailure(Exception):
    pass

def configured_domain(value):
    if not isinstance(value, str) or len(value) > 253 or not re.fullmatch(r'(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', value):
        return False
    try:
        ipaddress.ip_address(value)
        return False
    except ValueError:
        return True

def private_file(path):
    path = Path(path)
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077 or info.st_size > 16384:
        raise HookFailure('private_regular_file_required')
    return path.read_text()

class Cloudflare:
    def __init__(self, token, zone):
        self.token = token
        self.zone = zone

    def request(self, method, suffix, body=None):
        url = 'https://api.cloudflare.com/client/v4/zones/' + self.zone + '/dns_records' + suffix
        headers = {'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/json'}
        data = None if body is None else json.dumps(body).encode()
        try:
            with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers, method=method), timeout=30) as response:
                value = json.load(response)
        except urllib.error.HTTPError as error:
            if method == 'GET' and error.code == 404:
                return None
            raise HookFailure('cloudflare_http_' + str(error.code)) from None
        except (OSError, ValueError):
            raise HookFailure('cloudflare_outcome_unknown') from None
        if value.get('success') is not True:
            raise HookFailure('cloudflare_api_rejected')
        return value.get('result')

def checked_record(record, validation, domain, record_id=None):
    if not isinstance(record, dict) or record.get('type') != 'TXT' or record.get('name', '').rstrip('.').lower() != '_acme-challenge.' + domain or record.get('content') != validation or not re.fullmatch('[a-f0-9]{32}', record.get('id', '')):
        raise HookFailure('owned_txt_record_mismatch')
    if record_id is not None and record['id'] != record_id:
        raise HookFailure('owned_txt_identity_mismatch')
    return record['id']

def save_journal(path, value):
    temporary = path.with_suffix('.tmp')
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, 'w') as stream:
            json.dump(value, stream)
            stream.write('\n')
        os.replace(temporary, path)
    finally:
        if temporary.exists():
            temporary.unlink()

def perform(phase, domain, validation, config, api, sleep=time.sleep):
    if phase not in ('auth', 'cleanup') or not configured_domain(domain) or domain != config.get('domain') or not re.fullmatch('[A-Za-z0-9_-]{20,256}', validation or ''):
        raise HookFailure('configured_single_gateway_challenge_required')
    if not isinstance(config.get('zone_id'), str) or not re.fullmatch('[a-f0-9]{32}', config['zone_id']):
        raise HookFailure('exact_zone_required')
    delay = config.get('propagation_seconds', 60)
    if not isinstance(delay, int) or not 30 <= delay <= 120:
        raise HookFailure('invalid_propagation_delay')
    state = Path(config['state_directory'])
    info = state.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise HookFailure('private_state_directory_required')
    key = hashlib.sha256((domain + '\0' + validation).encode()).hexdigest()
    journal = state / (key + '.json')
    saved = json.loads(private_file(journal)) if journal.exists() else None
    if saved and (saved.get('domain') != domain or saved.get('validation') != validation):
        raise HookFailure('challenge_journal_mismatch')
    if phase == 'auth':
        if saved:
            if saved.get('phase') != 'created' or not saved.get('record_id'):
                raise HookFailure('prior_write_requires_inspection')
            checked_record(api.request('GET', '/' + saved['record_id']), validation, domain, saved['record_id'])
        else:
            saved = {'domain': domain, 'validation': validation, 'phase': 'create_attempted'}
            save_journal(journal, saved)
            record = api.request('POST', '', {'type': 'TXT', 'name': '_acme-challenge.' + domain, 'content': validation, 'ttl': 60})
            saved.update(record_id=checked_record(record, validation, domain), phase='created')
            save_journal(journal, saved)
        # Each sleep is bounded so the invoking operator can retain progress.
        while delay:
            slot = min(delay, 20)
            sleep(slot)
            delay -= slot
        return {'stage': 'owned_txt_ready', 'record_id': saved['record_id']}
    if saved is None or saved.get('phase') == 'cleaned':
        return {'stage': 'no_owned_txt_to_clean'}
    if not saved.get('record_id') or saved.get('phase') not in ('created', 'delete_attempted'):
        raise HookFailure('unconfirmed_create_requires_inspection')
    record = api.request('GET', '/' + saved['record_id'])
    if record is not None:
        checked_record(record, validation, domain, saved['record_id'])
        saved['phase'] = 'delete_attempted'
        save_journal(journal, saved)
        api.request('DELETE', '/' + saved['record_id'])
    saved['phase'] = 'cleaned'
    save_journal(journal, saved)
    return {'stage': 'owned_txt_cleaned', 'record_id': saved['record_id']}

def main():
    os.umask(0o077)
    if len(sys.argv) not in (2, 3):
        raise HookFailure('exact_hook_phase_required')
    config = json.loads(private_file(sys.argv[2] if len(sys.argv) == 3 else CONFIG))
    token = private_file(config['token_file']).strip()
    if not 20 <= len(token) <= 4096 or any(character.isspace() for character in token):
        raise HookFailure('new_scoped_dns_token_required')
    result = perform(sys.argv[1], os.environ.get('CERTBOT_DOMAIN'), os.environ.get('CERTBOT_VALIDATION'), config, Cloudflare(token, config['zone_id']))
    print(json.dumps(result), file=sys.stderr)

if __name__ == '__main__':
    try:
        main()
    except (HookFailure, OSError, ValueError, KeyError) as error:
        stage = str(error) if isinstance(error, HookFailure) else 'private_hook_configuration_unavailable'
        print(json.dumps({'success': False, 'stage': stage}), file=sys.stderr)
        sys.exit(1)
