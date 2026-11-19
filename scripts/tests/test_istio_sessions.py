# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Render the real session-storage chart with synthetic configuration."""

import base64
import json
import os
from pathlib import Path
import subprocess
import unittest

import yaml

from scripts.tests.test_istio_chart_config import lookup_api


CHART = Path(__file__).resolve().parents[2] / "infra/istio"


class IstioSessionTests(unittest.TestCase):
    def render(self, session=None, *, kubeconfig=None, upgrade=False, error=None):
        values = {
            "clientId": "synthetic-client", "tenantId": "synthetic-tenant",
            "allowedGroups": "synthetic-group", "cookieSecret": "a" * 32,
            "oauth2Proxy": {"sessionStore": session or {}},
        }
        command = ["helm", "template", "sessions", str(CHART), "-n", "sessions", "-f", "-"]
        if kubeconfig:
            command.extend(["--dry-run=server", "--kubeconfig", str(kubeconfig), "--disable-openapi-validation"])
        if upgrade:
            command.append("--is-upgrade")
        result = subprocess.run(
            command,
            input=json.dumps(values), text=True, capture_output=True, check=False,
            env={**os.environ, "KUBECONFIG": os.devnull}, timeout=30,
        )
        if error:
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(error, result.stderr)
            return None
        self.assertEqual(result.returncode, 0, result.stderr)
        return {(doc["kind"], doc["metadata"]["name"]): doc
                for doc in yaml.safe_load_all(result.stdout) if doc}

    def test_default_sessions_refresh_without_browser_refresh_tokens(self):
        objects = self.render()
        pod = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]
        args = pod["spec"]["containers"][0]["args"]
        for flag in ("--session-store-type=redis", "--cookie-expire=8h",
                     "--cookie-refresh=30m", "--scope=openid email profile offline_access",
                     "--api-route=^/[^/]+/cetirun-auth/"):
            self.assertIn(flag, args)
        self.assertIn(("Deployment", "oauth2-proxy-sessions"), objects)
        self.assertIn(("PersistentVolumeClaim", "oauth2-proxy-sessions"), objects)

    def test_cookie_mode_remains_explicit_and_has_no_server_storage(self):
        objects = self.render({"type": "cookie"})
        args = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertIn("--cookie-expire=45m", args)
        self.assertIn("--scope=openid email profile", args)
        self.assertFalse(any("oauth2-proxy-sessions" == name for _, name in objects))

    def test_external_redis_does_not_install_a_second_server(self):
        objects = self.render({"type": "redis", "redis": {
            "connectionURL": "rediss://sessions.example.com:6379/0",
            "existingSecret": "external-sessions", "passwordKey": "password",
        }})
        args = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]["args"]
        self.assertIn("--redis-connection-url=rediss://sessions.example.com:6379/0", args)
        self.assertFalse(any("oauth2-proxy-sessions" == name for _, name in objects))

    def test_ephemeral_storage_is_opt_in(self):
        objects = self.render({"redis": {"bundled": {"persistence": {"enabled": False}}}})
        self.assertNotIn(("PersistentVolumeClaim", "oauth2-proxy-sessions"), objects)
        pod = objects[("Deployment", "oauth2-proxy-sessions")]["spec"]["template"]["spec"]
        self.assertEqual(pod["volumes"], [{"name": "data", "emptyDir": {}}])
        args = pod["containers"][0]["args"]
        self.assertEqual(args[args.index("--appendonly") + 1], "no")

    def test_persistent_storage_is_retained_and_not_shared_with_gateway(self):
        objects = self.render()
        deployment = objects[("Deployment", "oauth2-proxy-sessions")]
        self.assertEqual(deployment["spec"]["strategy"], {"type": "Recreate"})
        pod = deployment["spec"]["template"]["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertEqual(pod["volumes"], [{"name": "data", "persistentVolumeClaim": {
            "claimName": "oauth2-proxy-sessions"}}])
        args = pod["containers"][0]["args"]
        self.assertEqual(args[args.index("--appendonly") + 1], "yes")
        pvc = objects[("PersistentVolumeClaim", "oauth2-proxy-sessions")]
        self.assertEqual(pvc["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        policy = objects[("NetworkPolicy", "oauth2-proxy-sessions")]["spec"]
        self.assertEqual(policy["ingress"][0]["from"], [{"podSelector": {"matchLabels": {"app": "oauth2-proxy"}}}])

    def test_generated_password_is_reused_and_rotation_changes_both_pods(self):
        def render_password(password):
            data = {"redis-password": base64.b64encode(password.encode()).decode()}
            with lookup_api({}, {"sessions": data}) as (config, _):
                first = self.render(kubeconfig=config, upgrade=True)
                self.assertEqual(first, self.render(kubeconfig=config, upgrade=True))
            self.assertEqual(first[("Secret", "oauth2-proxy-sessions")]["data"], data)
            return first
        before = render_password("synthetic-password-before")
        after = render_password("synthetic-password-after")
        for name in ("oauth2-proxy", "oauth2-proxy-sessions"):
            self.assertNotEqual(before[("Deployment", name)]["spec"]["template"]["metadata"]["annotations"],
                                after[("Deployment", name)]["spec"]["template"]["metadata"]["annotations"])

    def test_incomplete_existing_secret_is_not_replaced(self):
        with lookup_api({}, {"sessions": {}}) as (config, _):
            self.render(kubeconfig=config, upgrade=True, error="missing its password")

    def test_external_storage_requires_a_secret_and_never_embeds_passwords(self):
        self.render({"redis": {"connectionURL": "redis://sessions.example.com:6379/0"}}, error="existingSecret")
        self.render({"redis": {"bundled": {"enabled": False}}}, error="connectionURL")
        self.render({"redis": {"connectionURL": "redis://user:sk-dummy@sessions.example.com:6379/0", "existingSecret": "example"}}, error="without credentials")

    def test_bundled_server_can_use_an_existing_secret_and_storage_class(self):
        objects = self.render({"redis": {"existingSecret": "supplied-sessions", "passwordKey": "password",
                                         "bundled": {"persistence": {"storageClass": "example-rwo", "size": "2Gi"}}}})
        self.assertNotIn(("Secret", "oauth2-proxy-sessions"), objects)
        pvc = objects[("PersistentVolumeClaim", "oauth2-proxy-sessions")]["spec"]
        self.assertEqual(pvc["storageClassName"], "example-rwo")
        self.assertEqual(pvc["resources"]["requests"]["storage"], "2Gi")
        for name, env_name in (("oauth2-proxy", "OAUTH2_PROXY_REDIS_PASSWORD"), ("oauth2-proxy-sessions", "VALKEY_PASSWORD")):
            env = objects[("Deployment", name)]["spec"]["template"]["spec"]["containers"][0]["env"]
            password = next(item for item in env if item["name"] == env_name)
            self.assertEqual(password["valueFrom"]["secretKeyRef"], {"name": "supplied-sessions", "key": "password"})


if __name__ == "__main__":
    unittest.main()
