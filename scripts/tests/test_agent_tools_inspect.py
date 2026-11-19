# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Synthetic, offline tests for agent-tools-inspect.py."""

import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "agent-tools-inspect.py"
SPEC = importlib.util.spec_from_file_location("agent_tools_inspect", SCRIPT)
assert SPEC and SPEC.loader
inspect = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(inspect)


CENTRAL = "cetirun-credentials"
NAMESPACE_A = "agent-a-ns"
NAMESPACE_B = "agent-b-ns"
UID_A = "11111111-1111-1111-1111-111111111111"
UID_B = "22222222-2222-2222-2222-222222222222"


def rows(*values):
    return "\n".join("\t".join(value) for value in values) + ("\n" if values else "")


def record(name, record_id, tenant="tenant", owner="owner", *, namespace=CENTRAL,
           record_type="connection", provider="", account_id="", mode="", instance_uid="",
           connection_id="", instance_name="", instance_namespace=""):
    return inspect.SecretRecord(
        name, record_id, tenant, owner, provider, account_id, mode, instance_uid,
        connection_id, instance_name, instance_namespace, namespace, record_type,
    )


class AgentToolsInspectTests(unittest.TestCase):
    def test_shared_central_connection_uses_local_references_without_duplicate_credentials(self):
        agents = [inspect.Agent("agent-a", UID_A, NAMESPACE_A), inspect.Agent("agent-b", UID_B, NAMESPACE_B)]
        central_connection = record("agent-tools-connection-github", "github", provider="github", account_id="100", mode="delegated")
        refs = [
            record("agent-tools-connection-github", "github", namespace=NAMESPACE_A, record_type="connection-reference", provider="github", mode="delegated", instance_uid=UID_A, connection_id="github"),
            record("agent-tools-connection-github", "github", namespace=NAMESPACE_B, record_type="connection-reference", provider="github", mode="delegated", instance_uid=UID_B, connection_id="github"),
        ]
        bindings = [
            record("agent-tools-binding-a", "binding-a", namespace=NAMESPACE_A, record_type="binding", instance_uid=UID_A, connection_id="github"),
            record("agent-tools-binding-b", "binding-b", namespace=NAMESPACE_B, record_type="binding", instance_uid=UID_B, connection_id="github"),
        ]

        result = inspect.inventory(agents, CENTRAL, [central_connection], bindings, refs, set(), "all live Agent namespaces queried", [])

        self.assertEqual(len(result["connections"]), 1)
        self.assertEqual(result["connections"][0]["credentialStorage"], "central")
        self.assertEqual({consumer["name"] for consumer in result["connections"][0]["bindingConsumers"]}, {"agent-a", "agent-b"})
        self.assertEqual(len(result["connectionReferences"]), 2)
        self.assertTrue(all(item["credentialStorage"] == "none/reference" for item in result["connectionReferences"]))
        self.assertTrue(all(binding["connection"]["resolution"] == "referenced-central/present" for binding in result["bindings"]))
        self.assertIn("CONNECTION REFERENCES (credential-free, non-authoritative)", inspect.render_table(result))

    def test_owner_mismatch_does_not_link_reference_or_central_credential(self):
        agents = [inspect.Agent("agent-a", UID_A, NAMESPACE_A)]
        central_connection = record("central", "github", owner="owner-a", provider="github", mode="delegated")
        reference = record("reference", "github", owner="owner-b", namespace=NAMESPACE_A, record_type="connection-reference", instance_uid=UID_A, connection_id="github")
        binding = record("binding", "binding", owner="owner-b", namespace=NAMESPACE_A, record_type="binding", instance_uid=UID_A, connection_id="github")

        result = inspect.inventory(agents, CENTRAL, [central_connection], [binding], [reference], set(), "all live Agent namespaces queried", [])

        self.assertEqual(result["connections"][0]["bindingConsumers"], [])
        self.assertEqual(result["connectionReferences"][0]["centralConnection"]["resolution"], "owner-mismatch")
        self.assertEqual(result["bindings"][0]["connection"]["resolution"], "referenced-central/owner-mismatch")

    def test_central_binding_cannot_resolve_as_agent_authority(self):
        agents = [inspect.Agent("agent-a", UID_A, NAMESPACE_A)]
        central_connection = record("central", "github", provider="github")
        central_binding = record("legacy-binding", "legacy", record_type="binding", instance_uid=UID_A, connection_id="github")

        result = inspect.inventory(agents, CENTRAL, [central_connection], [central_binding], [], set(), "all live Agent namespaces queried", [])

        self.assertIsNone(result["bindings"][0]["connection"]["secretNamespace"])
        self.assertEqual(result["bindings"][0]["connection"]["resolution"], "invalid-binding-namespace")
        self.assertEqual(result["connections"][0]["relation"], "unassigned/unknown")

    def test_same_record_id_in_different_namespaces_does_not_collide(self):
        agents = [inspect.Agent("agent-a", UID_A, NAMESPACE_A)]
        central = record("central-github", "shared", provider="github", mode="delegated")
        local = record("local-azure", "shared", namespace=NAMESPACE_A, provider="azure", mode="agent", instance_uid=UID_A)
        binding = record("local-binding", "binding", namespace=NAMESPACE_A, record_type="binding", instance_uid=UID_A, connection_id="shared")

        result = inspect.inventory(agents, CENTRAL, [central, local], [binding], [], set(), "all live Agent namespaces queried", [])

        self.assertEqual(len(result["connections"]), 2)
        self.assertEqual(result["bindings"][0]["connection"]["secretName"], "local-azure")
        self.assertEqual(result["bindings"][0]["connection"]["secretNamespace"], NAMESPACE_A)
        central_row = next(item for item in result["connections"] if item["secretNamespace"] == CENTRAL)
        self.assertEqual(central_row["bindingConsumers"], [])

    def test_agent_filter_does_not_reassociate_recreated_name(self):
        agents = [inspect.Agent("recreated", UID_A, NAMESPACE_A)]
        central = record("central", "github", provider="github", mode="delegated")
        stale_ref = record("reference", "github", namespace=NAMESPACE_A, record_type="connection-reference", instance_uid=UID_B, connection_id="github", instance_name="recreated")
        stale_binding = record("binding", "old", namespace=NAMESPACE_A, record_type="binding", instance_uid=UID_B, connection_id="github", instance_name="recreated")
        result = inspect.inventory(agents, CENTRAL, [central], [stale_binding], [stale_ref], {UID_A}, "selected live Agent namespace plus central namespace queried", [])
        self.assertEqual(result["bindings"], [])
        self.assertEqual(result["connections"], [])

    def test_agent_filter_hides_unrelated_central_connection_from_other_owner(self):
        agents = [inspect.Agent("agent-a", UID_A, NAMESPACE_A), inspect.Agent("agent-b", UID_B, NAMESPACE_B)]
        connection_a = record("central-a", "github-a", owner="owner-a", provider="github", mode="delegated")
        connection_b = record("central-b", "github-b", owner="owner-b", provider="github", mode="delegated")
        reference_a = record("reference-a", "github-a", owner="owner-a", namespace=NAMESPACE_A,
                             record_type="connection-reference", instance_uid=UID_A, connection_id="github-a")
        reference_b = record("reference-b", "github-b", owner="owner-b", namespace=NAMESPACE_B,
                             record_type="connection-reference", instance_uid=UID_B, connection_id="github-b")
        binding_a = record("binding-a", "binding-a", owner="owner-a", namespace=NAMESPACE_A,
                         record_type="binding", instance_uid=UID_A, connection_id="github-a")
        binding_b = record("binding-b", "binding-b", owner="owner-b", namespace=NAMESPACE_B,
                         record_type="binding", instance_uid=UID_B, connection_id="github-b")

        result = inspect.inventory(
            agents, CENTRAL, [connection_a, connection_b], [binding_a, binding_b], [reference_a, reference_b],
            {UID_A}, "selected live Agent namespace plus central namespace queried", [],
        )

        self.assertEqual([item["secretName"] for item in result["connections"]], ["central-a"])
        self.assertEqual([item["secretName"] for item in result["connectionReferences"]], ["reference-a"])
        self.assertEqual([item["secretName"] for item in result["bindings"]], ["binding-a"])

    def test_kubectl_uses_metadata_projection_explicit_target_and_discards_stderr(self):
        client = inspect.Kubectl("/tmp/explicit-config", "chosen")
        with patch.object(inspect.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "")) as run:
            client.metadata_list(
                "secrets", CENTRAL, inspect.PREFIX + "record-type=binding",
                ("name", "id", "mode", "instance_uid", "instance_name_snapshot"),
            )
        command = run.call_args.args[0]
        self.assertEqual(command[:8], ["kubectl", "--kubeconfig", "/tmp/explicit-config", "--context", "chosen", "--request-timeout=15s", "get", "secrets"])
        self.assertIn("--namespace", command)
        self.assertIn("--selector", command)
        output = command[command.index("--output") + 1]
        self.assertIn(".metadata.name", output)
        self.assertIn("agenttools\\.cetirun\\.microsoft\\.com/id", output)
        self.assertIn("agenttools\\.cetirun\\.microsoft\\.com/mode", output)
        self.assertIn("agenttools\\.cetirun\\.microsoft\\.com/instance-uid", output)
        self.assertIn(".metadata.annotations.agenttools\\.cetirun\\.microsoft\\.com/instance-name", output)
        self.assertNotIn(".data", output)
        kwargs = run.call_args.kwargs
        self.assertIs(kwargs["stderr"], subprocess.DEVNULL)
        self.assertEqual(kwargs["env"]["KUBECTL_KUBERC"], "false")

    def test_agent_filter_queries_only_central_and_selected_namespace(self):
        agent_output = rows(("agent-a", UID_A, NAMESPACE_A), ("agent-b", UID_B, NAMESPACE_B))
        empty = subprocess.CompletedProcess([], 0, "")
        responses = [subprocess.CompletedProcess([], 0, agent_output), empty, empty, empty, empty, empty]
        with patch.object(inspect.subprocess, "run", side_effect=responses) as run, patch("sys.stdout") as stdout:
            code = inspect.main(["--kubeconfig", "/tmp/test", "--agent", "agent-a", "--output", "json"])
        self.assertEqual(code, 0)
        rendered = "".join(call.args[0] for call in stdout.write.call_args_list)
        payload = json.loads(rendered)
        self.assertEqual(payload["scope"]["referenceCoverage"], "selected live Agent namespace plus central namespace queried")
        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(len(commands), 5)
        namespaces = [command[command.index("--namespace") + 1] for command in commands[1:]]
        self.assertEqual(namespaces[:1], [CENTRAL])
        self.assertEqual(namespaces[1:], [NAMESPACE_A, NAMESPACE_A, NAMESPACE_A])
        self.assertTrue(all("--all-namespaces" not in command and "-A" not in command for command in commands))

    def test_unfiltered_inventory_skips_pending_agent_and_reports_it(self):
        agent_output = rows(("ready-agent", UID_A, NAMESPACE_A), ("pending-agent", UID_B, ""))
        empty = subprocess.CompletedProcess([], 0, "")
        responses = [subprocess.CompletedProcess([], 0, agent_output), empty, empty, empty, empty, empty]
        with patch.object(inspect.subprocess, "run", side_effect=responses) as run, patch("sys.stdout") as stdout:
            code = inspect.main(["--kubeconfig", "/tmp/test", "--output", "json"])
        self.assertEqual(code, 0)
        payload = json.loads("".join(call.args[0] for call in stdout.write.call_args_list))
        self.assertEqual(payload["scope"]["unqueriedAgents"], [{
            "uid": UID_B, "name": "pending-agent", "reason": "status.namespace is empty",
        }])
        self.assertIn("Pending/no-namespace", payload["scope"]["referenceCoverage"])
        commands = [call.args[0] for call in run.call_args_list]
        namespaces = [command[command.index("--namespace") + 1] for command in commands[1:]]
        self.assertEqual(namespaces, [CENTRAL, NAMESPACE_A, NAMESPACE_A, NAMESPACE_A])

    def test_pending_selected_agent_reports_incomplete_scope_and_queries_central_only(self):
        agent_output = rows(("ready-agent", UID_A, NAMESPACE_A), ("pending-agent", UID_B, ""))
        empty = subprocess.CompletedProcess([], 0, "")
        responses = [subprocess.CompletedProcess([], 0, agent_output), empty, empty]
        with patch.object(inspect.subprocess, "run", side_effect=responses) as run, patch("sys.stdout") as stdout:
            code = inspect.main(["--kubeconfig", "/tmp/test", "--agent", "pending-agent", "--output", "json"])
        self.assertEqual(code, 0)
        payload = json.loads("".join(call.args[0] for call in stdout.write.call_args_list))
        self.assertEqual(payload["scope"]["referenceCoverage"], "central namespace only; selected live Agent has no workload namespace")
        self.assertEqual(payload["scope"]["unqueriedAgents"][0]["uid"], UID_B)
        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(len(commands), 2)
        self.assertEqual([command[command.index("--namespace") + 1] for command in commands[1:]], [CENTRAL])

    def test_missing_or_forbidden_agent_namespace_is_not_treated_as_empty(self):
        agent_output = rows(("agent-a", UID_A, NAMESPACE_A))
        responses = [
            subprocess.CompletedProcess([], 0, agent_output),
            subprocess.CompletedProcess([], 0, ""), subprocess.CompletedProcess([], 0, ""),
            subprocess.CompletedProcess([], 1, "", "forbidden"),
        ]
        with patch.object(inspect.subprocess, "run", side_effect=responses), patch("sys.stderr") as stderr:
            code = inspect.main(["--kubeconfig", "/tmp/test", "--agent", "agent-a"])
        self.assertEqual(code, 1)
        message = "".join(call.args[0] for call in stderr.write.call_args_list)
        self.assertIn("kubectl metadata query failed", message)
        self.assertNotIn("forbidden", message)


if __name__ == "__main__":
    unittest.main()
