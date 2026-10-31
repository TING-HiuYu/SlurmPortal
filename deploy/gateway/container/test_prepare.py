import importlib.util
import io
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, HERE / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


prepare = load("gateway_container_prepare", "prepare.py")
fatal = load("gateway_exit_on_fatal", "exit-on-fatal.py")


class PreparationTests(unittest.TestCase):
    def test_build_context_excludes_neighbor_credentials_and_refuses_reuse(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            header = bytearray(20)
            header[:6] = b"\x7fELF\x02\x01"
            header[18:20] = (62).to_bytes(2, "little")
            binary = root / "web-access-gateway"
            binary.write_bytes(header)
            (root / "master-client.key.pem").write_text("PRIVATE TEST NEIGHBOR")
            out = root / "context"
            prepare.build_context(binary, out)
            self.assertEqual(set(path.name for path in out.iterdir()), {"Dockerfile", "web-access-gateway", "entrypoint.sh", "healthcheck.sh", "exit-on-fatal.py", "supervisord.conf", ".dockerignore", "SHA256SUMS"})
            self.assertEqual((out / "web-access-gateway").read_bytes(), header)
            self.assertEqual(out.stat().st_mode & 0o777, 0o700)
            with self.assertRaises(ValueError):
                prepare.build_context(binary, out)
            binary.write_text("Mach-O or a script must not be shipped as Linux")
            with self.assertRaises(ValueError):
                prepare.build_context(binary, root / "invalid")

    def test_nginx_preserves_auth_tls_and_exact_origin_without_readonly_writes(self):
        with tempfile.TemporaryDirectory() as temporary:
            out = Path(temporary) / "nginx.conf"
            prepare.render_nginx("cluster2-login.example.org", out)
            text = out.read_text()
            for required in ["worker_processes 1;", "listen 19933 ssl;", "server_name cluster2-login.example.org;", "proxy_ssl_name cluster2-login.example.org;", "proxy_ssl_verify on;", "https://127.0.0.1:19934", "__Host-hpc_gateway_jwt=", "https://old-portal.example.org", "return 403;", "return 204;", "/etc/go-ldap-admin-web-access-tls/https.key.pem", "client_body_temp_path /run/web-access-gateway/nginx/client-body;", "access_log /dev/stdout gateway_safe;"]:
                self.assertIn(required, text)
            self.assertNotIn("/var/log/nginx/", text)
            self.assertNotIn("proxy_ssl_verify off", text)
            self.assertNotIn("proxy_pass http://127.0.0.1:19935", text)
            with self.assertRaises(FileExistsError):
                prepare.render_nginx("cluster2-login.example.org", out)
            for bad in ["https://host", "host;return 200;", "*.example.org", "host/path", "host:19933", "x\nserver {}", "a..org"]:
                with self.assertRaises(ValueError):
                    prepare.render_nginx(bad, Path(temporary) / "bad")

    def test_alternate_public_port_only_changes_listener_without_old_port(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            default = prepare.render_nginx("cluster2-login.example.org", root / "default.conf").read_text()
            alternate = prepare.render_nginx("cluster2-login.example.org", root / "7000.conf", public_port=7000).read_text()
            self.assertEqual(alternate, default.replace("listen 19933 ssl;", "listen 7000 ssl;", 1))
            self.assertEqual(["7000 ssl"], re.findall(r"^\s*listen\s+([^;]+);", alternate, re.MULTILINE))
            # Exact equality above preserves every TLS, SNI, JWT/Origin and
            # callback restriction rather than checking only selected strings.
            self.assertNotIn("listen 127.0.0.1:19933 ssl;", default)
            self.assertNotIn("19933", alternate)

    def test_invalid_or_conflicting_public_ports_do_not_create_configuration(self):
        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary) / "rejected.conf"
            for value in (None, True, False, "7000", 7000.0, [], {}, -1, 1023, 65536, 19934, 19935):
                with self.subTest(public_port=value):
                    with self.assertRaisesRegex(ValueError, "public port"):
                        prepare.render_nginx("cluster2-login.example.org", target, public_port=value)
                    self.assertFalse(target.exists())
            for value in (1024, 65535):
                text = prepare.render_nginx("cluster2-login.example.org", Path(temporary) / f"{value}.conf", public_port=value).read_text()
                self.assertIn(f"listen {value} ssl;", text)
                self.assertNotIn("19933", text)

    def test_nginx_cli_accepts_configured_port_and_rejects_callback_port(self):
        with tempfile.TemporaryDirectory() as temporary:
            out = Path(temporary) / "7000.conf"
            result = subprocess.run([sys.executable, str(HERE / "prepare.py"), "nginx", "--hostname", "cluster2-login.example.org", "--public-port", "7000", "--out", str(out)], capture_output=True, text=True)
            self.assertEqual(0, result.returncode, result.stderr)
            self.assertIn("listen 7000 ssl;", out.read_text())
            rejected = Path(temporary) / "callback.conf"
            result = subprocess.run([sys.executable, str(HERE / "prepare.py"), "nginx", "--hostname", "cluster2-login.example.org", "--public-port", "19935", "--out", str(rejected)], capture_output=True, text=True)
            self.assertNotEqual(0, result.returncode)
            self.assertFalse(rejected.exists())

    def healthcheck(self, root, public_port=None, public_status="403", ready_exit=0):
        # Record argv instead of connecting anywhere or creating credentials.
        curl = root / "curl"
        curl.write_text(f"#!{sys.executable}\n" + """import json, os, sys
with open(os.environ['CURL_ARGUMENTS'], 'a') as stream:
    stream.write(json.dumps(sys.argv[1:]) + '\\n')
if '--fail' in sys.argv:
    sys.exit(int(os.environ['READY_EXIT']))
print(os.environ['PUBLIC_STATUS'], end='')
""")
        curl.chmod(0o755)
        arguments = root / "curl-arguments.jsonl"
        if arguments.exists():
            arguments.unlink()
        env = {**os.environ, "PATH": str(root) + os.pathsep + os.environ.get("PATH", ""), "GATEWAY_TLS_HOST": "cluster2-login.example.org", "CURL_ARGUMENTS": str(arguments), "READY_EXIT": str(ready_exit), "PUBLIC_STATUS": public_status}
        env.pop("GATEWAY_PUBLIC_PORT", None)
        if public_port is not None:
            env["GATEWAY_PUBLIC_PORT"] = public_port
        result = subprocess.run(["/bin/sh", str(HERE / "healthcheck.sh")], env=env, capture_output=True, text=True)
        calls = [json.loads(line) for line in arguments.read_text().splitlines()] if arguments.exists() else []
        return result, calls

    def test_healthcheck_public_port_matches_nginx_and_preserves_tls_sni_and_unauthenticated_403(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for configured, expected in ((None, 19933), ("7000", 7000)):
                with self.subTest(public_port=configured):
                    result, calls = self.healthcheck(root, configured)
                    self.assertEqual(0, result.returncode, result.stderr)
                    self.assertEqual(2, len(calls))
                    self.assertIn("cluster2-login.example.org:19934:127.0.0.1", calls[0])
                    self.assertIn("https://cluster2-login.example.org:19934/readyz", calls[0])
                    self.assertIn(f"cluster2-login.example.org:{expected}:127.0.0.1", calls[1])
                    self.assertIn(f"https://cluster2-login.example.org:{expected}/v1/auth/session", calls[1])
                    for call in calls:
                        self.assertIn("--resolve", call)
                        self.assertEqual("*", call[call.index("--noproxy") + 1])
                        self.assertFalse(set(call) & {"-k", "--insecure", "-H", "--header", "-b", "--cookie", "-u", "--user"})
                        self.assertFalse(any("Bearer" in argument or "Authorization" in argument for argument in call))
                    text = prepare.render_nginx("cluster2-login.example.org", root / f"nginx-{expected}.conf", public_port=expected).read_text()
                    self.assertEqual([f"{expected} ssl"], re.findall(r"^\s*listen\s+([^;]+);", text, re.MULTILINE))
                    if expected == 7000:
                        self.assertNotIn("19933", " ".join(calls[1]))

    def test_healthcheck_rejects_invalid_ports_before_curl_or_shell_expansion(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            marker = root / "injection-must-not-run"
            for value in ("", "0", "1023", "65536", "19934", "19935", "7000.0", "07000", "+7000", " 7000", "7000\n", "9" * 100, f"7000;touch {marker}", f"$(touch {marker})"):
                with self.subTest(public_port=value):
                    result, calls = self.healthcheck(root, value)
                    self.assertNotEqual(0, result.returncode)
                    self.assertEqual([], calls)
                    self.assertFalse(marker.exists())

    def test_healthcheck_fails_on_tls_failure_or_any_public_result_other_than_403(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result, calls = self.healthcheck(root, "7000", ready_exit=60)
            self.assertNotEqual(0, result.returncode)
            self.assertEqual(1, len(calls))
            for code in ("200", "204", "302", "401", "500"):
                with self.subTest(public_status=code):
                    result, _ = self.healthcheck(root, "7000", public_status=code)
                    self.assertNotEqual(0, result.returncode)

    def test_supervisor_fatal_requests_parent_shutdown_with_correct_protocol(self):
        output = io.StringIO()
        payload = "processname:gateway groupname:gateway from_state:BACKOFF"
        header = f"ver:3.0 eventname:PROCESS_STATE_FATAL len:{len(payload)}\n"
        with patch.object(fatal.os, "kill") as kill, patch.object(fatal.os, "getppid", return_value=456):
            fatal.main(io.StringIO(header + payload), output)
        self.assertEqual(output.getvalue(), "READY\nRESULT 2\nOK")
        kill.assert_called_once_with(456, signal.SIGTERM)

    @unittest.skipUnless(shutil.which("docker"), "Docker Compose CLI is unavailable")
    def test_compose_resolves_without_daemon_and_exposes_only_reviewed_mounts(self):
        with tempfile.TemporaryDirectory() as temporary:
            env = {**os.environ, "GATEWAY_IMAGE": "gateway:test-only", "GATEWAY_TLS_HOST": "cluster2-login.example.org", "GATEWAY_CONFIG_DIR": temporary + "/config", "GATEWAY_TLS_DIR": temporary + "/tls", "GATEWAY_TRUST_DIR": temporary + "/public-trust", "GATEWAY_STATE_DIR": temporary + "/state"}
            env.pop("GATEWAY_PUBLIC_PORT", None)
            result = subprocess.run(["docker", "compose", "-f", str(HERE / "compose.yaml.example"), "config", "--format", "json"], env=env, capture_output=True, text=True, check=True)
            service = json.loads(result.stdout)["services"]["gateway"]
            self.assertEqual("19933", service["environment"]["GATEWAY_PUBLIC_PORT"])
            self.assertEqual(service["network_mode"], "host")
            self.assertEqual(service["user"], "19033:19033")
            self.assertTrue(service["read_only"])
            self.assertEqual(service["cap_drop"], ["ALL"])
            self.assertIn("no-new-privileges:true", service["security_opt"])
            self.assertNotIn("ports", service)
            self.assertNotIn("privileged", service)
            binds = service["volumes"]
            self.assertEqual(len(binds), 4)
            for volume in binds:
                self.assertFalse(volume.get("bind", {}).get("create_host_path", False))
                self.assertFalse(any(value in volume["target"] for value in ["/HOME", "docker.sock", "/dev/"]))
                self.assertEqual(volume.get("read_only", False), volume["target"] != "/var/lib/go-ldap-admin-web-access-gateway")
            env["GATEWAY_PUBLIC_PORT"] = "7000"
            alternate = subprocess.run(["docker", "compose", "-f", str(HERE / "compose.yaml.example"), "config", "--format", "json"], env=env, capture_output=True, text=True, check=True)
            self.assertEqual("7000", json.loads(alternate.stdout)["services"]["gateway"]["environment"]["GATEWAY_PUBLIC_PORT"])


if __name__ == "__main__":
    unittest.main()
