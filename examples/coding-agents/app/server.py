# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Private HTTP/SSE facade for a real, single-tenant native Coding Runtime."""

import argparse
import http.server
import json
import os
from pathlib import Path
import select
import signal
import socket
import subprocess
import threading
import time
import uuid

from native import Gateway, RUNTIMES, RuntimeErrorCode, execute, launch


REQUEST_BYTES = 1 << 20
MESSAGE_COUNT = 100
INPUT_BYTES = 64 << 10
SSE_BYTES = 64 << 10
HTTP_TIMEOUT = 600


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = False

    def __init__(self, address, runtime, gateway, home):
        self.runtime = runtime
        self.gateway = gateway
        self.home = home
        self.busy = threading.Lock()
        self.stopping = threading.Event()
        super().__init__(address, Handler)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def setup(self):
        super().setup()
        self.connection.settimeout(15)

    def log_message(self, *_args):
        pass

    def json(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)
        self.close_connection = True

    def do_GET(self):
        try:
            if self.path == "/healthz":
                self.json(200, {"status": "ok"})
            elif self.path == "/v1/models":
                self.json(200, {
                    "object": "list",
                    "data": [{"id": model, "object": "model", "owned_by": "cetirun-gateway"}
                             for model in self.server.gateway.models()],
                })
            elif self.path == "/":
                data = Path(__file__).with_name("index.html").read_bytes()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(data)))
                self.send_header("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
                self.send_header("Cache-Control", "no-store")
                self.end_headers()
                self.wfile.write(data)
            else:
                self.json(404, {"error": {"code": "not_found"}})
        except RuntimeErrorCode as error:
            self.json(error.status, {"error": {"code": error.code}})
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            self.close_connection = True

    def cancelled(self):
        if self.server.stopping.is_set():
            return True
        if select.select([self.connection], [], [], 0)[0]:
            try:
                return not self.connection.recv(1, socket.MSG_PEEK)
            except OSError:
                return True
        return False

    def failure(self, status, code, streaming):
        try:
            if streaming:
                self.wfile.write(("data: " + json.dumps({"error": {"code": code}}) + "\n\n").encode())
                self.wfile.flush()
            else:
                self.json(status, {"error": {"code": code}})
        except OSError:
            self.close_connection = True

    def do_POST(self):
        acquired = False
        streaming = False
        try:
            if self.path != "/v1/chat/completions":
                raise RuntimeErrorCode("not_found", 404)
            if self.headers.get("Transfer-Encoding"):
                raise RuntimeErrorCode("transfer_encoding_not_supported", 400)
            if self.headers.get_content_type() != "application/json":
                raise RuntimeErrorCode("json_required", 415)
            lengths = self.headers.get_all("Content-Length", [])
            if len(lengths) != 1 or not lengths[0].isascii() or not lengths[0].isdigit():
                raise RuntimeErrorCode("invalid_content_length", 400)
            size = int(lengths[0])
            if not 0 < size <= REQUEST_BYTES:
                raise RuntimeErrorCode("body_limit_exceeded", 413)
            raw = self.rfile.read(size)
            if len(raw) != size:
                raise RuntimeErrorCode("incomplete_body", 400)
            body = json.loads(raw)
            messages = body.get("messages") if isinstance(body, dict) else None
            if not isinstance(messages, list) or not 1 <= len(messages) <= MESSAGE_COUNT:
                raise RuntimeErrorCode("invalid_messages", 400)
            for message in messages:
                if (
                    not isinstance(message, dict)
                    or message.get("role") not in ("system", "user", "assistant")
                    or not isinstance(message.get("content"), str)
                    or not message["content"].strip()
                ):
                    raise RuntimeErrorCode("text_messages_required", 400)
                if message["role"] == "user" and len(message["content"].encode("utf-8")) > INPUT_BYTES:
                    raise RuntimeErrorCode("input_limit_exceeded", 413)
            if messages[-1]["role"] != "user" or type(body.get("stream", False)) is not bool:
                raise RuntimeErrorCode("invalid_request", 400)
            if any(name in body for name in ("tools", "tool_choice", "n")):
                raise RuntimeErrorCode("unsupported_option", 400)
            memory_mode = self.headers.get("X-CetiRun-Memory-Mode", "")
            if memory_mode not in ("", "off", "no-capture"):
                raise RuntimeErrorCode("invalid_memory_mode", 400)
            model = body.get("model")
            if not isinstance(model, str) or model not in self.server.gateway.models():
                raise RuntimeErrorCode("model_not_authorized", 403)
            acquired = self.server.busy.acquire(blocking=False)
            if not acquired:
                raise RuntimeErrorCode("runtime_busy", 409)
            identifier = "chatcmpl-" + uuid.uuid4().hex
            created = int(time.time())
            last_heartbeat = time.monotonic()
            def encode_event(value):
                return ("data: " + json.dumps(value, ensure_ascii=False) + "\n\n").encode("utf-8")

            def event(value):
                data = encode_event(value)
                if len(data) > SSE_BYTES:
                    raise RuntimeErrorCode("sse_frame_limit_exceeded")
                self.wfile.write(data)
                self.wfile.flush()

            def payload(delta, finish=None):
                return {
                    "id": identifier, "object": "chat.completion.chunk",
                    "created": created, "model": model,
                    "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
                }

            def chunk(delta, finish=None):
                event(payload(delta, finish))

            def emit(text):
                if streaming:
                    budget = SSE_BYTES - len(encode_event(payload({"content": ""})))
                    # A Unicode scalar takes at most six bytes after JSON encoding.
                    count = budget // 6
                    if count < 1:
                        raise RuntimeErrorCode("sse_frame_limit_exceeded")
                    for start in range(0, len(text), count):
                        chunk({"content": text[start:start + count]})

            def tick():
                nonlocal last_heartbeat
                if streaming and time.monotonic() - last_heartbeat >= 15:
                    self.wfile.write(b": keepalive\n\n")
                    self.wfile.flush()
                    last_heartbeat = time.monotonic()

            if body.get("stream", False):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Connection", "close")
                self.send_header("X-Accel-Buffering", "no")
                self.end_headers()
                self.close_connection = True
                streaming = True
                chunk({"role": "assistant"})
            text = execute(
                self.server.runtime, self.server.gateway, model, messages,
                self.server.home, emit, tick, self.cancelled,
                deadline=HTTP_TIMEOUT, memory_mode=memory_mode,
            )
            if streaming:
                chunk({}, "stop")
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
            else:
                self.json(200, {
                    "id": identifier, "object": "chat.completion",
                    "created": created, "model": model,
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": text},
                                 "finish_reason": "stop"}],
                })
        except RuntimeErrorCode as error:
            self.failure(error.status, error.code, streaming)
        except (ValueError, UnicodeError):
            self.failure(502 if streaming else 400, "invalid_native_encoding" if streaming else "invalid_json", streaming)
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            self.close_connection = True
        except OSError:
            self.failure(502, "native_io_failed", streaming)
        finally:
            self.close_connection = True
            if acquired:
                self.server.busy.release()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("serve", "tui", "doctor"), nargs="?", default="serve")
    parser.add_argument("--runtime", choices=RUNTIMES, default=os.environ.get("CODING_RUNTIME", "copilot"))
    parser.add_argument("--model", default=os.environ.get("DEFAULT_MODEL"))
    args = parser.parse_args()
    try:
        gateway = Gateway.from_environment()
        home = Path(os.environ["HOME"])
        if args.action == "doctor":
            models = gateway.models()
            print(json.dumps({"runtime": args.runtime, "modelCount": len(models),
                              "requestedModelAuthorized": args.model in models if args.model else None}))
            return
        if args.action == "tui":
            models = gateway.models()
            if args.model not in models:
                raise RuntimeErrorCode("model_not_authorized", 403)
            with launch(args.runtime, gateway, args.model, home, interactive=True) as (argv, env, workspace):
                process = subprocess.Popen(argv, cwd=workspace, env=env)
                previous = signal.signal(signal.SIGTERM, lambda signum, _frame: parser.exit(128 + signum))
                try:
                    while True:
                        try:
                            parser.exit(process.wait())
                        except KeyboardInterrupt:
                            continue
                finally:
                    signal.signal(signal.SIGTERM, previous)
                    if process.poll() is None:
                        process.terminate()
                        try:
                            process.wait(timeout=2)
                        except subprocess.TimeoutExpired:
                            process.kill()
                            process.wait()
        server = Server((
            os.environ.get("CETIRUN_BIND_HOST", "0.0.0.0"),
            int(os.environ.get("PORT", "8080")),
        ), args.runtime, gateway, home)

        def interrupt(_signum, _frame):
            raise KeyboardInterrupt

        signal.signal(signal.SIGTERM, interrupt)
        print(f"Native {args.runtime} server listening on port {server.server_port}", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
        finally:
            server.stopping.set()
            server.server_close()
    except RuntimeErrorCode as error:
        parser.exit(1, error.code + "\n")


if __name__ == "__main__":
    main()
