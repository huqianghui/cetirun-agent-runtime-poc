# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline Istio rendering and Secret lookup checks with synthetic API objects."""

import base64
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import unittest

import yaml


CHART = Path(__file__).resolve().parents[2] / "infra/istio"
OFFLINE = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}
BASE_VALUES = {
    "clientId": "lint-client",
    "tenantId": "lint-tenant",
    "allowedGroups": "lint-group",
    "cookieSecret": "0123456789abcdef0123456789abcdef",
}


@contextmanager
def lookup_api(secrets, session_secrets=None):
    """Serve only API discovery and synthetic Secret GETs on loopback."""
    requests = []
    discovery = {
        "/version": {"major": "1", "minor": "37", "gitVersion": "v1.37.0"},
        "/api": {"kind": "APIVersions", "apiVersion": "v1", "versions": ["v1"]},
        "/apis": {"kind": "APIGroupList", "apiVersion": "v1", "groups": []},
    }
    groups = {}
    for version, kinds in {
        "v1": {"secrets": "Secret", "services": "Service", "serviceaccounts": "ServiceAccount", "persistentvolumeclaims": "PersistentVolumeClaim"},
        "apps/v1": {"deployments": "Deployment"},
        "networking.k8s.io/v1": {"networkpolicies": "NetworkPolicy"},
        "networking.istio.io/v1alpha3": {"envoyfilters": "EnvoyFilter"},
        "networking.istio.io/v1beta1": {
            "gateways": "Gateway", "virtualservices": "VirtualService", "serviceentries": "ServiceEntry",
        },
        "security.istio.io/v1": {"authorizationpolicies": "AuthorizationPolicy"},
    }.items():
        path = f"/apis/{version}" if "/" in version else f"/api/{version}"
        discovery[path] = {
            "kind": "APIResourceList", "apiVersion": "v1", "groupVersion": version,
            "resources": [
                {"name": name, "kind": kind, "namespaced": True, "verbs": ["get"]}
                for name, kind in kinds.items()
            ],
        }
        if "/" in version:
            group, suffix = version.split("/")
            groups.setdefault(group, []).append({"groupVersion": version, "version": suffix})
    discovery["/apis"]["groups"] = [
        {"name": name, "versions": versions, "preferredVersion": versions[-1]}
        for name, versions in groups.items()
    ]

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            path = self.path.split("?", 1)[0]
            requests.append(path)
            payload = discovery.get(path)
            for namespace, data in secrets.items():
                if path == f"/api/v1/namespaces/{namespace}/secrets/oauth2-proxy-credentials":
                    payload = {
                        "kind": "Secret", "apiVersion": "v1",
                        "metadata": {"namespace": namespace, "name": "oauth2-proxy-credentials"},
                        "data": data,
                    }
            for namespace, data in (session_secrets or {}).items():
                if path == f"/api/v1/namespaces/{namespace}/secrets/oauth2-proxy-sessions":
                    payload = {
                        "kind": "Secret", "apiVersion": "v1",
                        "metadata": {"namespace": namespace, "name": "oauth2-proxy-sessions"},
                        "data": data,
                    }
            self.send_response(200 if payload is not None else 404)
            if payload is None:
                payload = {"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404}
            body = json.dumps(payload).encode()
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    with ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="cetirun-istio-lookup-") as temporary:
                kubeconfig = Path(temporary) / "kubeconfig.json"
                kubeconfig.write_text(json.dumps({
                    "apiVersion": "v1", "kind": "Config", "current-context": "synthetic",
                    "clusters": [{"name": "synthetic", "cluster": {"server": f"http://127.0.0.1:{server.server_port}"}}],
                    "contexts": [{"name": "synthetic", "context": {"cluster": "synthetic", "user": "synthetic"}}],
                    "users": [{"name": "synthetic", "user": {}}],
                }))
                kubeconfig.chmod(0o600)
                yield kubeconfig, requests
        finally:
            server.shutdown()
            thread.join(timeout=5)


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class IstioChartConfigTests(unittest.TestCase):
    def render(self, values=None, *, error=None, upgrade=False, kubeconfig=None):
        command = [
            "helm", "template", "cetirun-istio", str(CHART),
            "--namespace", "cetirun-istio", "--values", "-",
        ]
        if upgrade:
            command.append("--is-upgrade")
        if kubeconfig:
            # The fixture exercises lookup, not Kubernetes schema validation.
            command.extend(["--dry-run=server", "--kubeconfig", str(kubeconfig), "--disable-openapi-validation"])
            # These cases exercise cookie-key migration without a session server.
            command.extend(["--set-string", "oauth2Proxy.sessionStore.type=cookie"])
        else:
            # Keep renders deterministic without generating a session password.
            command.extend(["--set-string", "oauth2Proxy.sessionStore.redis.existingSecret=synthetic-sessions"])
        result = subprocess.run(
            command, input=json.dumps({**BASE_VALUES, **(values or {})}),
            text=True, capture_output=True, env=OFFLINE, timeout=30, check=False,
        )
        if error is not None:
            self.assertNotEqual(result.returncode, 0, "invalid values rendered")
            self.assertIn(error, result.stderr)
            return None
        self.assertEqual(result.returncode, 0, result.stderr)
        return {
            (doc["kind"], doc["metadata"]["name"]): doc
            for doc in yaml.safe_load_all(result.stdout) if doc
        }

    def test_authentication_and_http_filters_match_only_https(self):
        for values in ({}, {"gateway": {"includeWildcardHost": False}}):
            with self.subTest(values=values):
                objects = self.render(values)
                policy = objects[("AuthorizationPolicy", "cetirun-auth-wildcard")]
                for rule in policy["spec"]["rules"]:
                    for target in rule["to"]:
                        self.assertEqual(target["operation"].get("ports"), ["443"])
                filters = []
                for (kind, name), obj in objects.items():
                    if kind != "EnvoyFilter":
                        continue
                    for patch in obj["spec"]["configPatches"]:
                        if patch["applyTo"] == "HTTP_FILTER":
                            filters.append(name)
                            self.assertEqual(patch["match"]["context"], "GATEWAY")
                            self.assertEqual(patch["match"]["listener"].get("portNumber"), 443)
                self.assertCountEqual(filters, [
                    "cetirun-ownership-check", "cetirun-redirect-header", "cetirun-security-headers",
                ])

    def test_wildcard_host_is_optional_without_changing_auth_rules(self):
        default = self.render()
        apex_only = self.render({"gateway": {"includeWildcardHost": False}})
        key = ("Gateway", "cetirun-gateway")
        server = default[key]["spec"]["servers"][0]
        self.assertEqual(server["hosts"], ["example.com", "*.example.com"])
        self.assertEqual(apex_only[key]["spec"]["servers"][0]["hosts"], ["example.com"])
        self.assertEqual(server["tls"]["credentialName"], "cetirun-tls")
        del default[key]
        del apex_only[key]
        self.assertEqual(default, apex_only)

    def test_session_protections_and_logging_defaults(self):
        objects = self.render()
        args = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]["args"]
        for flag in (
            "--cookie-secure=true", "--cookie-httponly=true",
            "--code-challenge-method=S256", "--insecure-oidc-skip-nonce=false",
            "--cookie-csrf-per-request=true", "--cookie-csrf-per-request-limit=20",
            "--api-route=^/api/", "--api-route=^/ui/[^/]+/api/", "--api-route=^/ui/[^/]+/assets/",
            "--request-logging=true", "--auth-logging=true", "--standard-logging=true",
            "--skip-jwt-bearer-tokens=true", "--pass-access-token=false",
        ):
            with self.subTest(flag=flag):
                self.assertIn(flag, args)
        for flag in ("--ssl-insecure-skip-verify", "--insecure-oidc-skip-issuer-verification"):
            self.assertFalse(any(arg.startswith(flag) for arg in args))

    def test_logging_and_bearer_auth_can_be_disabled_independently(self):
        self.assertEqual(self.render(), self.render({"oauth2Proxy": {"skipJwtBearerTokens": True}}))
        for field in ("request", "auth", "standard"):
            values = {"oauth2Proxy": {"logging": {field: False}, "skipJwtBearerTokens": False}}
            with self.subTest(field=field):
                objects = self.render(values)
                args = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]["args"]
                self.assertIn(f"--{field}-logging=false", args)
                self.assertIn("--skip-jwt-bearer-tokens=false", args)
                for other in {"request", "auth", "standard"} - {field}:
                    self.assertIn(f"--{other}-logging=true", args)

    def test_invalid_new_options_are_rejected(self):
        for value in ("false", 1, [], {}):
            for values, field in (
                ({"gateway": {"includeWildcardHost": value}}, "gateway.includeWildcardHost"),
                ({"oauth2Proxy": {"skipJwtBearerTokens": value}}, "oauth2Proxy.skipJwtBearerTokens"),
                *[({"oauth2Proxy": {"logging": {key: value}}}, f"oauth2Proxy.logging.{key}")
                  for key in ("request", "auth", "standard")],
            ):
                with self.subTest(field=field, value=value):
                    self.render(values, error=field)
        for value in (False, 1, [], {}, "Bad Namespace", "a/b", "a" * 64):
            with self.subTest(legacy_namespace=value):
                self.render({"cookieSecretLegacyNamespace": value}, error="cookieSecretLegacyNamespace")
        for value in (False, "disabled", ["request"]):
            self.render({"oauth2Proxy": {"logging": value}}, error="oauth2Proxy.logging")
        self.render({"oauth2Proxy": {"logging": {"requests": False}}}, error="oauth2Proxy.logging.requests")

    def test_missing_new_values_keep_compatible_defaults_on_upgrade(self):
        default = self.render()
        old = self.render({
            "gateway": {"includeWildcardHost": None},
            "oauth2Proxy": {"logging": None, "skipJwtBearerTokens": None},
            "cookieSecretLegacyNamespace": None,
        }, upgrade=True)
        self.assertEqual(old, default)

    def test_supplied_cookie_and_tls_values_are_stable_across_upgrade(self):
        values = {"tls": {"enabled": True, "cert": "synthetic-cert", "key": "synthetic-key"}}
        objects = self.render(values)
        self.assertEqual(objects, self.render(values, upgrade=True))
        cookie = objects[("Secret", "oauth2-proxy-credentials")]
        self.assertEqual(base64.b64decode(cookie["data"]["cookie-secret"]).decode(), BASE_VALUES["cookieSecret"])
        tls = objects[("Secret", "cetirun-tls")]
        self.assertEqual(tls["metadata"]["namespace"], "aks-istio-ingress")
        self.assertEqual(base64.b64decode(tls["data"]["tls.crt"]).decode(), "synthetic-cert")
        self.assertEqual(base64.b64decode(tls["data"]["tls.key"]).decode(), "synthetic-key")
        self.assertNotIn(("Secret", "cetirun-tls"), self.render())
        self.render({"tls": {"enabled": True}}, error="tls.cert is required", upgrade=True)
        self.render({"tls": {"enabled": True, "cert": "synthetic-cert"}}, error="tls.key is required", upgrade=True)

    def test_current_cookie_secret_wins_without_reading_legacy_namespace(self):
        data = {"cookie-secret": base64.b64encode(b"current-synthetic-cookie-secret32").decode()}
        for legacy in ("istio-system", "", "old-auth", "cetirun-istio"):
            with self.subTest(legacy_namespace=legacy), lookup_api({"cetirun-istio": data, "istio-system": {}}) as (config, requests):
                values = {"cookieSecret": "", "cookieSecretLegacyNamespace": legacy}
                first = self.render(values, upgrade=True, kubeconfig=config)
                second = self.render(values, upgrade=True, kubeconfig=config)
                self.assertEqual(first, second)
                self.assertEqual(first[("Secret", "oauth2-proxy-credentials")]["data"], data)
                secret_requests = [path for path in requests if "/secrets/" in path]
                self.assertEqual(secret_requests, [
                    "/api/v1/namespaces/cetirun-istio/secrets/oauth2-proxy-credentials",
                ] * 2)

    def test_legacy_cookie_migration_uses_the_configured_namespace(self):
        data = {"cookie-secret": base64.b64encode(b"legacy-synthetic-cookie-secret-32").decode()}
        for legacy in (None, "old-auth"):
            namespace = legacy or "istio-system"
            with self.subTest(legacy_namespace=legacy), lookup_api({namespace: data}) as (config, requests):
                values = {"cookieSecret": ""}
                if legacy is not None:
                    values["cookieSecretLegacyNamespace"] = legacy
                objects = self.render(values, upgrade=True, kubeconfig=config)
                self.assertEqual(objects[("Secret", "oauth2-proxy-credentials")]["data"], data)
                self.assertEqual([path for path in requests if "/secrets/" in path], [
                    "/api/v1/namespaces/cetirun-istio/secrets/oauth2-proxy-credentials",
                    f"/api/v1/namespaces/{namespace}/secrets/oauth2-proxy-credentials",
                ])

    def test_missing_cookie_key_is_never_replaced(self):
        for namespace, error in (
            ("cetirun-istio", "existing oauth2-proxy-credentials Secret is missing cookie-secret"),
            ("istio-system", "legacy oauth2-proxy-credentials Secret is missing cookie-secret"),
        ):
            for data in ({}, None, {"cookie-secret": ""}):
                with self.subTest(namespace=namespace, data=data), lookup_api({namespace: data}) as (config, _):
                    self.render({"cookieSecret": ""}, upgrade=True, kubeconfig=config, error=error)

    def test_upgrade_requires_a_cookie_secret_and_disabled_migration_does_not_read_legacy(self):
        error = "oauth2-proxy-credentials Secret was not found during upgrade"
        self.render({"cookieSecret": ""}, upgrade=True, error=error)
        for secrets, legacy in (({}, "istio-system"), ({"istio-system": {"cookie-secret": "unused"}}, "")):
            with self.subTest(legacy_namespace=legacy), lookup_api(secrets) as (config, requests):
                self.render({"cookieSecret": "", "cookieSecretLegacyNamespace": legacy}, upgrade=True, kubeconfig=config, error=error)
                if legacy == "":
                    self.assertNotIn("/api/v1/namespaces/istio-system/secrets/oauth2-proxy-credentials", requests)

    def test_first_install_generates_only_when_no_cookie_secret_exists(self):
        with lookup_api({}) as (config, _):
            objects = self.render({"cookieSecret": ""}, kubeconfig=config)
        cookie = objects[("Secret", "oauth2-proxy-credentials")]["data"]["cookie-secret"]
        self.assertRegex(base64.b64decode(cookie).decode(), r"^[A-Za-z0-9]{32}$")

    def test_explicit_cookie_value_does_not_lookup_or_change_tls_ownership(self):
        with lookup_api({"cetirun-istio": {}}) as (config, requests):
            objects = self.render(upgrade=True, kubeconfig=config)
            self.assertFalse(any("/secrets/" in path for path in requests))
        cookie = objects[("Secret", "oauth2-proxy-credentials")]["data"]["cookie-secret"]
        self.assertEqual(base64.b64decode(cookie).decode(), BASE_VALUES["cookieSecret"])
        self.assertNotIn(("Secret", "cetirun-tls"), objects)


if __name__ == "__main__":
    unittest.main()
