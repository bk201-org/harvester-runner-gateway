#!/usr/bin/env python3
"""Exercise smoke orchestration through a fake gateway client."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("smoke.sh")
TOKEN = "ab" * 32

FAKE_CLIENT = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

args = sys.argv[1:]
entry = {
    "args": args,
    "url": os.environ.get("GATEWAY_URL", ""),
    "tokenFile": os.environ.get("GATEWAY_TOKEN_FILE", ""),
    "hasToken": bool(os.environ.get("GATEWAY_TOKEN")),
    "caCert": os.environ.get("GATEWAY_CA_CERT", ""),
    "githubActions": os.environ.get("GITHUB_ACTIONS", ""),
    "audience": os.environ.get("GATEWAY_AUDIENCE", ""),
    "oidcURL": os.environ.get("ACTIONS_ID_TOKEN_REQUEST_URL", ""),
    "hasOIDCRequestToken": bool(os.environ.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN")),
}
with open(os.environ["FAKE_LOG"], "a", encoding="utf-8") as log:
    log.write(json.dumps(entry) + "\n")

state_path = Path(os.environ["FAKE_STATE"])
state = json.loads(state_path.read_text()) if state_path.exists() else {"power": "on"}
if args[:2] == ["vm", "power"]:
    state["power"] = args[3]
    state_path.write_text(json.dumps(state))
if os.environ.get("FAKE_FAIL_ATTACH") == "1" and args[:2] == ["vm", "attach"]:
    print("gateway HTTP 422: invalid_resource: simulated attach failure", file=sys.stderr)
    sys.exit(1)
if args[:2] == ["vm", "create"]:
    print(json.dumps({"id": "runner-gw-vm", "ready": True, "ipAddresses": ["10.0.0.10"]}))
elif args[:2] == ["volume", "create"]:
    print(json.dumps({"id": "runner-gw-volume"}))
elif args == ["vm", "get", "runner-gw-vm"]:
    print(json.dumps({"phase": "Running", "powerState": state["power"]}))
elif args == ["volume", "get", "runner-gw-volume"]:
    print(json.dumps({"phase": "Bound", "attachmentPhase": "Ready", "attachedTo": None}))
'''


class SmokeScriptTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.client = root / "harvester-runner-gateway-client"
        self.client.write_text(FAKE_CLIENT)
        self.client.chmod(0o755)
        self.log = root / "requests.jsonl"
        self.token_file = root / "token"
        self.token_file.write_text(TOKEN + "\n")
        self.token_file.chmod(0o600)
        self.config = root / "smoke.json"
        self.config_value = {
            "gatewayURL": "https://gateway.example.test",
            "image": "default/ubuntu",
            "network": "default/network",
            "tokenFile": str(self.token_file),
        }
        self.config.write_text(json.dumps(self.config_value))
        self.env = os.environ.copy()
        self.env.update({
            "GATEWAY_CLIENT": str(self.client),
            "GATEWAY_SMOKE_CONFIG": str(self.config),
            "FAKE_LOG": str(self.log),
            "FAKE_STATE": str(root / "state.json"),
        })
        for name in (
            "GITHUB_ACTIONS", "GATEWAY_SMOKE", "GATEWAY_TOKEN",
            "GATEWAY_TOKEN_FILE", "GATEWAY_CA_CERT", "GATEWAY_AUDIENCE",
            "ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN",
            "FAKE_FAIL_ATTACH",
        ):
            self.env.pop(name, None)

    def run_smoke(self, **extra_env):
        env = self.env.copy()
        env.update(extra_env)
        return subprocess.run(
            [str(SCRIPT)], env=env, capture_output=True, text=True, check=False
        )

    def requests(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    @staticmethod
    def flag_value(args, name):
        return args[args.index(name) + 1]

    def test_local_runs_use_client_token_file_and_fresh_keys(self):
        for _ in range(2):
            result = self.run_smoke(GATEWAY_TOKEN="must-not-be-used")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Gateway smoke test passed", result.stdout)
        requests = self.requests()
        self.assertTrue(all(item["tokenFile"] == str(self.token_file)
                            for item in requests))
        self.assertTrue(all(not item["hasToken"] for item in requests))
        creates = [item["args"] for item in requests
                   if item["args"][:2] == ["vm", "create"]]
        keys = [self.flag_value(args, "--idempotency-key") for args in creates]
        self.assertEqual(len(keys), 2)
        self.assertNotEqual(keys[0], keys[1])
        self.assertTrue(all(key.startswith("local-") for key in keys))
        self.assertTrue(all(self.flag_value(args, "--image") == "default/ubuntu"
                            for args in creates))
        self.assertTrue(all(self.flag_value(args, "--network") == "default/network"
                            for args in creates))

    def test_actions_run_uses_client_oidc_environment_and_opt_in(self):
        ca_cert = Path(self.temp.name) / "gateway-ca.crt"
        ca_cert.write_text("test certificate")
        env = {
            "GITHUB_ACTIONS": "true",
            "GATEWAY_SMOKE": "1",
            "GATEWAY_URL": "https://gateway.example.test",
            "GATEWAY_IMAGE": "default/ubuntu",
            "GATEWAY_NETWORK": "default/network",
            "GATEWAY_CA_CERT": str(ca_cert),
            "GATEWAY_TOKEN": "must-not-be-used",
            "GATEWAY_TOKEN_FILE": "/must/not/be/used",
            "ACTIONS_ID_TOKEN_REQUEST_URL": "https://oidc.example/token?request=1",
            "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
            "GITHUB_RUN_ID": "123",
            "GITHUB_RUN_ATTEMPT": "2",
            "GATEWAY_SMOKE_CONFIG": "/not/a/local/config",
        }
        result = self.run_smoke(**env)
        self.assertEqual(result.returncode, 0, result.stderr)
        requests = self.requests()
        self.assertTrue(all(item["githubActions"] == "true" for item in requests))
        self.assertTrue(all(item["tokenFile"] == "" and not item["hasToken"]
                            for item in requests))
        self.assertTrue(all(item["caCert"] == str(ca_cert) for item in requests))
        self.assertTrue(all(item["audience"] == "api://harvester-runner-gateway"
                            for item in requests))
        self.assertTrue(all(item["oidcURL"] == env["ACTIONS_ID_TOKEN_REQUEST_URL"]
                            and item["hasOIDCRequestToken"] for item in requests))
        create = next(item["args"] for item in requests
                      if item["args"][:2] == ["vm", "create"])
        self.assertEqual(self.flag_value(create, "--idempotency-key"), "123-2-vm")
        self.log.unlink()
        env["GATEWAY_SMOKE"] = "0"
        result = self.run_smoke(**env)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.log.exists(), "unapproved Actions run called the client")

    def test_local_run_passes_self_signed_certificate_to_client(self):
        ca_cert = Path(self.temp.name) / "gateway-ca.crt"
        ca_cert.write_text("test certificate")
        self.config_value["caCert"] = str(ca_cert)
        self.config.write_text(json.dumps(self.config_value))

        result = self.run_smoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(all(item["caCert"] == str(ca_cert)
                            for item in self.requests()))

    def test_failure_cleans_up_created_resources_with_client(self):
        result = self.run_smoke(FAKE_FAIL_ATTACH="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("simulated attach failure", result.stderr)
        commands = [item["args"] for item in self.requests()]
        self.assertIn(["volume", "delete", "runner-gw-volume"], commands)
        self.assertIn(["vm", "delete", "runner-gw-vm"], commands)

    def test_missing_client_fails_before_resource_operations(self):
        result = self.run_smoke(GATEWAY_CLIENT="/not/a/client")
        self.assertEqual(result.returncode, 2)
        self.assertIn("Gateway client is required", result.stderr)
        self.assertFalse(self.log.exists())

    def test_invalid_local_token_fails_before_client_call(self):
        self.token_file.write_text("not-a-smoke-token\n")
        result = self.run_smoke()
        self.assertEqual(result.returncode, 2)
        self.assertIn("64 hex characters", result.stderr)
        self.assertFalse(self.log.exists())


if __name__ == "__main__":
    unittest.main()
