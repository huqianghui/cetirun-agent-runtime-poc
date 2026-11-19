# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""CetiAdmin deployment inputs and stop gates, with no target API access."""

import copy
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import unittest
from unittest.mock import patch

from scripts.tests import test_full_deployment as full


DEPLOY = full.DEPLOY


def admin_values(config):
    return {
        "enabled": True,
        "instanceName": "cetiadmin-hermes",
        "owner": {"alias": config["ownerAlias"], "tenantID": config["identity"]["tenantId"],
                  "objectID": "88888888-8888-8888-8888-888888888888"},
        "platformReadNamespaces": ["cetirun-system"],
        "kubernetes": {"apiServer": "https://kubernetes.default.svc", "apiAudience": "https://kubernetes.default.svc.cluster.local",
                       "caConfigMapName": "kube-root-ca.crt", "apiEgressCIDRs": ["192.0.2.10/32", "2001:db8::10/128"]},
        "model": {"default": "auto", "initialMonthlyBudgetUSD": "100", "githubCopilot": False},
        "sharedSkills": {"enabled": True},
        "monitor": {"enabled": True, "schedule": "every 30m", "timezone": "Asia/Singapore", "deliver": "slack",
                    "slackChannel": "C012EXAMPLE", "slackSecretName": "cetiadmin-monitor-slack"},
        "azureWorkloadIdentity": {"enabled": True, "clientId": "99999999-9999-9999-9999-999999999999"},
        "migrationSource": {"agent": {"name": "old-operations", "uid": "old-agent-uid"},
                            "namespace": {"name": "cetirun-old-operations", "uid": "old-namespace-uid"}},
    }


class CetiAdminInputTests(full.DeploymentFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.admin = admin_values(self.config)
        self.config["platform"]["values"]["cetirun-operator"]["cetiadmin"] = self.admin

    def test_published_environment_example_enables_cetiadmin(self):
        config = full.yaml.safe_load(full.FIXTURE.read_text())
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
            _, values = self.configure(config)
        self.assertIs(values["platform"]["cetirun-operator"]["cetiadmin"]["enabled"], True)

    def test_legacy_scope_is_rejected_before_tools_even_when_disabled(self):
        for enabled in (True, False):
            for scope in (None, {}, {"mode": "all-managed-agents"}, {"mode": "teams", "teamNames": ["platform-ops"]}):
                with self.subTest(enabled=enabled, scope=scope):
                    config = copy.deepcopy(self.config)
                    admin = config["platform"]["values"]["cetirun-operator"]["cetiadmin"]
                    admin.pop("platformReadNamespaces")
                    admin.update(enabled=enabled, scope=scope)
                    with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("called before validation")):
                        with self.assertRaisesRegex(ValueError, "unsupported.*fields"):
                            self.configure(config)

    def test_default_configuration_has_no_agent_allowlist(self):
        self.admin.pop("platformReadNamespaces")
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
            _, values = self.configure(self.config)
        admin = values["platform"]["cetirun-operator"]["cetiadmin"]
        for field in ("scope", "teamNames", "targets", "accessHash"):
            self.assertNotIn(field, admin)
        self.assertEqual(admin["platformReadNamespaces"], [])

    def test_instance_name_fits_helm_release_before_tools(self):
        for enabled in (True, False):
            for length in (44, 45, 55):
                with self.subTest(enabled=enabled, length=length):
                    config = copy.deepcopy(self.config)
                    admin = config["platform"]["values"]["cetirun-operator"]["cetiadmin"]
                    admin.update(enabled=enabled, instanceName="a" * length)
                    with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
                        if length == 44:
                            _, values = self.configure(config)
                            self.assertEqual(values["platform"]["cetirun-operator"]["cetiadmin"]["instanceName"], "a" * length)
                        else:
                            with self.assertRaisesRegex(ValueError, "cetiadmin.instanceName"):
                                self.configure(config)

    def test_legacy_wire_format_is_only_an_explicit_disabled_upgrade(self):
        self.admin.update(enabled=False, legacyDisabledSource=True)
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
            _, values = self.configure(self.config)
        actual = values["platform"]["cetirun-operator"]["cetiadmin"]
        self.assertTrue(actual["legacyDisabledSource"])
        without_flag = copy.deepcopy(actual)
        without_flag.pop("legacyDisabledSource")
        self.assertEqual(DEPLOY.cetiadmin_config_hash(actual), DEPLOY.cetiadmin_config_hash(without_flag))
        for enabled, flag in ((True, True), (False, "true"), (False, None)):
            with self.subTest(enabled=enabled, flag=flag):
                self.admin.update(enabled=enabled, legacyDisabledSource=flag)
                with self.assertRaisesRegex(ValueError, "legacyDisabledSource"):
                    self.configure(self.config)

    def test_platform_read_namespaces_are_top_level_diagnostics_only(self):
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
            _, values = self.configure(self.config)
        admin = values["platform"]["cetirun-operator"]["cetiadmin"]
        self.assertEqual(admin["platformReadNamespaces"], ["cetirun-system"])
        self.assertNotIn("scope", admin)

    def test_platform_diagnostics_require_namespace_labels(self):
        for value in ("platform", None, [42], [""], ["Wrong_Name"], ["platform.example"], ["a" * 64]):
            with self.subTest(value=value):
                self.admin["platformReadNamespaces"] = value
                with self.assertRaisesRegex(ValueError, "cetiadmin.platformReadNamespaces"):
                    self.configure(self.config)

    def test_fields_are_preserved_without_an_extra_chart_or_image(self):
        before = copy.deepcopy(self.config)
        with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("configuration ran a command")):
            _, values = self.configure(self.config)
        actual = values["platform"]["cetirun-operator"]["cetiadmin"]
        for key, value in self.admin.items():
            self.assertEqual(actual[key], value)
        self.assertEqual(self.config, before)
        self.assertNotIn("cetiadmin", values["platform"])
        self.assertEqual(set(DEPLOY.CHARTS), {"platform", "broker", "istio"})

    def test_owner_budget_and_api_inputs_are_required_before_tools(self):
        for field in ("owner.alias", "owner.tenantID", "owner.objectID", "model.initialMonthlyBudgetUSD",
                      "kubernetes.apiAudience", "kubernetes.apiEgressCIDRs"):
            with self.subTest(field=field):
                config = copy.deepcopy(self.config)
                parent = config["platform"]["values"]["cetirun-operator"]["cetiadmin"]
                keys = field.split(".")
                parent[keys[0]].pop(keys[1])
                with patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("called before validation")):
                    with self.assertRaisesRegex(ValueError, "cetiadmin"):
                        self.configure(config)

    def test_missing_cetiadmin_block_is_not_silently_disabled(self):
        self.config["platform"]["values"]["cetirun-operator"].pop("cetiadmin")
        with self.assertRaisesRegex(ValueError, "cetiadmin"):
            self.configure(self.config)

    def test_owner_must_match_the_explicit_platform_owner_and_tenant(self):
        for field, value in (("alias", "other-owner"), ("tenantID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
                             ("objectID", "00000000-0000-0000-0000-000000000000"), ("objectID", "user@example.invalid")):
            with self.subTest(field=field):
                config = copy.deepcopy(self.config)
                config["platform"]["values"]["cetirun-operator"]["cetiadmin"]["owner"][field] = value
                with self.assertRaisesRegex(ValueError, "cetiadmin.owner"):
                    self.configure(config)

    def test_budget_is_an_explicit_nonnegative_decimal_string(self):
        for value in (None, True, 100, "-1", "1e3", "1.0000001", "NaN", "9" * 400):
            with self.subTest(value=type(value).__name__):
                config = copy.deepcopy(self.config)
                config["platform"]["values"]["cetirun-operator"]["cetiadmin"]["model"]["initialMonthlyBudgetUSD"] = value
                with self.assertRaisesRegex(ValueError, "cetiadmin.model.initialMonthlyBudgetUSD"):
                    self.configure(config)
        self.admin["model"]["initialMonthlyBudgetUSD"] = "0"
        self.configure(self.config)

    def test_api_address_scope_and_audience_are_not_widened(self):
        cases = [("apiServer", "http://kubernetes.default.svc"), ("apiServer", "https://other.example.invalid"),
                 ("apiServer", "https://user:sk-dummy@kubernetes.default.svc"),
                 ("apiAudience", "cetirun-connections-mcp"), ("apiEgressCIDRs", ["192.0.2.0/24"]),
                 ("apiEgressCIDRs", ["192.0.2.10"]),
                 ("apiEgressCIDRs", ["169.254.169.254/32"]), ("apiEgressCIDRs", ["127.0.0.1/32"]),
                 ("apiEgressCIDRs", ["::ffff:127.0.0.1/128"]), ("apiEgressCIDRs", ["::/128"])]
        for field, value in cases:
            with self.subTest(field=field, value=value):
                config = copy.deepcopy(self.config)
                admin = config["platform"]["values"]["cetirun-operator"]["cetiadmin"]
                admin["kubernetes"][field] = value
                if field == "apiAudience":
                    # Use the actual configured MCP audience, not a guessed default.
                    admin["kubernetes"][field] = DEPLOY.yaml_file(full.ROOT / "app/cetirun-operator/chart/values.yaml")["connectionsMCP"]["audience"]
                with self.assertRaisesRegex(ValueError, "cetiadmin.kubernetes"):
                    self.configure(config)

    def test_migration_source_requires_both_names_and_uids_and_a_new_target(self):
        for path, value in (("agent.uid", ""), ("namespace.uid", ""), ("namespace.name", ""),
                            ("agent.name", "cetiadmin-hermes"), ("agent.namespace", "wrong-namespace")):
            with self.subTest(path=path):
                config = copy.deepcopy(self.config)
                source = config["platform"]["values"]["cetirun-operator"]["cetiadmin"]["migrationSource"]
                DEPLOY.set_value(source, path, value)
                with self.assertRaisesRegex(ValueError, "cetiadmin.migrationSource"):
                    self.configure(config)

    def test_optional_inputs_fail_closed_and_never_accept_teams(self):
        for path, value in (("monitor.deliver", "teams"), ("monitor.timezone", "Invalid/Timezone"),
                            ("monitor.slackChannel", ""), ("monitor.slackSecretName", ""),
                            ("azureWorkloadIdentity.clientId", ""), ("runtime.version", "9.9.9"),
                            ("runtime.port", 18789), ("runtime.image", False), ("runtime.version", 0),
                            ("assets.image", "registry.example.com/other:1"),
                            ("teams.enabled", True), ("scope.mode", "cluster-admin"), ("teamNames", ["platform-ops"])):
            with self.subTest(path=path):
                config = copy.deepcopy(self.config)
                DEPLOY.set_value(config["platform"]["values"]["cetirun-operator"]["cetiadmin"], path, value)
                with self.assertRaisesRegex(ValueError, "cetiadmin"):
                    self.configure(config)

    def test_explicit_disabled_keeps_retained_fields_and_does_not_require_a_new_owner(self):
        self.admin["enabled"] = False
        _, values = self.configure(self.config)
        for key, value in self.admin.items():
            self.assertEqual(values["platform"]["cetirun-operator"]["cetiadmin"][key], value)
        self.config["platform"]["values"]["cetirun-operator"]["cetiadmin"] = {"enabled": False}
        _, values = self.configure(self.config)
        self.assertFalse(values["platform"]["cetirun-operator"]["cetiadmin"]["enabled"])

    def test_slack_reference_is_only_for_the_bound_agent_namespace(self):
        config, values = self.configure(self.config)
        deployment = DEPLOY.Deployment(config, values, self.root)
        refs = deployment.references()
        for key in ("SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"):
            self.assertIn(("Secret", "cetirun-cetiadmin-hermes", "cetiadmin-monitor-slack", key), refs)
            self.assertNotIn(("Secret", config["platform"]["namespace"], "cetiadmin-monitor-slack", key), refs)

    def test_removal_requires_explicit_cetiadmin_and_chat_disable(self):
        operator = self.config["platform"]["values"]["cetirun-operator"]
        operator["enabled"] = False
        with self.assertRaises(ValueError):
            self.configure(self.config)
        operator["cetiadmin"]["enabled"] = False
        with self.assertRaises(ValueError):
            self.configure(self.config)
        self.config["platform"]["values"]["global"] = {"portalChat": {"enabled": False}}
        _, values = self.configure(self.config)
        self.assertFalse(values["platform"]["cetirun-operator"]["enabled"])

    def test_upgrade_cannot_remove_operator_without_current_stop_evidence(self):
        self.admin["enabled"] = False
        config, values = self.configure(self.config)
        values["platform"]["cetirun-operator"]["enabled"] = False
        deployment = full.SyntheticCluster(config, values, self.root)
        deployment.charts["platform"] = self.root
        deployment.files["platform"] = self.root / "remove.json"
        deployment.files["platform"].write_text(json.dumps(values["platform"]))
        deployment.documents["platform"] = []
        with self.assertRaisesRegex(ValueError, "CetiAdmin|cetiadmin"):
            deployment.upgrade("platform")
        self.assertEqual(deployment.writes, [])

    def test_hash_matches_the_go_config_json_contract(self):
        encoded = ('{"enabled":false,"instanceName":"cetiadmin","owner":{"alias":"","tenantID":"","objectID":""},'
                   '"kubernetes":{"apiServer":"https://kubernetes.default.svc",'
                   '"apiAudience":"","caConfigMapName":"kube-root-ca.crt","apiEgressCIDRs":null},'
                   '"model":{"default":"auto","initialMonthlyBudgetUSD":"","githubCopilot":false},"runtime":{},'
                   '"azureWorkloadIdentity":{"enabled":false},"sharedSkills":{"enabled":false},'
                   '"monitor":{"enabled":false,"timezone":"UTC","deliver":"local"}}')
        self.assertEqual(DEPLOY.cetiadmin_config_hash({"enabled": False}), hashlib.sha256(encoded.encode()).hexdigest())
        for namespaces in ([], None, ["kube-system"]):
            with self.subTest(namespaces=namespaces):
                expected = encoded.replace('"kubernetes":', '"platformReadNamespaces":["kube-system"],"kubernetes":', 1) if namespaces else encoded
                actual = DEPLOY.cetiadmin_config_hash({"platformReadNamespaces": namespaces, "enabled": False})
                self.assertEqual(actual, hashlib.sha256(expected.encode()).hexdigest())

    def test_hash_rejects_legacy_scope_and_untyped_diagnostics(self):
        for scope in (None, {}, {"mode": "all-managed-agents"}, {"mode": "teams", "teamNames": ["platform-ops"]}):
            with self.subTest(scope=scope):
                with self.assertRaisesRegex(ValueError, "unsupported fields"):
                    DEPLOY.cetiadmin_config_hash({"enabled": False, "scope": scope})
        for value in ("platform", False, 0, [42]):
            with self.subTest(value=value):
                with self.assertRaisesRegex(ValueError, "platformReadNamespaces"):
                    DEPLOY.cetiadmin_config_hash({"enabled": False, "platformReadNamespaces": value})

    def test_invalid_cetiadmin_cli_output_never_echoes_the_input_value(self):
        marker = "sk-dummy-must-not-be-printed"
        self.admin["model"]["initialMonthlyBudgetUSD"] = marker
        path = self.root / "invalid-admin.json"
        path.write_text(json.dumps(self.config))
        with patch("sys.stdout", new_callable=io.StringIO) as stdout, patch("sys.stderr", new_callable=io.StringIO) as stderr, \
                patch.object(DEPLOY.subprocess, "run", side_effect=AssertionError("called before input validation")):
            self.assertEqual(DEPLOY.main(["--config", str(path), "--plan"]), 1)
        self.assertNotIn(marker, stdout.getvalue() + stderr.getvalue())


class StopDouble(DEPLOY.Deployment):
    """Serve only the narrow kubectl projections used by the stop check."""

    def __init__(self, config, values, scratch):
        super().__init__(config, values, scratch)
        self.commands, self.writes = [], []
        self.fail = ""
        admin = copy.deepcopy(values["platform"]["cetirun-operator"]["cetiadmin"])
        admin.pop("hookTimeoutSeconds")
        admin["runtime"]["version"] = config["release"]["version"]
        self.namespace = config["platform"]["namespace"]
        self.agent_namespace = "cetirun-" + admin["instanceName"]
        self.source_name = "cetirun-cetiadmin-config"
        self.binding_name = "cetiadmin-binding-" + hashlib.sha256((self.namespace + "/" + self.source_name).encode()).hexdigest()[:24]
        self.state = {
            "schemaVersion": 1, "source": {"name": self.source_name, "namespace": self.namespace, "uid": "source-uid"},
            "configHash": DEPLOY.cetiadmin_config_hash(admin), "configuration": admin, "phase": "Disabled", "hold": True,
            "recordUID": "record-uid", "agent": {"name": admin["instanceName"], "uid": "agent-uid"},
            "namespace": {"name": self.agent_namespace, "uid": "namespace-uid"},
            "serviceAccount": {"name": "cetiadmin-account", "namespace": self.agent_namespace, "uid": "account-uid"},
            "team": {"name": admin["instanceName"], "uid": "team-uid"}, "bootstrapped": True,
            "operatorImage": "registry.example.com/cetirun-operator:0.1.1", "runtimeVersion": "0.1.1",
        }
        self.objects = {}
        def add(kind, identity, **fields):
            metadata = {"resourceVersion": "1", "generation": 3, **identity}
            value = {"metadata": metadata, **fields}
            self.objects[(kind, identity.get("namespace", ""), identity["name"])] = value
            return value
        self.source = add("configmap", self.state["source"], data={"config.json": json.dumps(admin, sort_keys=True)})
        self.source["metadata"]["annotations"] = {"meta.helm.sh/release-name": config["platform"]["release"],
            "meta.helm.sh/release-namespace": self.namespace, "helm.sh/resource-policy": "keep"}
        self.checkpoint = add("configmap", {"name": self.binding_name, "namespace": self.namespace, "uid": "record-uid"})
        self.checkpoint["metadata"]["annotations"] = {"cetirun.microsoft.com/cetiadmin-source-uid": "source-uid"}
        self.agent = add("agent", self.state["agent"], spec={"suspended": True}, status={"phase": "Suspended"})
        ns = add("namespace", self.state["namespace"])
        sa = add("serviceaccount", self.state["serviceAccount"])
        owner = {"apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Agent", "name": admin["instanceName"], "uid": "agent-uid"}
        for document in (ns, sa):
            document["metadata"]["ownerReferences"] = [owner.copy()]
        self.operator = add("deployment", {"name": "cetirun-operator", "namespace": self.namespace, "uid": "operator-uid"},
            spec={"replicas": 1, "template": {"spec": {"containers": [{"name": "manager", "image": self.state["operatorImage"],
                "env": [{"name": "CETIRUN_CETIADMIN_CONFIG_MAP", "value": self.source_name}]}]}}},
            status={"observedGeneration": 3, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1})
        self.lists = {kind: [] for kind in (*DEPLOY.CETIADMIN_WRITER_FIELDS, "rolebindings", "clusterrolebindings")}
        self.charts["platform"] = scratch
        self.files["platform"] = scratch / "removal.json"

    def run(self, command, *, env=None, input_text=None):
        self.commands.append(command)
        if command[:3] == ["helm", "--kube-context", self.config["target"]["context"]]:
            assert command[3:5] == ["upgrade", "--install"]
            self.writes.append(command)
            return "removed\n"
        assert command[:4] == ["kubectl", "--context", self.config["target"]["context"], "--request-timeout=15s"]
        args = command[4:]
        assert args[0] == "get", "stop checks must not mutate the cluster"
        if self.fail and self.fail in " ".join(args):
            raise ValueError("CetiAdmin synthetic read failure")
        kind = args[1].lower()
        namespace = args[args.index("--namespace") + 1] if "--namespace" in args else ""
        projection = args[args.index("-o") + 1]
        fields = re.findall(r"\{\.([^{}]+)\}", projection)
        allowed_fields = {"metadata", "metadata.namespace", "subjects", r"data.config\.json", r"data.binding\.json",
                          "metadata.generation",
                          "spec.replicas", "status", 'spec.template.spec.containers[?(@.name=="manager")].image',
                          'spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="CETIRUN_CETIADMIN_CONFIG_MAP")].value',
                          "spec.agent.type", "spec.helm.values.serviceAccount.name", "status.aiGateway",
                          *DEPLOY.CETIADMIN_AGENT_FIELDS}
        allowed_fields.update(r"metadata.annotations.cetirun\.microsoft\.com/helm-maintenance-" + key
                              for key in ("agent", "agent-uid", "request-id"))
        allowed_fields.update(field for group in DEPLOY.CETIADMIN_WRITER_FIELDS.values() for field in group)
        allowed_fields.update(DEPLOY.AGENT_ACCOUNT_FIELDS)
        assert fields and set(fields) <= allowed_fields, "unexpected or credential-bearing projection"
        def field_value(document, field):
            if field == r"data.binding\.json":
                return json.dumps(self.state)
            if field == r"data.config\.json":
                return document["data"]["config.json"]
            if 'containers[?(@.name=="manager")]' in field:
                manager = document["spec"]["template"]["spec"]["containers"][0]
                return manager["env"][0]["value"] if ".env[" in field else manager["image"]
            if field == "status.active[*].uid":
                return " ".join(item["uid"] for item in document.get("status", {}).get("active", []))
            if field.startswith("metadata.annotations."):
                return document["metadata"].get("annotations", {}).get(field.removeprefix("metadata.annotations.").replace(r"\.", "."))
            return DEPLOY.get_value(document, field)
        def text(value):
            if value is None:
                return ""
            return json.dumps(value) if isinstance(value, (dict, list, bool)) else str(value)
        if kind == "agents.cetirun.microsoft.com":
            return "".join("\t".join(text(field_value(document, field)) for field in fields) + "\n"
                           for (resource, _, _), document in self.objects.items() if resource == "agent")
        if kind == "namespaces":
            return "".join("\t".join(text(field_value(document, field)) for field in fields) + "\n"
                           for (resource, _, _), document in self.objects.items() if resource == "namespace")
        if "{range .items[*]}" in projection:
            if kind == "rolebindings":
                assert "--all-namespaces" in args
            elif kind != "clusterrolebindings":
                assert namespace == self.agent_namespace
            return "".join("\t".join(text(field_value(item, field)) for field in fields) + "\n" for item in self.lists[kind])
        document = self.objects.get((kind, namespace, args[2]))
        if projection == "jsonpath={.metadata}":
            return json.dumps(document["metadata"]) if document else ""
        assert document is not None
        return "\n".join(text(field_value(document, field)) for field in fields) + "\n"


class CetiAdminStopTests(full.DeploymentFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        config, values = self.configure(self.config)
        values["platform"]["cetirun-operator"]["enabled"] = False
        self.deployment = StopDouble(config, values, self.root)

    def test_confirmed_stop_allows_upgrade_but_never_calls_delete_or_patch(self):
        d = self.deployment
        d.check_cetiadmin_stopped()
        d.upgrade("platform")
        self.assertEqual(len(d.writes), 1)
        self.assertTrue(all(command[4] == "get" for command in d.commands if command[0] == "kubectl"))
        self.assertTrue(all("--no-hooks" not in command and "--force" not in command for command in d.commands))

    def test_confirmed_component_hold_preserves_running_user_intent(self):
        d = self.deployment
        d.agent["spec"]["suspended"] = False
        d.check_cetiadmin_stopped()
        self.assertIs(d.agent["spec"]["suspended"], False)

    def test_missing_source_checkpoint_or_operator_is_not_confirmation(self):
        for kind, namespace, name in (("configmap", self.deployment.namespace, self.deployment.source_name),
                                      ("configmap", self.deployment.namespace, self.deployment.binding_name),
                                      ("deployment", self.deployment.namespace, "cetirun-operator")):
            with self.subTest(kind=kind, name=name):
                key = kind, namespace, name
                removed = self.deployment.objects.pop(key)
                with self.assertRaises(ValueError):
                    self.deployment.upgrade("platform")
                self.assertEqual(self.deployment.writes, [])
                self.deployment.objects[key] = removed

    def test_confirmed_partial_installation_has_no_writer_to_start_or_delete(self):
        d = self.deployment
        d.state.update(namespace={}, serviceAccount={}, bootstrapped=False, resources=[])
        d.agent["status"]["phase"] = "Pending"
        d.check_cetiadmin_stopped()
        self.assertEqual(d.writes, [])

    def test_unconfirmed_state_and_unknown_writes_fail(self):
        for field, value in (("phase", "Stopping"), ("phase", "Blocked"), ("hold", False),
                             ("pendingCreate", "unknown"), ("creation", {"object": "unknown"}),
                             ("bootstrapReleasePending", True), ("accessHash", "still-granted"),
                             ("configHash", "0" * 64), ("recordUID", "replaced-record")):
            with self.subTest(field=field):
                old = copy.deepcopy(self.deployment.state)
                self.deployment.state[field] = value
                with self.assertRaises(ValueError):
                    self.deployment.upgrade("platform")
                self.assertEqual(self.deployment.writes, [])
                self.deployment.state = old

    def test_replaced_namespace_sa_or_agent_is_not_adopted(self):
        for kind, identity in (("agent", "agent"), ("namespace", "namespace"), ("serviceaccount", "serviceAccount")):
            with self.subTest(kind=kind):
                recorded = self.deployment.state[identity]
                document = self.deployment.objects[(kind, recorded.get("namespace", ""), recorded["name"])]
                original = document["metadata"]["uid"]
                document["metadata"]["uid"] = "replacement"
                with self.assertRaises(ValueError):
                    self.deployment.upgrade("platform")
                document["metadata"]["uid"] = original
                self.assertEqual(self.deployment.writes, [])

    def test_operator_rollout_and_retained_agent_must_be_stopped_now(self):
        for document, path, value in ((self.deployment.operator, "status.observedGeneration", 2),
                                      (self.deployment.operator, "status.readyReplicas", 0),
                                      (self.deployment.agent, "status.phase", "Running"),
                                      (self.deployment.agent, "status.accessUrl", "https://old.example.invalid")):
            with self.subTest(path=path):
                before = copy.deepcopy(document)
                DEPLOY.set_value(document, path, value)
                with self.assertRaises(ValueError):
                    self.deployment.upgrade("platform")
                document.clear()
                document.update(before)
                self.assertEqual(self.deployment.writes, [])

    def test_operator_watching_another_source_cannot_confirm_stop(self):
        manager = self.deployment.operator["spec"]["template"]["spec"]["containers"][0]
        manager["env"][0]["value"] = "another-source"
        with self.assertRaises(ValueError):
            self.deployment.upgrade("platform")
        self.assertEqual(self.deployment.writes, [])

    def test_all_native_writers_are_checked_not_only_pods(self):
        cases = {
            "pods": {"status": {"phase": "Running"}},
            "statefulsets": {"spec": {"replicas": 1}, "status": {"replicas": 0}},
            "deployments": {"spec": {"replicas": 0}, "status": {"replicas": 1}},
            "replicasets": {"spec": {}, "status": {"replicas": 0}},
            "daemonsets": {}, "jobs": {"status": {"active": 0}},
            "cronjobs": {"spec": {"suspend": False}},
        }
        for kind, item in cases.items():
            with self.subTest(kind=kind):
                self.deployment.lists[kind] = [{"metadata": {"uid": "writer-uid"}, **item}]
                with self.assertRaisesRegex(ValueError, "writers"):
                    self.deployment.upgrade("platform")
                self.deployment.lists[kind] = []
                self.assertEqual(self.deployment.writes, [])

    def test_remaining_rolebindings_and_user_subjects_block_removal(self):
        account = self.deployment.state["serviceAccount"]
        for kind in ("rolebindings", "clusterrolebindings"):
            for subject in ({"kind": "ServiceAccount", "name": account["name"], "namespace": account["namespace"]},
                            {"kind": "User", "name": "system:serviceaccount:" + account["namespace"] + ":" + account["name"]}):
                with self.subTest(kind=kind, subject_kind=subject["kind"]):
                    self.deployment.lists[kind] = [{"metadata": {"namespace": account["namespace"]}, "subjects": [subject]}]
                    with self.assertRaisesRegex(ValueError, "revoked"):
                        self.deployment.upgrade("platform")
                    self.deployment.lists[kind] = []
                    self.assertEqual(self.deployment.writes, [])

    def test_unrelated_rbac_and_completed_writers_are_preserved(self):
        d = self.deployment
        d.lists["rolebindings"] = [{"metadata": {"namespace": "another"}, "subjects": [{"kind": "ServiceAccount", "name": "other"}]}]
        d.lists["pods"] = [{"metadata": {"uid": "done"}, "status": {"phase": "Succeeded"}}]
        d.lists["jobs"] = [{"metadata": {"uid": "done-job"}, "status": {"conditions": [{"type": "Complete", "status": "True"}]}}]
        d.lists["statefulsets"] = [{"metadata": {"uid": "zero"}, "spec": {"replicas": 0}, "status": {"replicas": 0}}]
        d.lists["cronjobs"] = [{"metadata": {"uid": "paused"}, "spec": {"suspend": True}}]
        before = copy.deepcopy(d.lists)
        d.upgrade("platform")
        self.assertEqual(d.lists, before)

    def test_source_change_after_preflight_cannot_reuse_the_stop_receipt(self):
        self.deployment.check_cetiadmin_stopped()
        self.deployment.source["metadata"]["resourceVersion"] = "2"
        with self.assertRaisesRegex(ValueError, "changed since preflight"):
            self.deployment.upgrade("platform")
        self.assertEqual(self.deployment.writes, [])

    def test_stop_read_failure_is_not_skipped(self):
        self.deployment.fail = "get pods"
        with self.assertRaisesRegex(ValueError, "read failure"):
            self.deployment.upgrade("platform")
        self.assertEqual(self.deployment.writes, [])

    def test_unrecorded_maintenance_blocks_removal(self):
        d = self.deployment
        self.assertNotIn("targets", d.state)
        d.objects[("agent", "", "new-agent")] = {
            "metadata": {"name": "new-agent", "uid": "new-agent-uid", "generation": 2},
            "spec": {"suspended": True}, "status": {"helmMaintenance": {"phase": "Held", "observedGeneration": 2}},
        }
        with self.assertRaisesRegex(ValueError, "maintenance"):
            d.check_cetiadmin_stopped()
        self.assertEqual(d.writes, [])

    def test_orphaned_namespace_maintenance_marker_blocks_removal(self):
        d = self.deployment
        self.assertNotIn("targets", d.state)
        d.objects[("namespace", "", "cetirun-deleted-agent")] = {
            "metadata": {"name": "cetirun-deleted-agent", "uid": "orphan-uid",
                         "annotations": {"cetirun.microsoft.com/helm-maintenance-request-id": "unresolved"}},
        }
        with self.assertRaisesRegex(ValueError, "maintenance"):
            d.check_cetiadmin_stopped()
        self.assertEqual(d.writes, [])

    def test_current_maintenance_list_failure_blocks_removal(self):
        for kind in ("agents.cetirun.microsoft.com", "namespaces"):
            with self.subTest(kind=kind):
                self.deployment.fail = "get " + kind
                with self.assertRaisesRegex(ValueError, "read failure"):
                    self.deployment.check_cetiadmin_stopped()
                self.assertEqual(self.deployment.writes, [])

    def test_legacy_authorization_inventory_is_not_stop_evidence(self):
        for field in ("targets", "readNamespaces", "accessHash"):
            with self.subTest(field=field):
                self.deployment.state[field] = [] if field != "accessHash" else ""
                with self.assertRaisesRegex(ValueError, "checkpoint binding"):
                    self.deployment.check_cetiadmin_stopped()
                del self.deployment.state[field]
                self.assertEqual(self.deployment.writes, [])

    def test_maintenance_phase_request_and_namespace_markers_block_removal(self):
        d = self.deployment
        target = {"name": "maintenance-target", "uid": "target-uid"}
        namespace = {"name": "cetirun-maintenance-target", "uid": "target-namespace-uid"}
        target_document = {"metadata": {"resourceVersion": "1", "generation": 2, **target},
                           "spec": {"suspended": True}, "status": {"helmMaintenance": {"phase": "Released", "observedGeneration": 2}}}
        ns = {"metadata": {"resourceVersion": "1", **namespace}}
        d.objects[("agent", "", target["name"])] = target_document
        d.objects[("namespace", "", namespace["name"])] = ns
        for phase in ("Requested", "Held", "Reapplying", "Blocked"):
            target_document["status"]["helmMaintenance"]["phase"] = phase
            with self.assertRaisesRegex(ValueError, "maintenance"):
                d.upgrade("platform")
        target_document["status"]["helmMaintenance"]["phase"] = "Released"
        target_document["spec"]["helm"] = {"maintenance": {"requestID": "still-requested"}}
        with self.assertRaisesRegex(ValueError, "maintenance"):
            d.upgrade("platform")
        target_document["spec"].pop("helm")
        target_document["status"]["helmMaintenance"]["observedGeneration"] = 1
        with self.assertRaisesRegex(ValueError, "maintenance"):
            d.check_cetiadmin_stopped()
        target_document["status"]["helmMaintenance"]["observedGeneration"] = 2
        ns["metadata"]["annotations"] = {"cetirun.microsoft.com/helm-maintenance-agent-uid": "target-uid"}
        with self.assertRaisesRegex(ValueError, "maintenance"):
            d.upgrade("platform")
        self.assertEqual(d.writes, [])
        ns["metadata"].pop("annotations")
        d.upgrade("platform")
        self.assertEqual(len(d.writes), 1)

    def migration(self):
        d = self.deployment
        admin = d.values["platform"]["cetirun-operator"]["cetiadmin"]
        admin.update(enabled=True, instanceName="new-cetiadmin", migrationSource={
            "agent": d.state["agent"].copy(), "namespace": d.state["namespace"].copy()})
        d.agent["spec"].update(agent={"type": "openclaw"}, helm={"values": {"serviceAccount": {"name": "cetiadmin-account", "automountToken": True}}})
        d.agent["status"]["aiGateway"] = {"blocked": True, "routed": False}
        d.objects[("customresourcedefinition", "", "agents.cetirun.microsoft.com")] = {
            "metadata": {"name": "agents.cetirun.microsoft.com", "uid": "crd-uid", "resourceVersion": "1"}}
        return d

    def test_only_confirmed_stopped_migration_source_can_keep_its_legacy_sa(self):
        d = self.migration()
        d.check_cetiadmin_migration()
        d.check_agent_mcp_conflicts()
        self.assertEqual(d.cetiadmin_migration_receipt["uid"], "agent-uid")
        self.assertEqual(d.writes, [])
        d.agent["metadata"]["resourceVersion"] = "2"
        with self.assertRaisesRegex(ValueError, "changed after preflight"):
            d.check_agent_mcp_conflicts()

    def test_migration_refuses_uid_mismatch_active_model_and_remaining_grants(self):
        d = self.migration()
        for field, value in (("metadata.uid", "replacement"), ("spec.suspended", False),
                             ("status.aiGateway.blocked", False), ("status.aiGateway.routed", True)):
            with self.subTest(field=field):
                before = copy.deepcopy(d.agent)
                DEPLOY.set_value(d.agent, field, value)
                with self.assertRaises(ValueError):
                    d.check_cetiadmin_migration()
                self.assertIsNone(d.cetiadmin_migration_receipt)
                d.agent.clear()
                d.agent.update(before)
        d.lists["clusterrolebindings"] = [{"subjects": [{"kind": "ServiceAccount", "name": "cetiadmin-account", "namespace": d.agent_namespace}]}]
        with self.assertRaisesRegex(ValueError, "revoked"):
            d.check_cetiadmin_migration()
        self.assertEqual(d.writes, [])


@unittest.skipUnless(shutil.which("helm"), "requires existing Helm for offline chart rendering")
class CetiAdminDeploymentTests(full.DeploymentFixture, unittest.TestCase):
    def test_default_enabled_plan_renders_only_operator_source_and_existing_images(self):
        admin = admin_values(self.config)
        admin.pop("migrationSource")
        admin.pop("enabled")
        self.config["platform"]["values"]["cetirun-operator"]["cetiadmin"] = admin
        deployment = self.deployment()
        docs = deployment.documents["platform"]
        source = next(doc for doc in docs if doc["kind"] == "ConfigMap" and doc["metadata"]["name"] == "cetirun-cetiadmin-config")
        config = json.loads(source["data"]["config.json"])
        self.assertTrue(config["enabled"])
        self.assertEqual(config["owner"], admin["owner"])
        self.assertEqual(config["model"], admin["model"])
        self.assertEqual(config["platformReadNamespaces"], admin["platformReadNamespaces"])
        self.assertNotIn("scope", config)
        self.assertEqual(config["kubernetes"]["apiEgressCIDRs"], admin["kubernetes"]["apiEgressCIDRs"])
        self.assertFalse(any(doc["kind"] in ("Agent", "Team") for doc in docs))
        plan = deployment.plan()
        self.assertEqual(len(plan["artifacts"]["images"]), 14)
        self.assertEqual(len(plan["artifacts"]["charts"]), 9)
        self.assertEqual(plan["cetiadmin"], {"enabled": True, "operatorRemovalRequested": False, "stopConfirmed": False})
        encoded = json.dumps(plan)
        for private in (admin["owner"]["objectID"], admin["monitor"]["slackSecretName"], admin["azureWorkloadIdentity"]["clientId"]):
            self.assertNotIn(private, encoded)
        self.assertEqual(deployment.writes, [])

    @unittest.skipUnless(shutil.which("make"), "requires existing Make for the public deployment entry point")
    def test_make_deploy_plan_uses_the_same_config_without_live_commands(self):
        admin = admin_values(self.config)
        admin.pop("enabled")
        self.config["platform"]["values"]["cetirun-operator"]["cetiadmin"] = admin
        path = self.root / "environment.json"
        path.write_text(json.dumps(self.config))
        commands = self.root / "commands"
        commands.mkdir()
        for name in ("kubectl", "docker", "az"):
            command = commands / name
            command.write_text("#!/bin/sh\nexit 91\n")
            command.chmod(0o700)
        isolated_home = self.root / "home"
        isolated_home.mkdir()
        env = {"PATH": str(commands) + os.pathsep + os.environ["PATH"], "HOME": str(isolated_home),
               "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull, "PYTHONDONTWRITEBYTECODE": "1"}
        result = subprocess.run(["make", "--no-print-directory", "-s", "deploy-plan", "DEPLOY_CONFIG=" + str(path)],
                                cwd=full.ROOT, env=env, text=True, capture_output=True, timeout=45)
        self.assertEqual(result.returncode, 0, result.stderr)
        plan = json.loads(result.stdout)
        self.assertEqual(plan["cetiadmin"], {"enabled": True, "operatorRemovalRequested": False, "stopConfirmed": False})
        for private in (admin["owner"]["objectID"], admin["monitor"]["slackSecretName"], admin["azureWorkloadIdentity"]["clientId"]):
            self.assertNotIn(private, result.stdout + result.stderr)

    def test_disable_preserves_source_and_protection_during_normal_upgrade(self):
        admin = admin_values(self.config)
        admin.pop("migrationSource")
        admin["enabled"] = False
        self.config["platform"]["values"]["cetirun-operator"]["cetiadmin"] = admin
        deployment = self.deployment()
        resources = {(doc["kind"], doc["metadata"]["name"]): doc for doc in deployment.documents["platform"]}
        for identity in (("Deployment", "cetirun-operator"), ("Deployment", "cetirun-sandbox-admission"),
                         ("ValidatingWebhookConfiguration", "cetirun-cetiadmin-admission"),
                         ("MutatingWebhookConfiguration", "cetirun-cetiadmin-admission"),
                         ("Job", "cetirun-cetiadmin-check")):
            self.assertIn(identity, resources)
        source = resources[("ConfigMap", "cetirun-cetiadmin-config")]
        self.assertEqual(source["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertFalse(json.loads(source["data"]["config.json"])["enabled"])
        self.assertEqual(deployment.values["platform"]["cetirun-operator"]["cetiadmin"]["owner"], admin["owner"])

    def removal(self):
        self.config["platform"]["values"]["cetirun-operator"]["enabled"] = False
        self.config["platform"]["values"]["global"] = {"portalChat": {"enabled": False}}
        d = self.deployment()
        d.installed.add(self.config["platform"]["release"])
        for doc in d.documents["platform"]:
            if doc["kind"] == "Secret":
                name = doc["metadata"]["name"]
                d.objects[("secret", d.config["platform"]["namespace"], name)] = {"name": name, "uid": "secret-uid", "annotations": {
                    "meta.helm.sh/release-name": d.config["platform"]["release"], "meta.helm.sh/release-namespace": d.config["platform"]["namespace"]}}
                d.keys[(d.config["platform"]["namespace"], name)] = set(doc.get("data", {}))
        return d

    def test_missing_stop_checkpoint_fails_execute_before_any_write(self):
        d = self.removal()
        with self.assertRaisesRegex(ValueError, "CetiAdmin"):
            d.execute()
        self.assertEqual(d.writes, [])
        self.assertFalse(d.plan()["cetiadmin"]["stopConfirmed"])

    def test_confirmed_removal_is_checked_before_writes_and_before_helm_upgrade(self):
        d = self.removal()
        stop = StopDouble(d.config, d.values, self.root)
        normal_run = d.run
        d.live[("deployment", stop.namespace, "cetirun-operator")] = stop.operator
        stop_reads = []
        def run(command, **kwargs):
            if command[0] == "kubectl" and command[4] == "get":
                args = command[4:]
                kind = args[1].lower()
                name = args[2] if args[2] != "-o" else ""
                output = args[args.index("-o") + 1]
                namespace = args[args.index("--namespace") + 1] if "--namespace" in args else ""
                operator_read = kind == "deployment" and name == "cetirun-operator" and not output.startswith("jsonpath-as-json=")
                if ((kind, namespace, name) in stop.objects and (kind != "deployment" or operator_read)
                        or kind in ("rolebindings", "clusterrolebindings")
                        or namespace == stop.agent_namespace and kind in stop.lists):
                    stop_reads.append(len(d.writes))
                    d.commands.append(command)
                    return stop.run(command, **kwargs)
            return normal_run(command, **kwargs)
        d.run = run
        d.execute()
        self.assertEqual(stop_reads[0], 0)
        self.assertGreater(stop_reads[-1], 0)
        self.assertEqual([name for name, _, _ in d.applied], ["istio", "broker", "platform"])
        self.assertFalse(d.applied[-1][1]["cetirun-operator"]["enabled"])
        self.assertIsNotNone(d.cetiadmin_stop_pin)

    def test_independent_governance_is_not_removed_by_cetiadmin_stop(self):
        d = self.removal()
        d.objects[("validatingwebhookconfiguration", "", "cetirun-sandbox-admission")] = {"name": "cetirun-sandbox-admission"}
        with self.assertRaisesRegex(ValueError, "Preserve existing admission"):
            d.check_operator_removal_controls()
        self.assertEqual(d.writes, [])


if __name__ == "__main__":
    unittest.main()
