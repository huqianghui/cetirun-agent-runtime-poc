# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "ensure-oauth2-proxy-id-token-header.py"
DEPLOY_SCRIPT = Path(__file__).resolve().parents[2] / "infra" / "istio" / "deploy.sh"
SPEC = importlib.util.spec_from_file_location("ensure_oauth_header", SCRIPT)
module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(module)


class OAuthHeaderTest(unittest.TestCase):
    def test_provider_forwards_refreshed_cookie_on_success(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: [authorization]
    headersToDownstreamOnAllow: [x-existing]
    headersToDownstreamOnDeny: [set-cookie, location]
"""
        result, changed = module.ensure_provider(source)
        self.assertTrue(changed)
        settings = self.provider(result)["envoyExtAuthzHttp"]
        self.assertEqual(settings["headersToDownstreamOnAllow"], ["x-existing", "set-cookie"])
        self.assertEqual(settings["headersToUpstreamOnAllow"], ["authorization"])
        self.assertEqual(module.ensure_provider(result), (result, False))

    def provider(self, mesh_text, name="oauth2-proxy"):
        providers = module._load_mesh(mesh_text).get("extensionProviders", [])
        matches = [provider for provider in providers if provider.get("name") == name]
        self.assertEqual(len(matches), 1)
        return matches[0]

    def test_adds_provider_without_replacing_existing_mesh_settings(self):
        source = "trustDomain: cluster.local\ndefaultConfig:\n  tracing: {}\n"
        result, changed = module.ensure_provider(source)
        self.assertTrue(changed)
        mesh = module._load_mesh(result)
        self.assertEqual(mesh["trustDomain"], "cluster.local")
        self.assertEqual(mesh["defaultConfig"], {"tracing": {}})
        self.assertIn(
            "authorization",
            self.provider(result)["envoyExtAuthzHttp"]["headersToUpstreamOnAllow"],
        )
        repeated, changed_again = module.ensure_provider(result)
        self.assertFalse(changed_again)
        self.assertEqual(repeated, result)

    def test_adds_provider_next_to_existing_provider_entries(self):
        source = """extensionProviders:
- name: oauth2-proxy-v2
  envoyExtAuthzHttp: {}
trustDomain: cluster.local
"""
        result, changed = module.ensure_provider(source)
        self.assertTrue(changed)
        mesh = module._load_mesh(result)
        self.assertEqual(len(mesh["extensionProviders"]), 2)
        self.assertEqual(self.provider(result, "oauth2-proxy-v2")["envoyExtAuthzHttp"], {})
        self.assertEqual(mesh["trustDomain"], "cluster.local")

    def test_converts_inline_empty_extension_provider_list(self):
        source = "extensionProviders: []\ntrustDomain: cluster.local\n"
        result, changed = module.ensure_provider(source)
        self.assertTrue(changed)
        self.assertEqual(result.count("extensionProviders:"), 1)
        self.assertNotIn("extensionProviders: []", result)
        self.assertIn("- name: oauth2-proxy", result)
        repeated, changed_again = module.ensure_provider(result)
        self.assertFalse(changed_again)
        self.assertEqual(repeated, result)

    def test_structured_rewrite_preserves_existing_mesh_values(self):
        source = "extensionProviders: []\ntrustDomain: example.test\ndefaultConfig:\n  tracing: {}\n"
        result, changed = module.ensure_provider(source)
        self.assertTrue(changed)
        mesh = module._load_mesh(result)
        self.assertEqual(mesh["trustDomain"], "example.test")
        self.assertEqual(mesh["defaultConfig"], {"tracing": {}})

    def test_rejects_unsupported_inline_extension_provider_values(self):
        for value in ("{}", "[existing]", "{existing: provider}"):
            with self.subTest(value=value):
                with self.assertRaisesRegex(RuntimeError, "Istio extensionProviders"):
                    module.ensure_provider(f"extensionProviders: {value}\n")

    def test_configmap_patch_uses_resource_version_precondition(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: []
"""
        patch, changed = module.configmap_patch(
            {"metadata": {"resourceVersion": "123"}, "data": {"mesh": source}}
        )
        self.assertTrue(changed)
        self.assertEqual(patch["metadata"], {"resourceVersion": "123"})
        self.assertIn("- authorization", patch["data"]["mesh"])
        with self.assertRaisesRegex(RuntimeError, "resourceVersion"):
            module.configmap_patch({"metadata": {}, "data": {"mesh": source}})

    def test_adds_authorization_without_dropping_existing_headers(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    service: oauth2-proxy.example.svc.cluster.local
    headersToUpstreamOnAllow:
    - x-auth-request-user
"""
        result, changed = module.ensure_header(source)
        self.assertTrue(changed)
        headers = self.provider(result)["envoyExtAuthzHttp"]["headersToUpstreamOnAllow"]
        self.assertEqual(headers, ["x-auth-request-user", "authorization"])

    def test_accepts_indentless_sequence_style(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow:
    - x-auth-request-user
    headersToDownstreamOnDeny:
    - location
"""
        result, changed = module.ensure_header(source)
        self.assertTrue(changed)
        ext_authz = self.provider(result)["envoyExtAuthzHttp"]
        self.assertEqual(
            ext_authz["headersToUpstreamOnAllow"],
            ["x-auth-request-user", "authorization"],
        )
        self.assertEqual(ext_authz["headersToDownstreamOnDeny"], ["location"])

    def test_is_idempotent(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow:
    - authorization
"""
        result, changed = module.ensure_header(source)
        self.assertFalse(changed)
        self.assertEqual(result, source)

    def test_converts_an_inline_empty_header_list_to_an_authorization_block(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: []
    statusOnError: \"403\"
"""
        result, changed = module.ensure_header(source)
        self.assertTrue(changed)
        ext_authz = self.provider(result)["envoyExtAuthzHttp"]
        self.assertEqual(ext_authz["headersToUpstreamOnAllow"], ["authorization"])
        self.assertEqual(ext_authz["statusOnError"], "403")
        repeated, changed_again = module.ensure_header(result)
        self.assertFalse(changed_again)
        self.assertEqual(repeated, result)

    def test_updates_nonempty_inline_header_list(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: [x-auth-request-user]
"""
        result, changed = module.ensure_header(source)
        self.assertTrue(changed)
        headers = self.provider(result)["envoyExtAuthzHttp"]["headersToUpstreamOnAllow"]
        self.assertEqual(headers, ["x-auth-request-user", "authorization"])

    def test_recognizes_provider_when_name_follows_ext_authz(self):
        source = """extensionProviders:
- envoyExtAuthzHttp:
    service: oauth2-proxy.example.svc.cluster.local
    headersToUpstreamOnAllow:
    - x-auth-request-user
  name: oauth2-proxy
"""
        result, changed = module.ensure_provider(source)
        self.assertTrue(changed)
        mesh = module._load_mesh(result)
        self.assertEqual(len(mesh["extensionProviders"]), 1)
        ext_authz = self.provider(result)["envoyExtAuthzHttp"]
        self.assertEqual(ext_authz["service"], "oauth2-proxy.example.svc.cluster.local")
        self.assertEqual(
            ext_authz["headersToUpstreamOnAllow"],
            ["x-auth-request-user", "authorization"],
        )

    def test_recognizes_unquoted_and_quoted_canonical_provider_names(self):
        for name in ("oauth2-proxy", '"oauth2-proxy"', "'oauth2-proxy'"):
            with self.subTest(name=name):
                source = f"""extensionProviders:
- name: {name}
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: []
"""
                result, changed = module.ensure_header(source)
                self.assertTrue(changed)
                self.assertIn("- authorization", result)

    def test_modifies_only_canonical_provider_when_a_near_prefix_exists(self):
        source = """extensionProviders:
- name: oauth2-proxy-v2
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: []
- name: oauth2-proxy
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: []
"""
        result, changed = module.ensure_header(source)
        self.assertTrue(changed)
        near_prefix = self.provider(result, "oauth2-proxy-v2")
        canonical = self.provider(result)
        self.assertEqual(near_prefix["envoyExtAuthzHttp"]["headersToUpstreamOnAllow"], [])
        self.assertEqual(
            canonical["envoyExtAuthzHttp"]["headersToUpstreamOnAllow"],
            ["authorization"],
        )

    def test_rejects_near_prefix_without_a_canonical_provider(self):
        source = """extensionProviders:
- name: oauth2-proxy-v2
  envoyExtAuthzHttp:
    headersToUpstreamOnAllow: []
"""
        with self.assertRaisesRegex(RuntimeError, "resolved exactly"):
            module.ensure_header(source)

    def test_adds_header_block_when_an_older_provider_does_not_have_one(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp:
    service: oauth2-proxy.example.svc.cluster.local
    port: 4180
    headersToDownstreamOnDeny:
    - location
"""
        result, changed = module.ensure_header(source)
        self.assertTrue(changed)
        ext_authz = self.provider(result)["envoyExtAuthzHttp"]
        self.assertEqual(ext_authz["headersToUpstreamOnAllow"], ["authorization"])
        self.assertEqual(ext_authz["headersToDownstreamOnDeny"], ["location"])
        repeated, changed_again = module.ensure_header(result)
        self.assertFalse(changed_again)
        self.assertEqual(repeated, result)

    def test_rejects_ambiguous_oauth2_proxy_providers(self):
        source = """extensionProviders:
- name: oauth2-proxy
  envoyExtAuthzHttp: {}
- name: oauth2-proxy
  envoyExtAuthzHttp: {}
"""
        with self.assertRaisesRegex(RuntimeError, "resolved exactly"):
            module.ensure_header(source)

    def test_rejects_duplicate_top_level_mapping_keys(self):
        source = "extensionProviders: []\nextensionProviders: []\n"
        with self.assertRaisesRegex(RuntimeError, "mesh configuration is invalid"):
            module.ensure_provider(source)

    def test_conflict_retry_recomputes_from_the_latest_mesh(self):
        configmaps = iter(
            [
                {
                    "metadata": {"resourceVersion": "1"},
                    "data": {"mesh": "trustDomain: initial.example\n"},
                },
                {
                    "metadata": {"resourceVersion": "2"},
                    "data": {
                        "mesh": "trustDomain: concurrent.example\ndefaultConfig:\n  tracing: {}\n"
                    },
                },
            ]
        )
        patches = []

        def runner(command, **_kwargs):
            if command[1] == "get":
                return SimpleNamespace(stdout=json.dumps(next(configmaps)))
            with Path(command[-1]).open(encoding="utf-8") as handle:
                patches.append(json.load(handle))
            if len(patches) == 1:
                return SimpleNamespace(returncode=1, stderr="Conflict")
            return SimpleNamespace(returncode=0, stderr="")

        module.reconcile_configmap("aks-istio-system", "mesh", runner=runner)
        self.assertEqual(len(patches), 2)
        self.assertEqual(patches[0]["metadata"]["resourceVersion"], "1")
        self.assertEqual(patches[1]["metadata"]["resourceVersion"], "2")
        latest = patches[1]["data"]["mesh"]
        self.assertIn("trustDomain: concurrent.example", latest)
        self.assertIn("defaultConfig:\n  tracing: {}", latest)
        self.assertIn("- name: oauth2-proxy", latest)

    def test_creates_a_missing_configmap_without_apply(self):
        created = []

        def runner(command, **_kwargs):
            if command[1] == "get":
                return SimpleNamespace(returncode=0, stdout="", stderr="")
            self.assertEqual(command[1:3], ["create", "--filename"])
            with Path(command[-1]).open(encoding="utf-8") as handle:
                created.append(json.load(handle))
            return SimpleNamespace(returncode=0, stderr="")

        module.reconcile_configmap("aks-istio-system", "mesh", runner=runner)
        self.assertEqual(len(created), 1)
        self.assertEqual(created[0]["metadata"], {
            "name": "mesh", "namespace": "aks-istio-system"
        })
        self.assertIn("- name: oauth2-proxy", created[0]["data"]["mesh"])
        self.assertIn("- authorization", created[0]["data"]["mesh"])

    def test_create_race_reloads_and_reconciles_the_winner(self):
        gets = iter(
            [
                SimpleNamespace(returncode=0, stdout="", stderr=""),
                SimpleNamespace(
                    returncode=0,
                    stdout=json.dumps({
                        "metadata": {"resourceVersion": "2"},
                        "data": {"mesh": "trustDomain: concurrent.example\n"},
                    }),
                    stderr="",
                ),
            ]
        )
        patches = []

        def runner(command, **_kwargs):
            if command[1] == "get":
                return next(gets)
            if command[1] == "create":
                return SimpleNamespace(returncode=1, stderr="AlreadyExists")
            with Path(command[-1]).open(encoding="utf-8") as handle:
                patches.append(json.load(handle))
            return SimpleNamespace(returncode=0, stderr="")

        module.reconcile_configmap("aks-istio-system", "mesh", runner=runner)
        self.assertEqual(len(patches), 1)
        self.assertEqual(patches[0]["metadata"]["resourceVersion"], "2")
        self.assertIn("trustDomain: concurrent.example", patches[0]["data"]["mesh"])
        self.assertIn("- name: oauth2-proxy", patches[0]["data"]["mesh"])

    def test_patch_delete_race_reloads_and_creates(self):
        gets = iter(
            [
                SimpleNamespace(
                    returncode=0,
                    stdout=json.dumps({
                        "metadata": {"resourceVersion": "1"},
                        "data": {"mesh": "trustDomain: deleted.example\n"},
                    }),
                    stderr="",
                ),
                SimpleNamespace(returncode=0, stdout="", stderr=""),
            ]
        )
        created = []

        def runner(command, **_kwargs):
            if command[1] == "get":
                return next(gets)
            if command[1] == "patch":
                return SimpleNamespace(returncode=1, stderr="NotFound")
            with Path(command[-1]).open(encoding="utf-8") as handle:
                created.append(json.load(handle))
            return SimpleNamespace(returncode=0, stderr="")

        module.reconcile_configmap("aks-istio-system", "mesh", runner=runner)
        self.assertEqual(len(created), 1)
        self.assertEqual(created[0]["metadata"]["name"], "mesh")
        self.assertIn("- name: oauth2-proxy", created[0]["data"]["mesh"])

    def test_deploy_uses_one_cas_reconciler_for_provider_and_headers(self):
        deploy = DEPLOY_SCRIPT.read_text(encoding="utf-8")
        self.assertEqual(deploy.count("ensure-oauth2-proxy-id-token-header.py"), 1)
        self.assertIn('python3 -c "import yaml"', deploy)
        self.assertNotIn("MERGED_MESH", deploy)
        self.assertNotIn("kubectl apply -f -", deploy)


if __name__ == "__main__":
    unittest.main()
