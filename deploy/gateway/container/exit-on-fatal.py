#!/usr/bin/python3
"""Let Docker restart the unit if Supervisor exhausts child startup retries."""
import os
import signal
import sys


def main(stream=sys.stdin, output=sys.stdout):
    output.write("READY\n")
    output.flush()
    header = stream.readline()
    if not header:
        return
    fields = dict(part.split(":", 1) for part in header.split())
    length = int(fields["len"])
    if length < 0 or length > 65536 or len(stream.read(length)) != length:
        raise ValueError("invalid supervisor event")
    output.write("RESULT 2\nOK")
    output.flush()
    if fields.get("eventname") == "PROCESS_STATE_FATAL":
        os.kill(os.getppid(), signal.SIGTERM)


if __name__ == "__main__":
    main()
