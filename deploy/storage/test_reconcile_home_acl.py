import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "reconcile_home_acl", Path(__file__).with_name("reconcile-home-acl.py")
)
acl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acl)


class HomeACLTests(unittest.TestCase):
    def test_only_exact_network_mount_is_accepted(self):
        with tempfile.TemporaryDirectory() as temporary, patch.object(acl.shutil, "which", return_value="/usr/bin/tool"):
            root = Path(temporary).resolve()
            home = root / "alice"
            home.mkdir()

            def mount(fs, source, target):
                return f'TARGET="{target}" SOURCE="{source}" FSTYPE="{fs}"'

            with patch.object(acl, "run", return_value=mount("ext4", "/dev/sda1", root)):
                with self.assertRaisesRegex(RuntimeError, "requires GlusterFS or NFSv4"):
                    acl.acl_backend(home, root)
            with patch.object(acl, "run", side_effect=[
                mount("nfs4", "server:/home", root),
                mount("ext4", "/dev/sda1", home),
            ]):
                with self.assertRaisesRegex(RuntimeError, "same mounted export"):
                    acl.acl_backend(home, root)
            with patch.object(acl, "run", return_value=mount("nfs4", "server:/home", root)):
                self.assertEqual(acl.acl_backend(home, root), "nfs4")
            with patch.object(acl, "run", return_value=mount("fuse.glusterfs", "server:/volume", root)):
                self.assertEqual(acl.acl_backend(home, root), "gluster")

    def test_gluster_backfill_filters_nested_filesystems(self):
        with tempfile.TemporaryDirectory() as temporary, patch.object(acl, "run") as run, patch.object(acl, "acl_backend", return_value="gluster"):
            root = Path(temporary)
            home = root / "alice"
            acl.reconcile_home_acl(root, 10001, home, {12001}, root)
            run.assert_any_call("find", str(home), "-xdev", "-fstype", "fuse.glusterfs",
                                "(", "-type", "d", "-o", "-type", "f", ")",
                                "-exec", "setfacl", "-m", "g:12001:r-X", "{}", "+")

    def test_nfs4_owner_numeric_gid_and_pending_backfill(self):
        with tempfile.TemporaryDirectory() as temporary, patch.object(acl, "acl_backend", return_value="nfs4"), patch.object(acl, "nfs4_as_owner") as owner:
            owner.return_value = ""
            root = Path(temporary)
            home = root / "alice"
            acl.reconcile_home_acl(root, 10001, home, {12001}, root, backfill_existing=False)
            owner.assert_called_once_with(
                10001, "nfs4_setfacl", "-s",
                "A::OWNER@:rwaDxtTnNcCoy,A:fdg:12001:rxtncy,A:g:GROUP@:tncy,A::EVERYONE@:tncy",
                str(home),
            )
            state = json.loads((root / "home-acl-10001.json").read_text())
            self.assertEqual(state["pending_backfill_gids"], [12001])
            owner.reset_mock()
            acl.reconcile_home_acl(root, 10001, home, {12001}, root)
            owner.assert_any_call(
                10001, "find", "-P", str(home), "-xdev", "-fstype", "nfs4",
                "-type", "f", "-exec", "nfs4_setfacl", "-a", "A:g:12001:RX", "{}", "+",
            )
            self.assertNotIn("pending_backfill_gids", json.loads((root / "home-acl-10001.json").read_text()))

    def test_nfs4_refuses_mixed_ownership_before_backfill(self):
        with tempfile.TemporaryDirectory() as temporary, patch.object(acl, "acl_backend", return_value="nfs4"), patch.object(acl, "nfs4_as_owner", return_value="/HOME/alice/foreign") as owner:
            root = Path(temporary)
            with self.assertRaisesRegex(RuntimeError, "non-owned HOME entry"):
                acl.reconcile_home_acl(root, 10001, root / "alice", {12001}, root)
            self.assertEqual(owner.call_count, 1)
            self.assertFalse((root / "home-acl-10001.json").exists())


if __name__ == "__main__":
    unittest.main()
