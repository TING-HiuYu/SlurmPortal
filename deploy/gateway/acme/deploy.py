#!/usr/bin/env python3
"""Validated public-TLS copies and hot reload. Never handles master mTLS material."""
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import shutil
import re
import socket
import ssl
import stat
import subprocess
import sys
import time
import uuid

def configured_domain(value):
    if not isinstance(value, str) or len(value) > 253 or not re.fullmatch(r'(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', value):
        return False
    try:
        ipaddress.ip_address(value)
        return False
    except ValueError:
        return True

class DeploymentFailure(Exception):
    pass

def checked_run(arguments, data=b''):
    try:
        result = subprocess.run(arguments, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20)
    except (OSError, subprocess.TimeoutExpired):
        raise DeploymentFailure('command_unavailable') from None
    if result.returncode or len(result.stdout) > 2 << 20:
        raise DeploymentFailure('command_rejected')
    return result.stdout

def certificate_fingerprint(path):
    return hashlib.sha256(checked_run(['openssl', 'x509', '-in', str(path), '-outform', 'DER'])).hexdigest()

def validate_candidate(certificate, key, domain, trust_store, minimum_remaining_seconds):
    if not configured_domain(domain) or not isinstance(minimum_remaining_seconds, int) or minimum_remaining_seconds < 0:
        raise DeploymentFailure('configured_gateway_domain_required')
    cert_public = checked_run(['openssl', 'x509', '-in', str(certificate), '-pubkey', '-noout'])
    cert_spki = checked_run(['openssl', 'pkey', '-pubin', '-outform', 'DER'], cert_public)
    key_spki = checked_run(['openssl', 'pkey', '-in', str(key), '-pubout', '-outform', 'DER'])
    if cert_spki != key_spki:
        raise DeploymentFailure('certificate_key_mismatch')
    # verify_hostname participates in verification exit status; x509 -checkhost
    # alone is deliberately not used as an exit-status hostname validator.
    checked_run(['openssl', 'verify', '-purpose', 'sslserver', '-verify_hostname', domain, '-CAfile', str(trust_store), '-untrusted', str(certificate), str(certificate)])
    checked_run(['openssl', 'x509', '-in', str(certificate), '-noout', '-checkend', str(minimum_remaining_seconds)])
    return {'domain': domain, 'leaf_sha256': certificate_fingerprint(certificate), 'public_key_sha256': hashlib.sha256(cert_spki).hexdigest()}

def ordinary_target(path):
    path = Path(path)
    parent = path.parent.lstat()
    if not stat.S_ISDIR(parent.st_mode) or parent.st_mode & 0o022:
        raise DeploymentFailure('secure_ordinary_target_directory_required')
    if path.exists() or path.is_symlink():
        value = path.lstat()
        if not stat.S_ISREG(value.st_mode) or value.st_mode & 0o022:
            raise DeploymentFailure('ordinary_target_file_required')
    return path

def atomic_copy(source, target, mode, uid, gid):
    target = ordinary_target(target)
    temporary = target.with_name('.' + target.name + '.' + uuid.uuid4().hex + '.tmp')
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    try:
        with os.fdopen(descriptor, 'wb') as stream, open(source, 'rb') as material:
            shutil.copyfileobj(material, stream, 65536)
            stream.flush()
            os.fchmod(stream.fileno(), mode)
            os.fchown(stream.fileno(), uid, gid)
            os.fsync(stream.fileno())
        os.replace(temporary, target)
        directory = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if temporary.exists():
            temporary.unlink()

def run_command(command):
    if not isinstance(command, dict) or not isinstance(command.get('argv'), list) or not command['argv'] or any(not isinstance(value, str) or '\0' in value for value in command['argv']):
        raise DeploymentFailure('trusted_command_argv_required')
    data = Path(command['stdin_file']).read_bytes() if command.get('stdin_file') else b''
    return checked_run(command['argv'], data)

def probe_certificate(endpoint, domain, trust_store):
    context = ssl.create_default_context(cafile=str(trust_store))
    with socket.create_connection((endpoint['host'], endpoint['port']), timeout=3) as connection:
        with context.wrap_socket(connection, server_hostname=domain) as tls:
            return hashlib.sha256(tls.getpeercert(binary_form=True)).hexdigest()

def local_tls_endpoints(config):
    public_port = config.get('public_port', 19933)
    if type(public_port) is not int or not 1024 <= public_port <= 65535 or public_port in (19934, 19935):
        raise DeploymentFailure('configured_public_tls_port_required')
    endpoints = config.get('endpoints')
    if (not isinstance(endpoints, list) or len(endpoints) != 2 or
            any(not isinstance(item, dict) or type(item.get('port')) is not int or
                item.get('host') not in ('127.0.0.1', '::1') for item in endpoints) or
            {item['port'] for item in endpoints} != {public_port, 19934}):
        raise DeploymentFailure('both_local_tls_endpoints_required')
    return endpoints

def verify_live(config, fingerprint):
    deadline = time.monotonic() + config.get('verification_timeout_seconds', 20)
    pending = list(local_tls_endpoints(config))
    while pending:
        remaining = []
        for endpoint in pending:
            try:
                if probe_certificate(endpoint, config['domain'], config['trust_store']) != fingerprint:
                    remaining.append(endpoint)
            except (OSError, ssl.SSLError, ValueError):
                remaining.append(endpoint)
        if not remaining:
            return
        if time.monotonic() >= deadline:
            raise DeploymentFailure('effective_tls_certificate_not_updated')
        pending = remaining
        time.sleep(0.5)

def deploy(config, command=run_command, live=verify_live):
    domain = config['domain']
    if not configured_domain(domain):
        raise DeploymentFailure('configured_gateway_domain_required')
    local_tls_endpoints(config)
    source_root = Path(config['source_root']).resolve(strict=True)
    cert, key = Path(config['source_certificate']).resolve(strict=True), Path(config['source_key']).resolve(strict=True)
    if not cert.is_relative_to(source_root) or not key.is_relative_to(source_root) or not cert.is_file() or not key.is_file() or cert.stat().st_size > 1 << 20 or key.stat().st_size > 65536:
        raise DeploymentFailure('isolated_acme_source_required')
    state = Path(config['state_directory'])
    state_info = state.lstat()
    if not stat.S_ISDIR(state_info.st_mode) or state_info.st_uid != os.geteuid() or state_info.st_mode & 0o077:
        raise DeploymentFailure('private_deployment_state_required')
    target_cert, target_key = ordinary_target(config['target_certificate']), ordinary_target(config['target_key'])
    protected = config.get('protected_paths')
    if not isinstance(protected, list) or len(protected) < 2 or any(not isinstance(value, str) or not Path(value).is_absolute() for value in protected):
        raise DeploymentFailure('explicit_main_mtls_protected_paths_required')
    if any(target.resolve() == Path(value).resolve() for target in (target_cert, target_key) for value in protected):
        raise DeploymentFailure('main_mtls_material_is_protected')
    if target_cert == target_key or target_cert.resolve() == cert or target_key.resolve() == key:
        raise DeploymentFailure('independent_ordinary_copies_required')
    if not all(isinstance(config.get(name), int) and config[name] >= 0 for name in ('owner_uid', 'owner_gid')):
        raise DeploymentFailure('explicit_runtime_file_ownership_required')
    with open(state / 'renewal.lock', 'a+b') as lock:
        os.chmod(lock.name, 0o600)
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        candidate = validate_candidate(cert, key, domain, config['trust_store'], config.get('minimum_remaining_seconds', 604800))
        before_identity = command(config['process_identity'])
        if not before_identity.strip() or before_identity.strip() == b'0':
            raise DeploymentFailure('gateway_process_identity_required')
        command(config['nginx_syntax_check'])
        if target_cert.is_file() and target_key.is_file() and cert.read_bytes() == target_cert.read_bytes() and key.read_bytes() == target_key.read_bytes():
            live(config, candidate['leaf_sha256'])
            return dict(candidate, changed=False, process_preserved=True)
        backup = state / ('backup-' + time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()) + '-' + uuid.uuid4().hex[:8])
        backup.mkdir(mode=0o700)
        old = {}
        for name, target in [('certificate', target_cert), ('key', target_key)]:
            if target.is_file():
                destination = backup / name
                shutil.copyfile(target, destination)
                destination.chmod(0o600)
                old[name] = {'path': destination, 'uid': target.stat().st_uid, 'gid': target.stat().st_gid, 'mode': stat.S_IMODE(target.stat().st_mode)}
        old_fingerprint = certificate_fingerprint(target_cert) if target_cert.is_file() else None
        try:
            atomic_copy(cert, target_cert, 0o644, config['owner_uid'], config['owner_gid'])
            atomic_copy(key, target_key, 0o600, config['owner_uid'], config['owner_gid'])
            # Readers switch only once both ordinary files are fully installed.
            command(config['reload_go'])
            command(config['reload_nginx'])
            live(config, candidate['leaf_sha256'])
            if command(config['process_identity']) != before_identity:
                raise DeploymentFailure('gateway_process_restarted')
        except Exception as error:
            rollback_ok = False
            try:
                for name, target in [('certificate', target_cert), ('key', target_key)]:
                    if name in old:
                        prior = old[name]
                        atomic_copy(prior['path'], target, prior['mode'], prior['uid'], prior['gid'])
                    elif target.is_file():
                        target.unlink()
                if old_fingerprint:
                    command(config['reload_go'])
                    command(config['reload_nginx'])
                    live(config, old_fingerprint)
                    rollback_ok = command(config['process_identity']) == before_identity
            except Exception:
                pass
            raise DeploymentFailure('renewal_activation_failed_old_files_restored' if rollback_ok else 'renewal_activation_failed_manual_inspection_required') from error
        result = dict(candidate, changed=True, process_preserved=True, ordinary_files=True)
        temporary = state / 'last-success.tmp'
        temporary.write_text(json.dumps(result) + '\n')
        temporary.chmod(0o600)
        os.replace(temporary, state / 'last-success.json')
        return result

def main():
    os.umask(0o077)
    if os.geteuid() != 0 or len(sys.argv) != 2:
        raise DeploymentFailure('root_and_private_site_config_required')
    path = Path(sys.argv[1]); info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
        raise DeploymentFailure('private_root_site_config_required')
    print(json.dumps(deploy(json.loads(path.read_text()))))

if __name__ == '__main__':
    try:
        main()
    except (DeploymentFailure, OSError, ValueError, KeyError) as error:
        print(json.dumps({'success': False, 'stage': str(error) if isinstance(error, DeploymentFailure) else 'tls_renewal_configuration_unavailable'}), file=sys.stderr)
        sys.exit(1)
