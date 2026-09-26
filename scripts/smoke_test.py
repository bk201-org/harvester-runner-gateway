#!/usr/bin/env python3
"""Exercise both smoke authentication paths without creating Harvester resources."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("smoke.sh")
TOKEN = "ab" * 32

FAKE_CURL = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import re
import sys
from urllib.parse import urlparse

args = sys.argv[1:]
url = args[-1]
if url.startswith("https://oidc.example/token"):
    print(json.dumps({"value": "oidc-token"}))
    sys.exit(0)

method = args[args.index("-X") + 1]
headers = [args[i + 1] for i, arg in enumerate(args[:-1]) if arg == "-H"]
config = sys.stdin.read() if "--config" in args else ""
match = re.search(r'header = "Authorization: Bearer ([^"]+)"', config)
auth = match.group(1) if match else ""
key = next((h.removeprefix("Idempotency-Key: ") for h in headers
            if h.startswith("Idempotency-Key: ")), "")
path = urlparse(url).path
with open(os.environ["FAKE_LOG"], "a", encoding="utf-8") as log:
    log.write(json.dumps({"method": method, "path": path, "auth": auth, "key": key}) + "\n")

state_path = Path(os.environ["FAKE_STATE"])
state = json.loads(state_path.read_text()) if state_path.exists() else {"power": "on"}
if method == "PUT" and path.endswith("/power"):
    body = args[args.index("--data") + 1]
    state["power"] = json.loads(body)["state"]
    state_path.write_text(json.dumps(state))
if os.environ.get("FAKE_FAIL_ATTACH") == "1" and method == "PUT" and "/volumes/" in path:
    print("simulated attach failure", file=sys.stderr)
    sys.exit(22)
if method == "POST" and path == "/v1/vms":
    print(json.dumps({"id": "hrgw-vm"}))
elif method == "POST" and path == "/v1/volumes":
    print(json.dumps({"id": "hrgw-volume"}))
elif method == "GET" and path == "/v1/vms/hrgw-vm":
    print(json.dumps({"phase": "Running", "powerState": state["power"]}))
elif method == "GET" and path == "/v1/volumes/hrgw-volume":
    print(json.dumps({"phase": "Bound", "attachmentPhase": "Ready", "attachedTo": None}))
'''


class SmokeScriptTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        bin_dir = root / "bin"
        bin_dir.mkdir()
        curl = bin_dir / "curl"
        curl.write_text(FAKE_CURL)
        curl.chmod(0o755)
        self.log = root / "requests.jsonl"
        token_file = root / "token"
        token_file.write_text(TOKEN + "\n")
        token_file.chmod(0o600)
        config = root / "smoke.json"
        config.write_text(json.dumps({
            "gatewayURL": "https://gateway.example.test",
            "image": "default/ubuntu",
            "network": "default/network",
            "tokenFile": str(token_file),
        }))
        self.env = os.environ.copy()
        self.env.update({
            "PATH": str(bin_dir) + os.pathsep + self.env["PATH"],
            "GATEWAY_SMOKE_CONFIG": str(config),
            "FAKE_LOG": str(self.log),
            "FAKE_STATE": str(root / "state.json"),
        })
        for name in ("GITHUB_ACTIONS", "GATEWAY_SMOKE", "FAKE_FAIL_ATTACH"):
            self.env.pop(name, None)

    def run_smoke(self, **extra_env):
        env = self.env.copy()
        env.update(extra_env)
        return subprocess.run([str(SCRIPT)], env=env, capture_output=True, text=True, check=False)

    def requests(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_local_runs_use_token_and_fresh_keys(self):
        for _ in range(2):
            result = self.run_smoke()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Gateway smoke test passed", result.stdout)
        requests = self.requests()
        self.assertTrue(all(item["auth"] == TOKEN for item in requests))
        keys = [item["key"] for item in requests
                if item["method"] == "POST" and item["path"] == "/v1/vms"]
        self.assertEqual(len(keys), 2)
        self.assertNotEqual(keys[0], keys[1])
        self.assertTrue(all(key.startswith("local-") for key in keys))

    def test_actions_run_uses_oidc_and_opt_in(self):
        env = {
            "GITHUB_ACTIONS": "true",
            "GATEWAY_SMOKE": "1",
            "GATEWAY_URL": "https://gateway.example.test",
            "GATEWAY_IMAGE": "default/ubuntu",
            "GATEWAY_NETWORK": "default/network",
            "ACTIONS_ID_TOKEN_REQUEST_URL": "https://oidc.example/token?request=1",
            "ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token",
            "GITHUB_RUN_ID": "123",
            "GITHUB_RUN_ATTEMPT": "2",
            "GATEWAY_SMOKE_CONFIG": "/not/a/local/config",
        }
        result = self.run_smoke(**env)
        self.assertEqual(result.returncode, 0, result.stderr)
        requests = self.requests()
        self.assertTrue(all(item["auth"] == "oidc-token" for item in requests))
        self.assertIn("123-2-vm", [item["key"] for item in requests])
        self.log.unlink()
        env["GATEWAY_SMOKE"] = "0"
        result = self.run_smoke(**env)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.log.exists(), "unapproved Actions run called the gateway")

    def test_failure_cleans_up_created_resources(self):
        result = self.run_smoke(FAKE_FAIL_ATTACH="1")
        self.assertNotEqual(result.returncode, 0)
        requests = self.requests()
        self.assertIn(("DELETE", "/v1/volumes/hrgw-volume"),
                      [(item["method"], item["path"]) for item in requests])
        self.assertIn(("DELETE", "/v1/vms/hrgw-vm"),
                      [(item["method"], item["path"]) for item in requests])


if __name__ == "__main__":
    unittest.main()
