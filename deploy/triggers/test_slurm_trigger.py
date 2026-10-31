import importlib.util
import pathlib
import subprocess
import unittest
from unittest.mock import patch

path = pathlib.Path(__file__).with_name("slurm_trigger.py")
spec = importlib.util.spec_from_file_location("slurm_trigger", path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class SlurmTriggerTest(unittest.TestCase):
    def test_application_admin_is_not_provisioned(self):
        with patch.object(module, "identity") as lookup, patch.object(module, "provision") as provision:
            module.apply({}, {"event": "user.updated", "data": {"user": {
                "username": "admin", "uidNumber": 0, "gidNumber": 0}}})
            lookup.assert_not_called()
            provision.assert_not_called()

    def test_uses_argument_array_and_checks_identity(self):
        config = {"cluster": "lab", "deletePolicy": "disable",
                  "identityPrefix": ["/usr/bin/getent"], "sacctmgrPrefix": ["/usr/bin/sacctmgr"]}
        payload = {"event": "user.created", "data": {"user": {
            "username": "alice", "uidNumber": 10001, "gidNumber": 10002,
            "homeDirectory": "/HOME/alice"}}}
        calls = []
        def fake_run(prefix, *args):
            calls.append([*prefix, *args])
            if prefix == config["identityPrefix"]:
                return "alice:x:10001:10002::/HOME/alice:/bin/bash"
            if "association" in args:
                return "lab|alice|\nlab|alice|alice"
            if "WithAssoc" in args:
                return "alice|lab|alice"
            return ""
        with patch.object(module, "run", side_effect=fake_run), patch.object(module, "ensure_storage") as storage:
            module.apply(config, payload)
            storage.assert_called_once_with(config, payload["data"]["user"])
        self.assertTrue(any(call[:2] == ["/usr/bin/getent", "passwd"] for call in calls))
        self.assertTrue(any("association" in call for call in calls))
        self.assertFalse(any("add" in call for call in calls))

    def test_refuses_mismatched_identity_and_live_deletion(self):
        config = {"cluster": "lab", "deletePolicy": "delete",
                  "identityPrefix": ["getent"], "sacctmgrPrefix": ["sacctmgr"]}
        user = {"username": "alice", "uidNumber": 10001, "gidNumber": 10002,
                "homeDirectory": "/HOME/alice"}
        with patch.object(module, "run", return_value="alice:x:10003:10002::/HOME/alice:/bin/bash"):
            with self.assertRaisesRegex(ValueError, "differs"):
                module.apply(config, {"event": "user.created", "data": {"user": user}})
        with patch.object(module, "run", return_value="alice:x:10001:10002::/HOME/alice:/bin/bash"):
            with self.assertRaisesRegex(ValueError, "still exists"):
                module.apply(config, {"event": "user.deleted", "data": {"user": user}})

    def test_missing_identity_on_delete_is_safe(self):
        config = {"cluster": "lab", "deletePolicy": "delete",
                  "identityPrefix": ["getent"], "sacctmgrPrefix": ["sacctmgr"]}
        missing = subprocess.CalledProcessError(2, ["getent", "passwd", "alice"])
        with patch.object(module, "identity", side_effect=missing), patch.object(module, "delete") as deletion:
            module.apply(config, {"event": "user.deleted", "data": {"user": {
                "username": "alice", "uidNumber": 10001, "gidNumber": 10001}}})
            deletion.assert_called_once_with(["sacctmgr"], "lab", "alice")


if __name__ == "__main__":
    unittest.main()
