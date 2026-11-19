# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Render the agent chart with OpenClaw's Portal sign-in (trusted-proxy mode)."""

import json
import shutil
import unittest

from test_native_gateway_chart import RUNTIME_CHART, native_values, object_of_kind, render


def legacy_values(**extra):
    values = {
        "agentType": "openclaw",
        "aiGateway": {
            "enabled": True,
            "keySecretName": "example-model-key",
            "models": [{"id": "example-model", "name": "Example model"}],
        },
        "secrets": {
            "openclawGatewayToken": "example-token",
            "provider": "openai",
            "model": "example-model",
            "apiBase": "http://gateway.example-platform.svc.cluster.local:4000/v1",
        },
    }
    for key, value in extra.items():
        values[key] = {**values.get(key, {}), **value}
    return values


def rendered(values):
    objects = render(RUNTIME_CHART, values)
    config = json.loads(object_of_kind(objects, "ConfigMap")["data"]["openclaw.json"])
    container = next(c for c in object_of_kind(objects, "StatefulSet")["spec"]["template"]["spec"]["containers"]
                     if c["name"] == "cetirun")
    names = {env["name"]: env for env in container.get("env", [])}
    return config, names


def mtls_values(enabled=True):
    values = native_values()
    values["sandboxWorkload"].update({
        "reconciled": True, "executionHeld": False,
        "ingressMTLS": True, "trustedProxyIPs": ["127.0.0.1"],
    })
    values["podAnnotations"]["cetirun.microsoft.com/agent-uid"] = "example-agent-uid"
    values["openclaw"]["portalSignIn"] = {"enabled": enabled}
    values["openclaw"]["config"]["gateway"] = {"bind": "loopback"}
    return values


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class OpenClawPortalSignInTests(unittest.TestCase):
    def test_off_by_default_keeps_the_gateway_token(self):
        config, env = rendered(legacy_values())
        self.assertNotIn("auth", config["gateway"])
        self.assertIn("OPENCLAW_GATEWAY_TOKEN", env)
        self.assertNotIn("OPENCLAW_GATEWAY_PASSWORD", env)

    def test_on_trusts_the_sidecar_for_the_owner_and_turns_the_token_into_a_password(self):
        config, env = rendered(legacy_values(openclaw={"portalSignIn": {"enabled": True}}))
        self.assertEqual(config["gateway"]["auth"], {
            "mode": "trusted-proxy",
            "trustedProxy": {"userHeader": "x-auth-request-email", "allowLoopback": True,
                             "deviceAutoApprove": {"enabled": True}},
        })
        self.assertIn("127.0.0.6", config["gateway"]["trustedProxies"])
        # OpenClaw refuses to start in trusted-proxy mode with a token set.
        self.assertNotIn("OPENCLAW_GATEWAY_TOKEN", env)
        self.assertEqual(env["OPENCLAW_GATEWAY_PASSWORD"]["valueFrom"]["secretKeyRef"]["key"], "OPENCLAW_GATEWAY_TOKEN")

    def test_tailscale_keeps_the_token(self):
        values = legacy_values(openclaw={"portalSignIn": {"enabled": True}})
        values["secrets"]["tailscaleAuthKey"] = "tskey-example"
        config, env = rendered(values)
        self.assertNotIn("auth", config["gateway"])
        self.assertIn("OPENCLAW_GATEWAY_TOKEN", env)

    def test_a_native_sandbox_workload_keeps_the_token(self):
        for reconciled in (False, True):
            for proxies in (["192.0.2.10"], ["10.244.0.0/16"], ["127.0.0.1"]):
                with self.subTest(reconciled=reconciled, proxies=proxies):
                    values = native_values()
                    values["sandboxWorkload"]["trustedProxyIPs"] = proxies
                    if reconciled:
                        values["sandboxWorkload"].update({"reconciled": True, "executionHeld": False})
                    values["openclaw"]["portalSignIn"] = {"enabled": True}
                    config, env = rendered(values)
                    self.assertNotEqual(config["gateway"].get("auth", {}).get("mode"), "trusted-proxy")
                    self.assertIn("OPENCLAW_GATEWAY_TOKEN", env)
                    self.assertNotIn("OPENCLAW_GATEWAY_PASSWORD", env)

    def test_mtls_sign_in_trusts_only_the_managed_loopback_proxy(self):
        config, env = rendered(mtls_values())
        self.assertEqual(config["gateway"]["bind"], "loopback")
        self.assertEqual(config["gateway"]["trustedProxies"], ["127.0.0.1"])
        self.assertEqual(config["gateway"]["auth"], {
            "mode": "trusted-proxy",
            "trustedProxy": {"userHeader": "x-auth-request-email", "allowLoopback": True,
                             "deviceAutoApprove": {"enabled": True}},
        })
        self.assertNotIn("OPENCLAW_GATEWAY_TOKEN", env)
        self.assertEqual(env["OPENCLAW_GATEWAY_PASSWORD"]["valueFrom"]["secretKeyRef"], {
            "name": "example-gateway-token", "key": "OPENCLAW_GATEWAY_TOKEN",
        })
        pod = object_of_kind(render(RUNTIME_CHART, mtls_values()), "StatefulSet")["spec"]["template"]["spec"]
        doctor = next(container for container in pod["initContainers"] if container["name"] == "doctor")
        doctor_env = {item["name"]: item for item in doctor["env"]}
        self.assertNotIn("OPENCLAW_GATEWAY_TOKEN", doctor_env)
        self.assertNotIn("OPENCLAW_GATEWAY_PASSWORD", doctor_env)

    def test_mtls_without_sign_in_keeps_the_token(self):
        config, env = rendered(mtls_values(enabled=False))
        self.assertNotEqual(config["gateway"].get("auth", {}).get("mode"), "trusted-proxy")
        self.assertIn("OPENCLAW_GATEWAY_TOKEN", env)
        self.assertNotIn("OPENCLAW_GATEWAY_PASSWORD", env)

    def test_portal_sign_in_removes_local_and_remote_tokens(self):
        for native in (False, True):
            for token in ("sk-dummy-remote", {"source": "env", "provider": "default", "id": "EXAMPLE_REMOTE_TOKEN"}):
                with self.subTest(native=native, token_type=type(token).__name__):
                    values = mtls_values() if native else legacy_values(openclaw={"portalSignIn": {"enabled": True}})
                    gateway = values["openclaw"].setdefault("config", {}).setdefault("gateway", {})
                    remote = {"url": "wss://gateway.example.com", "password": "sk-dummy-password"}
                    gateway.update({
                        "mode": "local", "auth": {"mode": "token", "token": "sk-dummy-local"},
                        "remote": {**remote, "token": token},
                    })
                    config, env = rendered(values)
                    self.assertEqual(config["gateway"]["mode"], "local")
                    self.assertEqual(config["gateway"]["auth"]["mode"], "trusted-proxy")
                    self.assertNotIn("token", config["gateway"]["auth"])
                    self.assertEqual(config["gateway"]["remote"], remote)
                    self.assertIn("OPENCLAW_GATEWAY_PASSWORD", env)
                    self.assertNotIn("OPENCLAW_GATEWAY_TOKEN", env)

    def test_non_proxy_modes_preserve_remote_tokens(self):
        tailscale = legacy_values(openclaw={"portalSignIn": {"enabled": True}})
        tailscale["secrets"]["tailscaleAuthKey"] = "tskey-example"
        plain = native_values()
        plain["openclaw"]["portalSignIn"] = {"enabled": True}
        for name, values in (("legacy off", legacy_values()), ("mTLS off", mtls_values(enabled=False)),
                             ("Tailscale", tailscale), ("plain Sandbox", plain)):
            with self.subTest(name=name):
                gateway = values.setdefault("openclaw", {}).setdefault("config", {}).setdefault("gateway", {})
                remote = {"url": "wss://gateway.example.com", "token": "sk-dummy-remote"}
                gateway["remote"] = remote
                config, env = rendered(values)
                self.assertEqual(config["gateway"]["remote"], remote)
                self.assertNotEqual(config["gateway"].get("auth", {}).get("mode"), "trusted-proxy")
                self.assertIn("OPENCLAW_GATEWAY_TOKEN", env)

    def test_mtls_sign_in_rejects_missing_or_broadened_proxy_trust(self):
        for proxies in (None, [], ["127.0.0.6"], ["192.0.2.10"], ["127.0.0.0/8"],
                        ["10.244.0.0/16"], ["127.0.0.1", "192.0.2.10"]):
            with self.subTest(proxies=proxies):
                values = mtls_values()
                if proxies is None:
                    del values["sandboxWorkload"]["trustedProxyIPs"]
                else:
                    values["sandboxWorkload"]["trustedProxyIPs"] = proxies
                render(RUNTIME_CHART, values, error="127.0.0.1")

    def test_mtls_sign_in_requires_a_loopback_listener(self):
        values = mtls_values()
        values["openclaw"]["config"]["gateway"]["bind"] = "lan"
        render(RUNTIME_CHART, values, error="gateway.bind=loopback")

    def test_without_the_sidecar_address_the_chart_refuses(self):
        values = legacy_values(openclaw={"portalSignIn": {"enabled": True},
                                         "config": {"gateway": {"trustedProxies": ["192.0.2.1"]}}})
        render(RUNTIME_CHART, values, error="127.0.0.6")


if __name__ == "__main__":
    unittest.main()
