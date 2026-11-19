# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Run in the pinned Copilot image, with no network except synthetic loopback."""

import gzip
import http.client
import http.server
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

from native_probe import Fixture as _Fixture, KEY, MODEL, native
import server


ANSWER = "Synthetic reply: 你好 👋"
MESSAGES = [
    {"role": "user", "content": "Earlier task: " + "x" * 40000},
    {"role": "assistant", "content": "Earlier answer: " + "y" * 40000},
    {"role": "user", "content": 'Hello 世界\nKeep the history and "quotes".'},
]


class Fixture(_Fixture):
    def do_POST(self):
        raw = self.rfile.read(int(self.headers["Content-Length"]))
        if self.headers.get("Content-Encoding") == "gzip":
            raw = gzip.decompress(raw)
        self.server.captured.append(json.loads(raw))
        if self.server.fail:
            self.respond(400, {"error": {"type": "invalid_request_error", "code": "synthetic_stop",
                                       "message": "Offline fixture failure."}})
            return
        content = {"type": "output_text", "text": ANSWER, "annotations": []}
        item = {"id": "msg_fixture", "type": "message", "role": "assistant",
                "status": "completed", "content": [content]}
        response = {"id": "resp_fixture", "object": "response", "created_at": 1,
                    "status": "completed", "model": MODEL, "output": [item],
                    "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
        events = [
            {"type": "response.created", "response": dict(response, status="in_progress", output=[])},
            {"type": "response.output_item.added", "output_index": 0,
             "item": dict(item, status="in_progress", content=[])},
            {"type": "response.content_part.added", "item_id": item["id"], "output_index": 0,
             "content_index": 0, "part": dict(content, text="")},
            {"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0,
             "content_index": 0, "delta": ANSWER},
            {"type": "response.output_text.done", "item_id": item["id"], "output_index": 0,
             "content_index": 0, "text": ANSWER},
            {"type": "response.content_part.done", "item_id": item["id"], "output_index": 0,
             "content_index": 0, "part": content},
            {"type": "response.output_item.done", "output_index": 0, "item": item},
            {"type": "response.completed", "response": response},
        ]
        data = "".join("event: " + event["type"] + "\ndata: "
                       + json.dumps(dict(event, sequence_number=i)) + "\n\n"
                       for i, event in enumerate(events)).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class CopilotChatTests(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.TemporaryDirectory()
        self.addCleanup(self.home.cleanup)
        home = Path(self.home.name)
        environment = {"PATH": os.environ["PATH"], "HOME": str(home), "LANG": "C.UTF-8",
                       "TERM": "xterm-256color", "XDG_CACHE_HOME": str(home / ".cache"),
                       "TMPDIR": str(home)}
        self.environment = patch.dict(os.environ, environment, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.fixture = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        self.fixture.captured = []
        self.fixture.fail = False
        self.fixture_thread = threading.Thread(target=self.fixture.serve_forever)
        self.fixture_thread.start()
        gateway = native.Gateway(f"http://127.0.0.1:{self.fixture.server_port}/v1", KEY)
        self.adapter = server.Server(("127.0.0.1", 0), "copilot", gateway, home)
        self.adapter_thread = threading.Thread(target=self.adapter.serve_forever)
        self.adapter_thread.start()
        self.events = []
        original = native.Events.accept

        def record(events, line):
            self.events.append(json.loads(line))
            return original(events, line)

        observer = patch.object(native.Events, "accept", record)
        observer.start()
        self.addCleanup(observer.stop)

    def tearDown(self):
        self.adapter.stopping.set()
        for instance, thread in ((self.adapter, self.adapter_thread), (self.fixture, self.fixture_thread)):
            instance.shutdown()
            instance.server_close()
            thread.join()

    def request(self, streaming):
        client = http.client.HTTPConnection(*self.adapter.server_address, timeout=60)
        try:
            client.request("POST", "/v1/chat/completions", json.dumps({
                "model": MODEL, "messages": MESSAGES, "stream": streaming,
            }).encode(), {"Content-Type": "application/json", "X-CetiRun-Memory-Mode": "off"})
            response = client.getresponse()
            return response.status, response.read().decode()
        finally:
            client.close()

    def test_complete_conversation_reaches_real_cli_model_request(self):
        self.request(False)
        self.assertEqual(len(self.fixture.captured), 1)
        prompt = native.prompt(MESSAGES)
        texts = [part["text"] for message in self.fixture.captured[0]["input"]
                 if message.get("role") == "user" for part in message.get("content", [])
                 if part.get("type") == "input_text"]
        self.assertTrue(any(prompt in text for text in texts), "Full stdin conversation was not forwarded")

    def test_json_success_requires_real_cli_result(self):
        status, body = self.request(False)
        self.assertEqual(status, 200, body)
        self.assertEqual(json.loads(body)["choices"][0]["message"]["content"], ANSWER)
        self.assertEqual([e["exitCode"] for e in self.events if e["type"] == "result"], [0])
        self.assertEqual(len(self.fixture.captured), 1)

    def test_sse_success_finishes_after_real_cli_result(self):
        status, body = self.request(True)
        self.assertEqual(status, 200)
        frames = [line.removeprefix("data: ") for line in body.splitlines() if line.startswith("data: ")]
        self.assertEqual(frames[-1], "[DONE]", body)
        chunks = [json.loads(frame) for frame in frames[:-1]]
        self.assertFalse(any("error" in chunk for chunk in chunks))
        self.assertEqual("".join(c["choices"][0]["delta"].get("content", "") for c in chunks), ANSWER)
        self.assertEqual(chunks[-1]["choices"][0]["finish_reason"], "stop")
        self.assertEqual([e["exitCode"] for e in self.events if e["type"] == "result"], [0])
        self.assertEqual(len(self.fixture.captured), 1)

    def test_native_failure_does_not_emit_success_or_replay(self):
        self.fixture.fail = True
        status, body = self.request(True)
        self.assertEqual(status, 200)
        self.assertIn('"code": "native_run_failed"', body)
        self.assertNotIn("[DONE]", body)
        self.assertNotIn('"finish_reason": "stop"', body)
        self.assertEqual(len(self.fixture.captured), 1)
        self.assertIn("session.error", [e["type"] for e in self.events])


if __name__ == "__main__":
    unittest.main()
