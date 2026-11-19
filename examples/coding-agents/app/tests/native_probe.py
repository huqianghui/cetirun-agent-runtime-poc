# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline native-CLI contract probe; run inside the matching pinned image."""

import gzip
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
from urllib.parse import urlsplit
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import native


KEY = "synthetic-gateway-key"
MODEL = "fixture-coding-model"


class Fixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def respond(self, status, value):
        data = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.respond(200, {"data": [{"id": MODEL, "object": "model"}]})

    def do_POST(self):
        data = self.rfile.read(int(self.headers["Content-Length"]))
        if self.headers.get("Content-Encoding") == "gzip":
            data = gzip.decompress(data)
        self.server.captured.append((self.path, dict(self.headers), json.loads(data)))
        self.respond(400, {"error": {
            "type": "invalid_request_error", "code": "synthetic_stop",
            "message": "Offline fixture stops before inference.",
        }})


def main(runtime, memory_mode="off"):
    fixture = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
    fixture.captured = []
    thread = threading.Thread(target=fixture.serve_forever)
    thread.start()
    try:
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            canonical = home / ".agents"
            for name, description in (
                ("platform-skill-marker", "platform-skill-marker verifies native skill discovery"),
                ("cetirun-memory", "platform-memory-skill-marker describes optional Memory"),
            ):
                skill = canonical / "skills" / name
                skill.mkdir(parents=True)
                (skill / "SKILL.md").write_text(f"---\nname: {name}\ndescription: {description}\n---\nSynthetic skill.\n")
            (canonical / "AGENTS.md").write_text("platform-main-marker: obey the platform instructions.\n")
            user_config = home / ".copilot"
            user_config.mkdir()
            stale = '{"providers":[{"name":"old","type":"openai","baseUrl":"http://127.0.0.1:1/v1","apiKey":"synthetic-old-key"}],"models":[{"id":"fixture-coding-model","provider":"old"}]}'
            (user_config / "providers.json").write_text(stale)
            (user_config / "settings.json").write_text('{"model":"old/fixture-coding-model","memory":true}')
            assets = home / "provided-assets"
            helper = assets / "bin/cetirun-memory"
            helper.parent.mkdir(parents=True)
            helper.write_text(f"#!{sys.executable}\n" + """
import json, os, sys
from pathlib import Path
assert sys.argv[1:] == ["mcp", "--stdio", "--owner-private"]
root = Path(os.environ["CETIRUN_MEMORY_ROOT"])
root.mkdir(exist_ok=True)
(root / "fixture-flags.json").write_text(json.dumps({
    name: os.environ.get(name) for name in ("CETIRUN_MEMORY_NO_MEMORY", "CETIRUN_MEMORY_NO_CAPTURE")
}))
for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    method = request.get("method")
    result = {}
    if method == "initialize":
        result = {"protocolVersion": request["params"]["protocolVersion"],
                  "capabilities": {"tools": {}}, "serverInfo": {"name": "synthetic-memory", "version": "1"}}
    elif method == "tools/list":
        result = {"tools": [{"name": "cetirun_memory", "description": "owner-private-synthetic-memory-tool-marker",
                            "inputSchema": {"type": "object", "properties": {}}}]}
    elif method == "resources/list":
        result = {"resources": []}
    elif method == "prompts/list":
        result = {"prompts": []}
    print(json.dumps({"jsonrpc": "2.0", "id": request["id"], "result": result}), flush=True)
""")
            helper.chmod(0o700)
            memory_root = home / "synthetic-memory-root"
            gateway = native.Gateway(f"http://127.0.0.1:{fixture.server_port}/v1", KEY)
            environment = {
                "PATH": os.environ["PATH"], "HOME": str(home), "TERM": "xterm-256color",
                "XDG_CACHE_HOME": "/tmp/.cache", "TMPDIR": "/tmp", "LANG": "C.UTF-8",
                "COPILOT_HOME": str(user_config),
                "COPILOT_PROVIDERS_CONFIG": str(user_config / "providers.json"),
                "CETIRUN_MEMORY_ASSETS": str(assets), "CETIRUN_MEMORY_ROOT": str(memory_root),
            }
            with patch.dict(os.environ, environment, clear=True), native.launch(
                runtime, gateway, MODEL, home, memory_mode=memory_mode,
            ) as (argv, env, workspace):
                if runtime == "copilot":
                    root = Path(env["COPILOT_PROVIDERS_CONFIG"]).parent
                    for path in root.rglob("*"):
                        if path.is_file():
                            assert KEY not in path.read_text(), f"Gateway key written to {path.name}"
                process = subprocess.Popen(
                    argv, env=env, cwd=workspace, stdin=subprocess.PIPE,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
                )
                try:
                    output, error = process.communicate(
                        native.prompt([{"role": "user", "content": "Inspect this workspace only."}]).encode(),
                        timeout=60,
                    )
                except subprocess.TimeoutExpired:
                    native.terminate(process)
                    output, error = process.communicate()
                if not fixture.captured:
                    raise AssertionError(f"No request reached the synthetic Gateway:\n{output[-3000:]!r}\n{error[-3000:]!r}")
            assert (user_config / "providers.json").read_text() == stale, "User provider file changed"
            candidates = [request for request in fixture.captured if "platform-main-marker" in json.dumps(request[2])]
            if not candidates:
                details = [{
                    "path": item[0], "keys": list(item[2]),
                    "skillFound": "platform-skill-marker" in json.dumps(item[2]),
                    "inputRoles": [entry.get("role") for entry in item[2].get("input", []) if isinstance(entry, dict)],
                } for item in fixture.captured]
                raise AssertionError(f"Platform main instructions absent: {details!r}\n{output[-3000:]!r}\n{error[-3000:]!r}")
            path, headers, payload = candidates[0]
            assert urlsplit(path).path == ("/v1/messages" if runtime == "claude" else "/v1/responses"), path
            lower = {key.lower(): value for key, value in headers.items()}
            assert lower.get("authorization") == "Bearer " + KEY or lower.get("x-api-key") == KEY, "Wrong Gateway key"
            assert payload["model"] == MODEL, payload["model"]
            context = json.dumps(payload, ensure_ascii=False)
            assert "Inspect this workspace only." in context, "User task absent from model request"
            assert "platform-main-marker" in context, "Platform main instructions absent"
            assert "platform-skill-marker" in context, "Native platform skills absent"
            enabled = memory_mode != "off"
            memory_skill = "cetirun-platform:cetirun-memory" if runtime == "claude" else "platform-memory-skill-marker"
            assert (memory_skill in context) == enabled, "Wrong Memory skill discovery"
            assert ("owner-private-synthetic-memory-tool-marker" in context) == enabled, "Wrong Memory MCP exposure"
            if enabled:
                flags = json.loads((memory_root / "fixture-flags.json").read_text())
                assert flags == {"CETIRUN_MEMORY_NO_MEMORY": "false", "CETIRUN_MEMORY_NO_CAPTURE": "true"}, flags
            else:
                assert not memory_root.exists(), "Memory-off helper was started"
            print(f"{runtime} ({memory_mode}): Gateway routing, original config preservation, main instructions, native skills and Memory MCP restrictions passed")
    finally:
        fixture.shutdown()
        fixture.server_close()
        thread.join()


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else "off")
