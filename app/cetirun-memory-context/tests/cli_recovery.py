#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Exercise the installed helper on disposable stores, including real process loss."""

import argparse
import ctypes
import json
import os
from pathlib import Path
import select
import signal
import struct
import subprocess
import sys
import tempfile
import time
import uuid


class Store:
    def __init__(self, helper, root):
        self.helper = helper
        self.root = root
        self.env = {**os.environ, "CETIRUN_MEMORY_ROOT": str(root)}

    def request(self, payload, generation=0):
        return {
            "contractVersion": "memory-context/v1",
            "requestId": "cli-" + uuid.uuid4().hex,
            "expectedGeneration": generation,
            "requestContext": {
                "audience": "owner-private", "destination": {"kind": "owner-private"},
                "scope": {"project": "synthetic-recovery"}, "opportunityId": "synthetic-task",
            },
            "payload": payload,
        }

    def execute(self, command, request):
        result = subprocess.run(
            [str(self.helper), *command.split()], input=json.dumps(request),
            text=True, capture_output=True, env=self.env, timeout=30,
        )
        response = json.loads(result.stdout)
        if result.returncode != 0 or not response.get("ok"):
            raise AssertionError(f"{command} failed: {response.get('reason')} (exit {result.returncode})")
        return response

    def status(self):
        return self.execute("status", self.request({}))

    def call(self, command, payload):
        generation = self.status().get("generation", 0)
        return self.execute(command, self.request(payload, generation))

    def capture(self, title, body="Synthetic retained knowledge.", source=None):
        note = {
            "kind": "fact", "title": title, "body": body,
            "scope": {"project": "synthetic-recovery"}, "status": "active",
            "temporalClass": "historical", "revalidation": {"mode": "on-change"},
            "sources": [{"kind": "fixture", "ref": "synthetic-source", "origin": "tool-observed",
                         "assurance": "reported", "observedAt": "2026-11-01T00:00:00Z", "version": "v1"}],
        }
        if source:
            note["derivedFrom"] = [{"ref": source, "role": "counterexample"}]
        return self.call("capture", {"note": note})["data"]["refs"][0]


def interrupt_after_head(store, plan):
    libc = ctypes.CDLL(None, use_errno=True)
    libc.inotify_init1.argtypes = [ctypes.c_int]
    libc.inotify_init1.restype = ctypes.c_int
    libc.inotify_add_watch.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_uint32]
    libc.inotify_add_watch.restype = ctypes.c_int
    descriptor = libc.inotify_init1(os.O_CLOEXEC | os.O_NONBLOCK)
    if descriptor < 0:
        raise OSError(ctypes.get_errno(), "inotify_init1 failed")
    process = None
    try:
        if libc.inotify_add_watch(descriptor, os.fsencode(store.root / "state"), 0x80) < 0:
            raise OSError(ctypes.get_errno(), "inotify_add_watch failed")
        request = store.request({"plan": plan}, store.status()["generation"])
        process = subprocess.Popen(
            [str(store.helper), "forget", "apply"], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=store.env,
        )
        process.stdin.write(json.dumps(request))
        process.stdin.close()
        process.stdin = None
        deadline = time.monotonic() + 10
        observed = False
        while time.monotonic() < deadline and not observed:
            ready, _, _ = select.select([descriptor], [], [], 0.1)
            if ready:
                events = os.read(descriptor, 65536)
                offset = 0
                while offset < len(events):
                    _, mask, _, length = struct.unpack_from("iIII", events, offset)
                    name = events[offset + 16:offset + 16 + length].rstrip(b"\0")
                    offset += 16 + length
                    if mask & 0x80 and name == b"HEAD.json":
                        os.kill(process.pid, signal.SIGSTOP)
                        os.kill(process.pid, signal.SIGKILL)
                        observed = True
                        break
            if process.poll() is not None and not observed:
                raise AssertionError("The helper exited before the HEAD interruption was observed")
        if not observed:
            raise AssertionError("No HEAD switch observed within the test deadline")
        process.communicate(timeout=10)
        if process.returncode != -signal.SIGKILL:
            raise AssertionError("The helper was not terminated at the observed boundary")
    finally:
        if process and process.poll() is None:
            process.kill()
            process.communicate(timeout=10)
        os.close(descriptor)


def run_checks(helper, work_dir):
    with tempfile.TemporaryDirectory(prefix="memory-cli-recovery-", dir=work_dir) as temporary:
        store = Store(helper, Path(temporary) / ".cetirun/memory")
        store.call("init", {})
        kept = store.capture("retained record")
        root = store.capture("forgotten record", "SYNTHETIC_PROCESS_FORGET_MARKER")
        for index in range(24):
            store.capture(f"forgotten derivative {index}", "SYNTHETIC_DERIVED_MARKER", root)
        plan = store.call("forget plan", {"refs": [root]})["data"]
        interrupt_after_head(store, plan)
        body = store.root / "objects/note" / root["id"] / "r1.md"
        if not body.is_file():
            raise AssertionError("Interruption was too late to witness pending physical deletion")
        pending = store.status()
        pending_reported = pending["data"].get("purgePending") and plan["digest"] in pending["data"].get("pendingPurges", [])
        store.capture("write after process loss")
        if store.status()["data"].get("purgePending") or any((store.root / name).exists() for name in plan["files"]):
            raise AssertionError("The next write did not finish the durable purge")
        if not pending_reported:
            raise AssertionError("status did not expose the interrupted deletion")
        hits = store.call("query", {"queryTerms": ["retained record"]})["data"]["hits"]
        if len(hits) != 1 or hits[0]["ref"] != kept:
            raise AssertionError("Process recovery changed unrelated committed knowledge")

        damaged = store.capture("damaged record")
        damaged_file = store.root / "objects/note" / damaged["id"] / "r1.md"
        damaged_file.write_text("Synthetic damaged bytes.\n")
        query = store.call("query", {"queryTerms": ["retained record"]})
        if len(query["data"]["hits"]) != 1 or not query.get("partial"):
            raise AssertionError("A damaged note hid healthy knowledge or its partial-result warning")
        index = store.root / "index.md"
        edit = "Synthetic owner index edit.\n"
        index.write_text(edit)
        repaired = store.call("repair-index", {})
        if "DamagedObject:" + damaged["id"] not in repaired.get("partial", {}).get("reasons", []):
            raise AssertionError("Repair failed to report the damaged object")
        backups = list(store.root.glob("index.md.conflict-*"))
        if len(backups) != 1 or backups[0].read_text() != edit:
            raise AssertionError("Repair did not preserve the owner's edit")
        store.capture("write after index repair")
        plan = store.call("forget plan", {"refs": [damaged]})["data"]
        store.call("record-use", {})
        forget = store.request({"plan": plan}, store.status()["generation"])
        committed = store.execute("forget apply", forget)
        if damaged_file.exists():
            raise AssertionError("A damaged object survived its confirmed forget")
        store.call("record-use", {})
        replay = store.execute("forget apply", forget)
        receipt = store.execute("status", store.request({"lookupRequestId": forget["requestId"]}))["data"]["result"]
        if replay["generation"] != committed["generation"] or replay["data"].get("purgePending") or receipt.get("purgePending"):
            raise AssertionError("A completed forget replay changed its generation or lost physical completion")
        return {
            "processLossAfterHead": "passed", "durablePurgeResumed": "passed",
            "unrelatedKnowledgePreserved": "passed", "damagedNoteIsolation": "passed",
            "editedViewPreserved": "passed", "writeAfterRepair": "passed",
            "forgetAcrossUnrelatedWrite": "passed", "forgetReceiptReplay": "passed",
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", type=Path, required=True, help="installed cetirun-memory executable")
    parser.add_argument("--work-dir", type=Path, required=True, help="existing directory for a disposable synthetic store")
    args = parser.parse_args()
    if sys.platform != "linux":
        parser.error("Linux inotify is required for the process interruption check")
    for name, path in (("helper", args.helper), ("work-dir", args.work_dir)):
        if not path.is_absolute() or path.resolve() != path or path == Path("/"):
            parser.error(f"{name} must be a canonical absolute path other than /")
    if not args.helper.is_file() or not os.access(args.helper, os.X_OK) or not args.work_dir.is_dir():
        parser.error("helper must be executable and work-dir must exist")
    print(json.dumps(run_checks(args.helper, args.work_dir), sort_keys=True))


if __name__ == "__main__":
    main()
