# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline OAuth2 Proxy chart regressions using synthetic configuration only."""

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


CHART = Path(__file__).resolve().parents[2] / "infra/istio"
OFFLINE = {
    **os.environ,
    "KUBECONFIG": os.devnull,
    "HELM_REGISTRY_CONFIG": os.devnull,
    "HELM_REPOSITORY_CONFIG": os.devnull,
}
ENTRA_VALUES = {
    "clientId": "lint-client",
    "tenantId": "lint-tenant",
    "allowedGroups": "lint-group",
    "cookieSecret": "lint-cookie-secret",
    "oauth2Proxy": {"sessionStore": {"type": "cookie"}},
}
NO_ENV_COLLECTION = {"name": "AZMON_COLLECT_ENV", "value": "False"}
COOKIE_ENV = {
    "name": "COOKIE_SECRET",
    "valueFrom": {
        "secretKeyRef": {"name": "oauth2-proxy-credentials", "key": "cookie-secret"},
    },
}
ENTRA_ARGS = [
    "--http-address=0.0.0.0:4180",
    "--provider=entra-id",
    "--oidc-issuer-url=https://login.microsoftonline.com/lint-tenant/v2.0",
    "--oidc-email-claim=preferred_username",
    "--client-id=lint-client",
    "--redirect-url=https://example.com/oauth2/callback",
    "--entra-id-federated-token-auth",
    "--cookie-secret=$(COOKIE_SECRET)",
    "--cookie-domain=example.com",
    "--cookie-name=_cetirun_oauth2",
    "--cookie-secure=true",
    "--cookie-httponly=true",
    "--code-challenge-method=S256",
    "--insecure-oidc-skip-nonce=false",
    "--request-logging=true",
    "--auth-logging=true",
    "--standard-logging=true",
    "--cookie-samesite=lax",
    "--cookie-csrf-per-request=true",
    "--cookie-csrf-per-request-limit=20",
    "--cookie-expire=45m",
    "--email-domain=*",
    "--skip-provider-button=true",
    "--api-route=^/api/",
    "--api-route=^/ui/[^/]+/api/",
    "--api-route=^/ui/[^/]+/assets/",
    "--api-route=^/[^/]+/cetirun-auth/",
    "--allowed-group=lint-group",
    "--scope=openid email profile",
    "--upstream=static://200",
    "--reverse-proxy=true",
    "--set-xauthrequest=true",
    "--whitelist-domain=example.com",
    "--pass-access-token=false",
    "--pass-authorization-header=true",
    "--set-authorization-header=true",
    "--skip-jwt-bearer-tokens=true",
    "--silence-ping-logging=true",
    "--footer=-",
]


def local_values():
    return {
        "cookieSecret": "lint-cookie-secret",
        "oauth2Proxy": {
            "sessionStore": {"type": "cookie"},
            "mode": "local-oidc",
            "emailClaim": "email",
            "localOIDC": {
                "issuerURL": "https://idp.cetirun.localhost/realms/cetirun",
                "clientID": "cetirun-local-e2e",
                "existingSecret": "cetirun-local-oidc",
            },
        },
    }


def oidc_values():
    return {
        **ENTRA_VALUES,
        "cloud": "AzureChinaCloud",
        "oauth2Proxy": {
            "sessionStore": {"type": "cookie"},
            "mode": "oidc",
            "oidc": {
                "clientID": "lint-client",
                "existingSecret": "portal-oidc",
            },
        },
    }


def local_args(issuer):
    replacements = {
        "--provider=entra-id": "--provider=oidc",
        "--oidc-issuer-url=https://login.microsoftonline.com/lint-tenant/v2.0":
            f"--oidc-issuer-url={issuer}",
        "--oidc-email-claim=preferred_username": "--oidc-email-claim=email",
        "--client-id=lint-client": "--client-id=cetirun-local-e2e",
        "--skip-jwt-bearer-tokens=true": "--skip-jwt-bearer-tokens=false",
    }
    return [
        replacements.get(arg, arg) for arg in ENTRA_ARGS
        if arg not in ("--entra-id-federated-token-auth", "--allowed-group=lint-group")
    ]


def client_secret_env(key="client-secret", *, name="cetirun-local-oidc"):
    return {
        "name": "OAUTH2_PROXY_CLIENT_SECRET",
        "valueFrom": {
            "secretKeyRef": {"name": name, "key": key},
        },
    }


@unittest.skipUnless(shutil.which("helm"), "requires Helm 3 or 4")
class IstioLocalOIDCTests(unittest.TestCase):
    def render(self, values, *, error=None, args=(), chart=CHART):
        result = subprocess.run(
            [
                "helm", "template", "cetirun-istio", str(chart),
                "--namespace", "cetirun-istio", "--values", "-",
                *args,
            ],
            input=json.dumps(values), text=True, capture_output=True,
            env=OFFLINE, timeout=30, check=False,
        )
        if error is not None:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertIn(error, result.stderr)
            return None
        self.assertEqual(result.returncode, 0, result.stderr)
        return {
            (document["kind"], document["metadata"]["name"]): document
            for document in yaml.safe_load_all(result.stdout) if document
        }

    def test_explicit_cookie_entra_provider_and_session_configuration(self):
        objects = self.render(ENTRA_VALUES)
        self.assertEqual(
            objects, self.render({**ENTRA_VALUES, "oauth2Proxy": {"mode": "entra-id", "sessionStore": {"type": "cookie"}}}),
        )
        pod = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]
        self.assertEqual(
            pod["metadata"]["labels"],
            {"app": "oauth2-proxy", "azure.workload.identity/use": "true"},
        )
        container = pod["spec"]["containers"][0]
        self.assertEqual(container["args"], ENTRA_ARGS)
        self.assertEqual(container["env"], [NO_ENV_COLLECTION, COOKIE_ENV])
        account = objects[("ServiceAccount", "oauth2-proxy")]
        self.assertEqual(
            account["metadata"]["annotations"],
            {"azure.workload.identity/client-id": "lint-client"},
        )
        self.assertEqual(account["metadata"]["labels"]["azure.workload.identity/use"], "true")

    def test_cloud_default_keeps_entra_federation_unchanged(self):
        self.assertEqual(self.render(ENTRA_VALUES), self.render({**ENTRA_VALUES, "cloud": "AzureCloud"}))
        self.render({**ENTRA_VALUES, "cloud": "AzureChinaCloud"}, error="requires oauth2Proxy.mode=oidc")
        for cloud in ("", "unknown", False, 123, [], {}):
            with self.subTest(cloud=cloud):
                self.render({**ENTRA_VALUES, "cloud": cloud}, error="cloud must be")

    def test_oidc_cloud_selection_changes_only_provider_and_client_auth(self):
        for cloud, authority in (
            ("AzureCloud", "login.microsoftonline.com"),
            ("AzureChinaCloud", "login.partner.microsoftonline.cn"),
        ):
            with self.subTest(cloud=cloud):
                values = {**oidc_values(), "cloud": cloud}
                expected = self.render(ENTRA_VALUES)
                pod = expected[("Deployment", "oauth2-proxy")]["spec"]["template"]
                del pod["metadata"]["labels"]["azure.workload.identity/use"]
                container = pod["spec"]["containers"][0]
                replacements = {
                    "--provider=entra-id": "--provider=oidc",
                    "--oidc-issuer-url=https://login.microsoftonline.com/lint-tenant/v2.0":
                        f"--oidc-issuer-url=https://{authority}/lint-tenant/v2.0",
                    "--skip-jwt-bearer-tokens=true": "--skip-jwt-bearer-tokens=false",
                }
                container["args"] = [
                    replacements.get(arg, arg) for arg in ENTRA_ARGS
                    if arg != "--entra-id-federated-token-auth"
                ]
                container["env"].append(client_secret_env(name="portal-oidc"))
                account = expected[("ServiceAccount", "oauth2-proxy")]
                del account["metadata"]["annotations"]
                del account["metadata"]["labels"]["azure.workload.identity/use"]
                self.assertEqual(self.render(values), expected)

    def test_oidc_cli_overrides_issuer_and_secret_reference(self):
        objects = self.render({}, args=[
            "--set-string", "cloud=AzureChinaCloud",
            "--set-string", "oauth2Proxy.mode=oidc",
            "--set-string", "oauth2Proxy.oidc.issuerURL=https://identity.example.com:8443/tenant",
            "--set-string", "oauth2Proxy.oidc.clientID=custom-client",
            "--set-string", "oauth2Proxy.oidc.existingSecret=custom-secret",
            "--set-string", "oauth2Proxy.oidc.clientSecretKey=secret-key",
            "--set-string", "allowedGroups=group-a\\,group-b",
        ])
        container = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]
        self.assertIn("--oidc-issuer-url=https://identity.example.com:8443/tenant", container["args"])
        self.assertIn("--client-id=custom-client", container["args"])
        self.assertIn("--allowed-group=group-a", container["args"])
        self.assertIn("--allowed-group=group-b", container["args"])
        self.assertIn("--scope=openid email profile offline_access", container["args"])
        self.assertIn("--session-store-type=redis", container["args"])
        self.assertIn(client_secret_env("secret-key", name="custom-secret"), container["env"])
        self.assertNotIn(("Secret", "custom-secret"), objects)

    def test_oidc_upgrade_supplies_defaults_missing_from_retained_values(self):
        # Helm --reuse-values uses the old chart defaults with the new templates.
        with tempfile.TemporaryDirectory(prefix="cetirun-oidc-upgrade-") as temporary:
            chart = Path(temporary) / "chart"
            shutil.copytree(CHART, chart)
            defaults_path = chart / "values.yaml"
            defaults = yaml.safe_load(defaults_path.read_text())
            defaults.pop("cloud")
            defaults["oauth2Proxy"].pop("oidc")
            defaults_path.write_text(yaml.safe_dump(defaults))
            for cloud in (None, "AzureChinaCloud"):
                for overrides in ({}, {"issuerURL": "https://identity.example.com/tenant"}, {"clientSecretKey": "oidc-secret"}):
                    with self.subTest(cloud=cloud, overrides=overrides):
                        values = oidc_values()
                        if cloud is None:
                            values.pop("cloud")
                        values["oauth2Proxy"]["oidc"].update(overrides)
                        self.assertEqual(
                            self.render(values, chart=chart, args=["--is-upgrade"]),
                            self.render(values, args=["--is-upgrade"]),
                        )
            for field in ("issuerURL", "clientSecretKey"):
                values = oidc_values()
                values["oauth2Proxy"]["oidc"][field] = False
                self.render(values, chart=chart, error=f"oauth2Proxy.oidc.{field}")
            values = oidc_values()
            values["oauth2Proxy"]["oidc"]["clientSecretKey"] = ""
            self.render(values, chart=chart, error="oauth2Proxy.oidc.clientSecretKey")

    def test_group_names_remain_complete_string_arguments(self):
        for values in (copy.deepcopy(ENTRA_VALUES), oidc_values()):
            with self.subTest(mode=values["oauth2Proxy"].get("mode", "entra-id")):
                values["allowedGroups"] = 'cetirun-admins #ops, eng: admins, team "quoted"'
                objects = self.render(values)
                args = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]["args"]
                self.assertTrue(all(isinstance(arg, str) for arg in args))
                self.assertEqual([arg for arg in args if arg.startswith("--allowed-group=")], [
                    "--allowed-group=cetirun-admins #ops",
                    "--allowed-group=eng: admins",
                    '--allowed-group=team "quoted"',
                ])

    def test_entra_rejects_empty_group_lists(self):
        for groups in (",", " , ", " ", False, 123, [], {}):
            with self.subTest(groups=groups):
                self.render({**ENTRA_VALUES, "allowedGroups": groups}, error="allowedGroups")

    def test_oidc_requires_groups_and_does_not_allow_bearer_auth(self):
        for groups in ("", " , ", False, 123, [], {}):
            with self.subTest(groups=groups):
                self.render({**oidc_values(), "allowedGroups": groups}, error="allowedGroups")
        values = oidc_values()
        values["oauth2Proxy"]["skipJwtBearerTokens"] = True
        self.render(values, error="oauth2Proxy.skipJwtBearerTokens=true is not supported in oidc mode")
        self.render({**oidc_values(), "tenantId": ""}, error="tenantId is required")

    def test_oidc_rejects_unsafe_issuer_and_secret_settings(self):
        for issuer in (
            "http://identity.example.com", "http://localhost:5556", "https://user@identity.example.com",
            "https://identity.example.com?query=value", "https://identity.example.com#fragment",
            "https://identity.example.com:65536", "https://:8443", "https://",
            "https://identity.example.com/\n--insecure-oidc-skip-issuer-verification", False, 123, [], {},
        ):
            with self.subTest(issuer=issuer):
                values = oidc_values()
                values["oauth2Proxy"]["oidc"]["issuerURL"] = issuer
                self.render(values, error="oauth2Proxy.oidc.issuerURL")
        for key, invalid in (
            ("clientID", ""), ("existingSecret", ""), ("existingSecret", "ns/secret"),
            ("clientSecretKey", ""), ("clientSecretKey", "../key"),
        ):
            with self.subTest(key=key, invalid=invalid):
                values = oidc_values()
                values["oauth2Proxy"]["oidc"][key] = invalid
                self.render(values, error=f"oauth2Proxy.oidc.{key}")
        for key in ("allowHTTP", "clientSecret", "skipIssuerVerification", "skipTLSVerify"):
            with self.subTest(key=key):
                values = oidc_values()
                values["oauth2Proxy"]["oidc"][key] = True
                self.render(values, error=f"oauth2Proxy.oidc.{key} is not supported")

    def test_oidc_settings_do_not_implicitly_change_authentication_mode(self):
        values = oidc_values()
        values["cloud"] = "AzureCloud"
        values["oauth2Proxy"]["mode"] = "entra-id"
        self.render(values, error="oauth2Proxy.oidc settings require oauth2Proxy.mode=oidc")
        values = oidc_values()
        values["oauth2Proxy"]["localOIDC"] = local_values()["oauth2Proxy"]["localOIDC"]
        self.render(values, error="oauth2Proxy.localOIDC settings require oauth2Proxy.mode=local-oidc")

    def test_entra_still_requires_its_configuration(self):
        for key in ("clientId", "tenantId", "allowedGroups"):
            with self.subTest(key=key):
                values = {**ENTRA_VALUES, key: ""}
                self.render(values, error=f"{key} is required")

    def test_local_oidc_changes_only_auth_provider_wiring(self):
        values = local_values()
        objects = self.render(values)
        baseline = self.render(ENTRA_VALUES)
        expected = copy.deepcopy(baseline)
        pod = expected[("Deployment", "oauth2-proxy")]["spec"]["template"]
        del pod["metadata"]["labels"]["azure.workload.identity/use"]
        container = pod["spec"]["containers"][0]
        container["args"] = local_args(values["oauth2Proxy"]["localOIDC"]["issuerURL"])
        container["env"] = [NO_ENV_COLLECTION, COOKIE_ENV, client_secret_env()]
        account = expected[("ServiceAccount", "oauth2-proxy")]
        del account["metadata"]["annotations"]
        del account["metadata"]["labels"]["azure.workload.identity/use"]
        self.assertEqual(objects, expected)
        self.assertNotIn("azure.workload.identity", json.dumps(list(objects.values())))
        self.assertNotIn(("Secret", "cetirun-local-oidc"), objects)
        self.assertEqual(objects, self.render({**ENTRA_VALUES, **values}))

    def test_local_oidc_uses_custom_secret_key(self):
        values = local_values()
        values["oauth2Proxy"]["localOIDC"]["clientSecretKey"] = "oidc-client-secret"
        objects = self.render(values)
        container = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(
            container["env"],
            [NO_ENV_COLLECTION, COOKIE_ENV, client_secret_env("oidc-client-secret")],
        )

    def test_local_oidc_never_enables_bearer_authentication(self):
        values = local_values()
        default = self.render(values)
        values["oauth2Proxy"]["skipJwtBearerTokens"] = False
        self.assertEqual(default, self.render(values))
        values["oauth2Proxy"]["skipJwtBearerTokens"] = True
        self.render(values, error="oauth2Proxy.skipJwtBearerTokens=true is not supported in local-oidc mode")

    def test_local_oidc_preserves_redis_sessions(self):
        values = local_values()
        values["oauth2Proxy"]["sessionStore"] = {
            "type": "redis",
            "redis": {
                "connectionURL": "redis://sessions.example.com:6379/0",
                "existingSecret": "session-credentials",
            },
        }
        objects = self.render(values)
        pod = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]
        container = pod["spec"]["containers"][0]
        expected_args = local_args(values["oauth2Proxy"]["localOIDC"]["issuerURL"])
        index = expected_args.index("--cookie-expire=45m")
        expected_args[index:index + 1] = [
            "--cookie-expire=8h", "--cookie-refresh=30m", "--session-store-type=redis",
            "--redis-connection-url=redis://sessions.example.com:6379/0",
        ]
        index = expected_args.index("--scope=openid email profile")
        expected_args[index] = "--scope=openid email profile offline_access"
        self.assertEqual(container["args"], expected_args)
        self.assertEqual(container["env"], [
            NO_ENV_COLLECTION, COOKIE_ENV, client_secret_env(),
            {
                "name": "OAUTH2_PROXY_REDIS_PASSWORD",
                "valueFrom": {
                    "secretKeyRef": {"name": "session-credentials", "key": "redis-password"},
                },
            },
        ])
        self.assertIn("checksum/redis-session-credentials", pod["metadata"]["annotations"])
        self.assertEqual(pod["metadata"]["labels"], {"app": "oauth2-proxy"})

    def test_http_requires_explicit_local_flag(self):
        values = local_values()
        values["oauth2Proxy"]["localOIDC"]["issuerURL"] = "http://idp.cetirun.localhost"
        self.render(values, error="oauth2Proxy.localOIDC.allowHTTP")

    def test_http_accepts_only_explicit_local_targets(self):
        for issuer in (
            "http://idp.cetirun.localhost",
            "http://localhost:5556/dex",
            "http://127.0.0.1:5556/dex",
            "http://127.1.2.3:5556/dex",
            "http://[::1]:5556/dex",
        ):
            with self.subTest(issuer=issuer):
                values = local_values()
                values["oauth2Proxy"]["localOIDC"].update({"issuerURL": issuer, "allowHTTP": True})
                objects = self.render(values)
                container = objects[("Deployment", "oauth2-proxy")]["spec"]["template"]["spec"]["containers"][0]
                self.assertEqual(container["args"], local_args(issuer))
                self.assertEqual(container["env"], [NO_ENV_COLLECTION, COOKIE_ENV, client_secret_env()])

    def test_rejects_nonlocal_http_and_invalid_issuer_urls(self):
        for issuer in (
            "http://idp.example.com",
            "http://idp.localhost.example.com",
            "http://127.0.0.1.example.com",
            "http://192.0.2.1",
            "http://127.256.1.1",
            "http://[::]",
            "http://idp.localhost@idp.example.com",
            "http://idp.example.com@idp.localhost",
            "http://localhost:",
            "http://localhost:65536",
            "http://localhost:0",
            "http://localhost?query=value",
            "http://localhost#fragment",
            "https://idp.localhost?",
            "https://idp.localhost#",
            "https://user@idp.localhost",
            "https://:8443",
            "https://",
            "https://idp.localhost/\n--insecure-oidc-skip-issuer-verification",
            "file:///idp.localhost",
            "//idp.localhost",
        ):
            with self.subTest(issuer=issuer):
                values = local_values()
                values["oauth2Proxy"]["localOIDC"].update({"issuerURL": issuer, "allowHTTP": True})
                self.render(values, error="oauth2Proxy.localOIDC.issuerURL")

    def test_rejects_missing_empty_or_mistyped_local_fields(self):
        for key in ("issuerURL", "clientID", "existingSecret", "clientSecretKey"):
            for invalid in (None, "", " ", "\n", False, 123, [], {}):
                with self.subTest(key=key, invalid=invalid):
                    values = local_values()
                    values["oauth2Proxy"]["localOIDC"][key] = invalid
                    self.render(values, error=f"oauth2Proxy.localOIDC.{key}")

    def test_rejects_invalid_secret_references(self):
        for key, invalid in (
            ("existingSecret", "BadName"),
            ("existingSecret", "namespace/secret"),
            ("existingSecret", "-secret"),
            ("existingSecret", "secret."),
            ("existingSecret", "a" * 254),
            ("clientSecretKey", "secret/key"),
            ("clientSecretKey", "."),
            ("clientSecretKey", "..key"),
            ("clientSecretKey", "a" * 254),
        ):
            with self.subTest(key=key, invalid=invalid):
                values = local_values()
                values["oauth2Proxy"]["localOIDC"][key] = invalid
                self.render(values, error=f"oauth2Proxy.localOIDC.{key}")

    def test_rejects_invalid_mode_and_local_settings(self):
        for invalid in ("", "local", "disabled", None, False, 123, [], {}):
            with self.subTest(mode=invalid):
                values = local_values()
                values["oauth2Proxy"]["mode"] = invalid
                self.render(values, error="oauth2Proxy.mode")
        for invalid in (None, False, "oidc", 123, []):
            with self.subTest(localOIDC=invalid):
                values = local_values()
                values["oauth2Proxy"]["localOIDC"] = invalid
                self.render(values, error="oauth2Proxy.localOIDC")
        for invalid in (None, "", "false", "true", 1, [], {}):
            with self.subTest(allowHTTP=invalid):
                values = local_values()
                values["oauth2Proxy"]["localOIDC"]["allowHTTP"] = invalid
                self.render(values, error="oauth2Proxy.localOIDC.allowHTTP")

    def test_rejects_inline_credentials_and_unknown_local_settings(self):
        for key in ("clientSecret", "skipIssuerVerification", "skipTLSVerify"):
            with self.subTest(key=key):
                values = local_values()
                values["oauth2Proxy"]["localOIDC"][key] = "unsupported"
                self.render(values, error=f"oauth2Proxy.localOIDC.{key} is not supported")

    def test_local_settings_do_not_implicitly_enable_local_mode(self):
        values = {**ENTRA_VALUES, **local_values()}
        del values["oauth2Proxy"]["mode"]
        self.render(values, error="oauth2Proxy.localOIDC settings require oauth2Proxy.mode=local-oidc")


if __name__ == "__main__":
    unittest.main()
