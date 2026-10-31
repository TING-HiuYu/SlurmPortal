#!/usr/bin/env python3
"""Run inside the HK gateway container; never signal PID1 or restart a process."""
import json
import os
from pathlib import Path
import signal
import sys

EXPECTED = '/usr/local/libexec/go-ldap-admin/web-access-gateway'

def identity():
    found = []
    for item in Path('/proc').iterdir():
        if not item.name.isdecimal() or int(item.name) <= 1:
            continue
        try:
            if os.readlink(item / 'exe') == EXPECTED:
                fields = (item / 'stat').read_text().rsplit(')', 1)[1].split()
                found.append((int(item.name), fields[19]))
        except (FileNotFoundError, PermissionError, ProcessLookupError):
            pass
    if len(found) != 1:
        raise RuntimeError('unique_gateway_process_required')
    return found[0]

def main():
    if len(sys.argv) != 2 or sys.argv[1] not in ('identify', 'hup'):
        raise RuntimeError('exact_process_action_required')
    before = identity()
    if sys.argv[1] == 'hup':
        if not hasattr(os, 'pidfd_open') or not hasattr(signal, 'pidfd_send_signal'):
            raise RuntimeError('pidfd_support_required')
        descriptor = os.pidfd_open(before[0])
        try:
            if identity() != before:
                raise RuntimeError('gateway_identity_changed')
            signal.pidfd_send_signal(descriptor, signal.SIGHUP)
        finally:
            os.close(descriptor)
    print(json.dumps({'pid': before[0], 'start_ticks': before[1]}))

if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, OSError, ValueError) as error:
        print(json.dumps({'success': False, 'stage': str(error) if isinstance(error, RuntimeError) else 'gateway_process_unavailable'}), file=sys.stderr)
        sys.exit(1)
