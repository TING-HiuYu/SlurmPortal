#!/usr/bin/env python3
"""Prepare a minimal public image context or a non-root Nginx configuration.

This utility does not connect to Docker, copy secrets or install a service.
"""
import argparse
import hashlib
from pathlib import Path
import re
import shutil
import struct

HERE = Path(__file__).resolve().parent


def build_context(binary, output):
    binary, output = Path(binary).resolve(), Path(output).absolute()
    with binary.open("rb") as stream:
        header = stream.read(20)
    if len(header) != 20 or header[:6] != b"\x7fELF\x02\x01" or struct.unpack("<H", header[18:20])[0] != 62:
        raise ValueError("expected a prebuilt Linux amd64 ELF64 binary")
    if output.exists():
        raise ValueError("use a new build-context directory; existing contents are never reused")
    output.mkdir(parents=True, mode=0o700)
    files = ["Dockerfile", "entrypoint.sh", "healthcheck.sh", "exit-on-fatal.py", "supervisord.conf"]
    for name in files:
        shutil.copyfile(HERE / name, output / name)
        (output / name).chmod(0o755 if name.endswith((".sh", ".py")) else 0o644)
    shutil.copyfile(binary, output / "web-access-gateway")
    (output / "web-access-gateway").chmod(0o755)
    allow = ["Dockerfile", "web-access-gateway", *files[1:]]
    (output / ".dockerignore").write_text("*\n" + "".join(f"!{name}\n" for name in allow))
    sums = []
    for name in allow:
        sums.append(f"{hashlib.sha256((output / name).read_bytes()).hexdigest()}  {name}\n")
    (output / "SHA256SUMS").write_text("".join(sums))
    return output


def render_nginx(hostname, output, public_port=19933):
    if len(hostname) > 253 or not re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?", hostname) or ".." in hostname:
        raise ValueError("an exact DNS hostname is required, without URL, path, port or wildcard")
    if type(public_port) is not int or not 1024 <= public_port <= 65535 or public_port in (19934, 19935):
        raise ValueError("public port must be an integer from 1024 to 65535, excluding 19934 and 19935")
    template = (HERE.parent / "nginx.conf.example").read_text()
    required = ["gateway.example.org", "pid /run/go-ldap-admin-web-access-nginx/nginx.pid;", "access_log /var/log/nginx/web-access-gateway.access.log gateway_safe;"]
    if any(value not in template for value in required) or template.count("listen 19933 ssl;") != 1:
        raise ValueError("upstream Nginx template changed; review the container rendering rules")
    template = template.replace("gateway.example.org", hostname)
    if public_port != 19933:
        template = template.replace("listen 19933 ssl;", f"listen {public_port} ssl;", 1)
    # Compose reserves one CPU; do not spawn a worker for every NAS host core.
    template = template.replace("worker_processes auto;", "worker_processes 1;", 1)
    template = template.replace("pid /run/go-ldap-admin-web-access-nginx/nginx.pid;", "pid /run/web-access-gateway/nginx.pid;")
    template = template.replace("access_log /var/log/nginx/web-access-gateway.access.log gateway_safe;", "access_log /dev/stdout gateway_safe;")
    template = template.replace("/etc/go-ldap-admin/web-access-gateway/https.", "/etc/go-ldap-admin-web-access-tls/https.")
    paths = """    client_body_temp_path /run/web-access-gateway/nginx/client-body;
    proxy_temp_path /run/web-access-gateway/nginx/proxy;
    fastcgi_temp_path /run/web-access-gateway/nginx/fastcgi;
    uwsgi_temp_path /run/web-access-gateway/nginx/uwsgi;
    scgi_temp_path /run/web-access-gateway/nginx/scgi;
"""
    template = template.replace("http {\n", "http {\n" + paths, 1)
    output = Path(output)
    with output.open("x") as stream:
        stream.write(template)
    output.chmod(0o644)
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    context = commands.add_parser("context")
    context.add_argument("--binary", required=True)
    context.add_argument("--out", required=True)
    nginx = commands.add_parser("nginx")
    nginx.add_argument("--hostname", required=True)
    nginx.add_argument("--public-port", type=int, default=19933)
    nginx.add_argument("--out", required=True)
    args = parser.parse_args()
    try:
        output = build_context(args.binary, args.out) if args.action == "context" else render_nginx(args.hostname, args.out, args.public_port)
    except (OSError, ValueError) as error:
        parser.error(str(error))
    print(output)


if __name__ == "__main__":
    main()
