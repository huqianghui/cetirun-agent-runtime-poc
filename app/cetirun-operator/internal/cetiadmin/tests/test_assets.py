# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Static packaging/example checks, not qualification of operations prose."""

import json
from pathlib import Path
import re
import shlex
import unittest


ASSETS = Path(__file__).resolve().parents[1] / "assets"
ENTRYPOINTS = (
    "skills/cetiadmin/SKILL.md",
    "cron/cetirun-monitor-cron.md",
)


def code_blocks(text, language):
    return re.findall(r"^```" + language + r"\n(.*?)^```", text, flags=re.MULTILINE | re.DOTALL)


class AssetContractTest(unittest.TestCase):
    def test_entrypoints_exist_and_documents_are_nonempty(self):
        for relative in ENTRYPOINTS:
            self.assertTrue((ASSETS / relative).is_file(), relative)
        for path in ASSETS.rglob("*.md"):
            with self.subTest(path=path.relative_to(ASSETS)):
                self.assertFalse(path.is_symlink())
                self.assertTrue(path.read_text(encoding="utf-8").strip())

    def test_reference_links_are_portable(self):
        for path in ASSETS.rglob("*.md"):
            text = path.read_text()
            for link in re.findall(r"\[[^\]]+\]\(([^)]+)\)", text):
                with self.subTest(path=path.relative_to(ASSETS), link=link):
                    filename, _, fragment = link.partition("#")
                    self.assertNotIn("://", filename)
                    self.assertFalse(Path(filename).is_absolute())
                    target = (path.parent / filename).resolve() if filename else path
                    self.assertTrue(target.is_relative_to(ASSETS))
                    self.assertTrue(target.is_file())
                    if fragment:
                        headings = re.findall(r"^#+ (.+)$", target.read_text(), re.MULTILINE)
                        anchors = {
                            re.sub(r"[^a-z0-9 -]", "", heading.lower()).replace(" ", "-")
                            for heading in headings
                        }
                        self.assertIn(fragment, anchors)

    def test_json_examples_parse_and_audit_has_all_required_fields(self):
        examples = []
        for path in ASSETS.rglob("*.md"):
            for block in code_blocks(path.read_text(), "json"):
                with self.subTest(path=path.relative_to(ASSETS)):
                    examples.append(json.loads(block))
        audit = next(item for item in examples if isinstance(item, dict) and "actor" in item)
        self.assertEqual(set(audit), {
            "timestamp", "actor", "action", "resource_type", "resource_name", "namespace",
            "details", "result", "automation_tier", "request_id",
        })
        self.assertEqual(audit["actor"], "cetiadmin")
        monitor = next(item for item in examples if isinstance(item, dict) and "monitor" in item)
        self.assertEqual(set(monitor["instance"]), {"name", "namespace", "uid"})
        self.assertEqual(set(monitor["monitor"]), {
            "enabled", "schedule", "timezone", "deliver", "slackChannel", "slackSecretName",
        })
        issue_state = next(item for item in examples if isinstance(item, dict) and "knownIssues" in item)
        self.assertEqual(issue_state["pendingDeliveries"], {})
        self.assertTrue(all(not issue["slackSent"] for issue in issue_state["knownIssues"].values()))

    def test_command_examples_select_context_and_history_preserving_rollback(self):
        helm_rollbacks = 0
        kubectl_commands = 0
        for path in ASSETS.rglob("*.md"):
            for block in code_blocks(path.read_text(), "sh"):
                for line in block.replace("\\\n", " ").splitlines():
                    args = shlex.split(line)
                    if not args or args[0] not in {"kubectl", "helm"}:
                        continue
                    with self.subTest(path=path.relative_to(ASSETS), command=line):
                        context_flag = "--context" if args[0] == "kubectl" else "--kube-context"
                        self.assertIn(context_flag, args)
                        self.assertEqual(args[args.index(context_flag) + 1], "cetiadmin")
                        self.assertNotIn("--force", args)
                        if args[0] == "kubectl":
                            kubectl_commands += 1
                        if args[:2] == ["helm", "rollback"]:
                            helm_rollbacks += 1
                            for flag in ("--no-hooks", "--wait=false", "--history-max=0"):
                                self.assertIn(flag, args)
        self.assertGreater(kubectl_commands, 0)
        self.assertGreater(helm_rollbacks, 0)


if __name__ == "__main__":
    unittest.main()
