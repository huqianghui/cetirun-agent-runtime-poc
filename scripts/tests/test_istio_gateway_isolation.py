# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import ctypes
import ctypes.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import unittest

import yaml


TEMPLATE = (
    Path(__file__).resolve().parents[2]
    / "infra"
    / "istio"
    / "templates"
    / "ingress"
    / "envoy-filter-alias.yaml"
)
SECURITY_HEADERS_TEMPLATE = TEMPLATE.with_name("envoy-filter-security-headers.yaml")
CHART = TEMPLATE.parents[2]
LUA_LIBRARY = ctypes.util.find_library("lua5.4")
MTLS_VALUES = (
    "--set", "portalAuth.mtls.enabled=true",
    "--set-string", "portalAuth.mtls.serverIdentity=spiffe://cluster.local/ns/synthetic/sa/portal",
)

LUA_GATEWAY_FIXTURE = r'''
function gateway_fixture(kind)
  local result = {}
  local values = {
    [":path"] = "/ui/cetiadmin/",
    [":method"] = "GET",
    [":authority"] = "example.com",
    ["x-auth-request-email"] = "owner@example.com",
    ["authorization"] = "Bearer verified-id-token",
    ["x-cetirun-portal-id-token"] = "Bearer client-supplied-token",
    ["cookie"] = "_cetirun_oauth2=portal; agent_session=agent",
  }
  if kind == "missing-identity" then
    values["x-auth-request-email"] = nil
    values["x-auth-request-preferred-username"] = nil
  end
  local headers = {}
  function headers:get(name) return values[name] end
  function headers:remove(name) values[name] = nil end
  function headers:replace(name, value) values[name] = value end

  local metadata = {}
  function metadata:get(name)
    result.metadata_read = true
    if name == "cetirun" then
      return {client_authorization = "Bearer agent-token"}
    end
    return nil
  end
  local stream_info = {}
  function stream_info:dynamicMetadata() return metadata end

  local handle = {}
  function handle:headers() return headers end
  function handle:streamInfo() return stream_info end
  function handle:httpCall(cluster, request_headers, _, timeout)
    result.cluster = cluster
    result.internal_token = request_headers["x-cetirun-portal-id-token"]
    result.timeout = timeout
    if kind == "denied" then return {[":status"] = "403"}, "" end
    return {[":status"] = "200"}, ""
  end
  function handle:respond(response_headers, body)
    result.response_status = response_headers[":status"]
    result.response_body = body
  end

  envoy_on_request(handle)
  return table.concat({
    result.cluster or "",
    result.internal_token or "",
    tostring(result.timeout or ""),
    result.response_status or "",
    result.response_body or "",
    result.metadata_read and "metadata-read" or "",
    values["authorization"] or "",
    values["cookie"] or "",
    values["x-cetirun-portal-id-token"] or "",
  }, "\t")
end
'''


class IstioGatewayIsolationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = TEMPLATE.read_text(encoding="utf-8")
        cls.security_headers = SECURITY_HEADERS_TEMPLATE.read_text(encoding="utf-8")

    def _lua_string_list(self, name):
        match = re.search(rf"local {name} = \{{([^\n]*)\}}", self.source)
        self.assertIsNotNone(match, f"missing Lua list {name}")
        return re.findall(r'"([^"]+)"', match.group(1))

    @staticmethod
    def _render_chart(*extra_args):
        command = [
            "helm", "template", "cetirun-istio", str(CHART),
            "--namespace", "cetirun-istio",
            "--set-string", "clientId=lint-client",
            "--set-string", "tenantId=lint-tenant",
            "--set-string", "allowedGroups=lint-group",
            "--set-string", "cookieSecret=0123456789abcdef0123456789abcdef",
            "--set-string", "oauth2Proxy.sessionStore.redis.existingSecret=synthetic-sessions",
            *extra_args,
        ]
        env = {**os.environ, "KUBECONFIG": os.devnull, "HELM_REGISTRY_CONFIG": os.devnull}
        return subprocess.run(command, text=True, capture_output=True, env=env, timeout=30, check=False)

    @classmethod
    def _render_ownership_filter(cls, *extra_args):
        result = cls._render_chart(*extra_args)
        if result.returncode:
            raise AssertionError(f"helm template failed: {result.stderr}")
        for document in yaml.safe_load_all(result.stdout):
            if document and document.get("kind") == "EnvoyFilter" and document.get("metadata", {}).get("name") == "cetirun-ownership-check":
                return document
        raise AssertionError("rendered ownership EnvoyFilter has no Lua code")

    @classmethod
    def _render_ownership_lua(cls, *extra_args):
        ownership_filter = cls._render_ownership_filter(*extra_args)
        for patch in ownership_filter["spec"]["configPatches"]:
            typed_config = patch.get("patch", {}).get("value", {}).get("typed_config", {})
            if typed_config.get("inline_code"):
                return typed_config["inline_code"]
        raise AssertionError("rendered ownership EnvoyFilter has no Lua code")

    @staticmethod
    def _call_rendered_lua(script, function, value):
        """Evaluate one pure helper from the rendered Envoy Lua without a cluster."""
        lua = ctypes.CDLL(LUA_LIBRARY)
        lua.luaL_newstate.restype = ctypes.c_void_p
        lua.luaL_openlibs.argtypes = [ctypes.c_void_p]
        lua.luaL_loadstring.argtypes = [ctypes.c_void_p, ctypes.c_char_p]
        lua.luaL_loadstring.restype = ctypes.c_int
        lua.lua_pcallk.argtypes = [ctypes.c_void_p, ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_longlong, ctypes.c_void_p]
        lua.lua_pcallk.restype = ctypes.c_int
        lua.lua_getglobal.argtypes = [ctypes.c_void_p, ctypes.c_char_p]
        lua.lua_getglobal.restype = ctypes.c_int
        lua.lua_pushstring.argtypes = [ctypes.c_void_p, ctypes.c_char_p]
        lua.lua_pushnil.argtypes = [ctypes.c_void_p]
        lua.lua_tolstring.argtypes = [ctypes.c_void_p, ctypes.c_int, ctypes.POINTER(ctypes.c_size_t)]
        lua.lua_tolstring.restype = ctypes.c_char_p
        lua.lua_close.argtypes = [ctypes.c_void_p]

        state = lua.luaL_newstate()
        try:
            lua.luaL_openlibs(state)
            if lua.luaL_loadstring(state, script.encode()) != 0 or lua.lua_pcallk(state, 0, 0, 0, 0, None) != 0:
                raise AssertionError("rendered ownership Lua did not compile")
            lua.lua_getglobal(state, function.encode())
            if value is None:
                lua.lua_pushnil(state)
            else:
                lua.lua_pushstring(state, value.encode())
            if lua.lua_pcallk(state, 1, 1, 0, 0, None) != 0:
                raise AssertionError(f"rendered Lua helper {function} failed")
            length = ctypes.c_size_t()
            result = lua.lua_tolstring(state, -1, ctypes.byref(length))
            return result[:length.value].decode() if result else None
        finally:
            lua.lua_close(state)

    def test_portal_exempt_paths_use_exact_and_segment_matches(self):
        exact_paths = self._lua_string_list("EXEMPT_EXACT_PATHS")
        path_prefixes = self._lua_string_list("EXEMPT_PATH_PREFIXES")

        self.assertIn("/healthz", exact_paths)
        self.assertIn("/readyz", exact_paths)
        self.assertNotIn("/healthz", path_prefixes)
        self.assertNotIn("/readyz", path_prefixes)
        self.assertIn(
            "for _, exact_path in ipairs(EXEMPT_EXACT_PATHS) do\n"
            "                  if path == exact_path then return true end",
            self.source,
        )
        self.assertIn(
            'return path == prefix or path:sub(1, #prefix + 1) == prefix .. "/"',
            self.source,
        )
        self.assertIn(
            "for _, prefix in ipairs(EXEMPT_PATH_PREFIXES) do\n"
            "                  if has_path_segment_prefix(path, prefix) then return true end",
            self.source,
        )

        def is_exempt_path(path):
            return path in exact_paths or any(
                path == prefix or path.startswith(prefix + "/")
                for prefix in path_prefixes
            )

        cases = {
            "/healthz": True,
            "/readyz": True,
            "/healthz/live": False,
            "/readyz/live": False,
            "/healthz-demo/term/": False,
            "/readyz-demo/term/": False,
            "/connections": True,
            "/connections/": True,
            "/connections/term/": False,
            "/connections/desktop/": False,
            "/connections/cetirun-auth/status": False,
            "/connections/api/": False,
            "/connections-demo/term/": False,
            "/connectionsdemo": False,
            "/ui/connections/": False,
            "/admin": True,
            "/admin/users": True,
            "/admin-tools/term/": False,
            "/api/v1/instance": True,
            "/api-demo/term/": False,
        }
        for path, expected in cases.items():
            with self.subTest(path=path):
                self.assertEqual(is_exempt_path(path), expected)

    def test_portal_spa_routes_are_exempt_and_reserved(self):
        # A new Portal page must not be interpreted as an Agent name after
        # sign-in, or be shadowed by an Agent created with the same bare name.
        root = TEMPLATE.parents[4]
        app = (root / "app/cetirun-portal/web/src/App.tsx").read_text()
        handler = (root / "app/cetirun-portal/internal/handler/instance.go").read_text()
        reserved_block = re.search(
            r"var reservedInstanceNames = map\[string\]struct\{\}\{(.*?)\n\}",
            handler,
            re.DOTALL,
        )
        self.assertIsNotNone(reserved_block)
        reserved = set(re.findall(r'"([^"/]+)":', reserved_block.group(1)))
        exemptions = self._lua_string_list("EXEMPT_PATH_PREFIXES") + self._lua_string_list("EXEMPT_EXACT_PATHS")
        routes = set(re.findall(r'path="/([^/":]+)', app))
        self.assertIn("connections", routes)
        for route in routes:
            with self.subTest(route=route):
                self.assertIn("/" + route, exemptions)
                self.assertIn(route, reserved)

    def test_portal_sign_in_cookies_never_reach_agents(self):
        # OAuth2 Proxy names a CSRF cookie per sign-in when
        # --cookie-csrf-per-request is on: <session cookie>_<8 characters of
        # the state hash>_csrf, or <session cookie>_csrf otherwise.
        for line in (
            'return name == PORTAL_COOKIE_NAME or',
            'name:match("^" .. PORTAL_COOKIE_NAME .. "_%d+$") ~= nil or',
            'name:match("^" .. PORTAL_COOKIE_NAME .. "_[%w_%-]*csrf$") ~= nil',
        ):
            with self.subTest(line=line):
                self.assertIn(line, self.source)

        # The Lua patterns above, as Python regular expressions.
        def is_portal_cookie_name(name):
            return (
                name == "_cetirun_oauth2"
                or re.fullmatch(r"_cetirun_oauth2_\d+", name) is not None
                or re.fullmatch(r"_cetirun_oauth2_[A-Za-z0-9_-]*csrf", name) is not None
            )

        cases = {
            "_cetirun_oauth2": True,
            "_cetirun_oauth2_0": True,
            "_cetirun_oauth2_12": True,
            "_cetirun_oauth2_csrf": True,
            "_cetirun_oauth2_D2Wd31uG_csrf": True,
            "_cetirun_oauth2_-j84Ob9O_csrf": True,
            "_cetirun_oauth2_6Ys_ZHE7_csrf": True,
            "_cetirun_oauth2_csrf_extra": False,
            "_cetirun_oauth2x": False,
            "agent_session": False,
            "openclaw_csrf": False,
        }
        for name, expected in cases.items():
            with self.subTest(name=name):
                self.assertEqual(is_portal_cookie_name(name), expected)

    def test_instance_branch_preserves_ownership_and_credential_isolation(self):
        token_removal = self.source.index(
            'request_handle:headers():remove("x-cetirun-portal-id-token")'
        )
        exempt_branch = self.source.index(
            "if is_exempt(path_only, authority) then", token_removal
        )
        instance_branch = self.source.index(
            "-- Native agent UIs are namespaced", exempt_branch
        )
        ownership_check = self.source.index(
            "request_handle:httpCall(", instance_branch
        )
        fail_closed = self.source.index(
            'if status ~= "200" then', ownership_check
        )
        cookie_removal = self.source.index(
            "strip_portal_session_cookie(request_handle)", fail_closed
        )
        native_api_auth = self.source.index(
            "forward_native_agent_api_key(request_handle, path_only, authority)", cookie_removal
        )

        self.assertLess(token_removal, exempt_branch)
        self.assertLess(exempt_branch, instance_branch)
        self.assertLess(instance_branch, ownership_check)
        self.assertLess(ownership_check, fail_closed)
        self.assertLess(fail_closed, cookie_removal)
        self.assertLess(cookie_removal, native_api_auth)

    @unittest.skipUnless(shutil.which("helm") and LUA_LIBRARY, "requires Helm and liblua5.4")
    def test_rendered_ownership_lua_forwards_only_the_ext_auth_id_token(self):
        script = self._render_ownership_lua(*MTLS_VALUES)
        self.assertIn("function trusted_portal_identity_token(value)", script)
        self.assertIn(
            'internal_auth_headers["x-cetirun-portal-id-token"] = identity_token',
            script,
        )
        self.assertNotIn('["authorization"] = identity_token', script)
        for value, expected in (
            ("Bearer validated-id-token", "Bearer validated-id-token"),
            ("bearer validated-id-token", None),
            ("Basic client-credential", None),
            ("", None),
            (None, None),
        ):
            with self.subTest(value=value):
                self.assertEqual(
                    self._call_rendered_lua(script, "trusted_portal_identity_token", value),
                    expected,
                )

    @unittest.skipUnless(shutil.which("helm") and LUA_LIBRARY, "requires Helm and liblua5.4")
    def test_rendered_ownership_lua_keeps_id_tokens_out_of_agent_requests_and_denials(self):
        plaintext_script = self._render_ownership_lua() + LUA_GATEWAY_FIXTURE
        plaintext_owned = self._call_rendered_lua(plaintext_script, "gateway_fixture", "owned").split("\t")
        self.assertEqual(plaintext_owned, [
            "portal_auth_cluster", "", "5000", "", "",
            "metadata-read", "Bearer agent-token", "agent_session=agent", "",
        ])

        script = self._render_ownership_lua(*MTLS_VALUES) + LUA_GATEWAY_FIXTURE

        owned = self._call_rendered_lua(script, "gateway_fixture", "owned").split("\t")
        self.assertEqual(owned, [
            "portal_auth_mtls_cluster", "Bearer verified-id-token", "5000", "", "",
            "metadata-read", "Bearer agent-token", "agent_session=agent", "",
        ])

        denied = self._call_rendered_lua(script, "gateway_fixture", "denied").split("\t")
        self.assertEqual(denied[0:3], ["portal_auth_mtls_cluster", "Bearer verified-id-token", "5000"])
        self.assertEqual(denied[3], "403")
        self.assertNotIn("verified-id-token", denied[4])
        self.assertNotIn("client-supplied-token", denied[4])
        self.assertEqual(denied[5], "")

        missing_identity = self._call_rendered_lua(script, "gateway_fixture", "missing-identity").split("\t")
        self.assertEqual(missing_identity[0], "")
        self.assertEqual(missing_identity[3:6], ["403", "Forbidden: no identity", ""])

    @unittest.skipUnless(shutil.which("helm"), "requires Helm")
    def test_mtls_ownership_cluster_is_distinct_and_has_no_plaintext_fallback(self):
        disabled = self._render_ownership_filter()
        enabled = self._render_ownership_filter(*MTLS_VALUES)

        def clusters(ownership_filter):
            return {
                value["name"]: value
                for patch in ownership_filter["spec"]["configPatches"]
                if (value := patch.get("patch", {}).get("value", {})).get("type") == "LOGICAL_DNS"
            }

        self.assertEqual(set(clusters(disabled)), {"portal_auth_cluster"})
        enabled_clusters = clusters(enabled)
        self.assertEqual(set(enabled_clusters), {"portal_auth_cluster", "portal_auth_mtls_cluster"})
        mtls = enabled_clusters["portal_auth_mtls_cluster"]
        self.assertEqual(mtls["transport_socket"]["name"], "envoy.transport_sockets.tls")
        tls = mtls["transport_socket"]["typed_config"]
        self.assertEqual(
            tls["@type"],
            "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext",
        )
        context = tls["common_tls_context"]
        self.assertEqual(context["tls_params"], {
            "tls_minimum_protocol_version": "TLSv1_2",
            "tls_maximum_protocol_version": "TLSv1_3",
        })
        self.assertEqual(context["alpn_protocols"], ["istio-peer-exchange", "istio"])
        self.assertEqual(context["tls_certificate_sds_secret_configs"][0]["name"], "default")
        self.assertEqual(
            context["combined_validation_context"]["validation_context_sds_secret_config"]["name"],
            "ROOTCA",
        )
        self.assertEqual(
            context["combined_validation_context"]["default_validation_context"]["match_subject_alt_names"],
            [{"exact": "spiffe://cluster.local/ns/synthetic/sa/portal"}],
        )
        for sds in (
            context["tls_certificate_sds_secret_configs"][0]["sds_config"],
            context["combined_validation_context"]["validation_context_sds_secret_config"]["sds_config"],
        ):
            source = sds["api_config_source"]
            self.assertEqual(source["api_type"], "GRPC")
            self.assertEqual(source["grpc_services"], [{"envoy_grpc": {"cluster_name": "sds-grpc"}}])
            self.assertTrue(source["set_node_on_first_message_only"])
            self.assertEqual(source["transport_api_version"], "V3")
            self.assertEqual(sds["initial_fetch_timeout"], "0s")
            self.assertEqual(sds["resource_api_version"], "V3")
        self.assertEqual(
            tls["sni"],
            "outbound_.8081_._.cetirun-portal.cetirun-portal.svc.cluster.local",
        )
        self.assertNotIn("transport_socket_matches", mtls)
        self.assertNotIn("raw_buffer", json.dumps(mtls))

    @unittest.skipUnless(shutil.which("helm"), "requires Helm")
    def test_mtls_ownership_requires_a_well_formed_server_identity(self):
        missing = self._render_chart("--set", "portalAuth.mtls.enabled=true")
        self.assertNotEqual(missing.returncode, 0)
        self.assertIn("portalAuth.mtls.serverIdentity is required", missing.stderr)
        malformed = self._render_chart(
            "--set", "portalAuth.mtls.enabled=true",
            "--set-string", "portalAuth.mtls.serverIdentity=spiffe://cluster.local/not-an-identity",
        )
        self.assertNotEqual(malformed.returncode, 0)
        self.assertIn("portalAuth.mtls.serverIdentity must be an exact SPIFFE URI", malformed.stderr)

    def test_ownership_check_cannot_forward_or_trust_a_client_portal_token(self):
        # A supplied purpose header is cleared before the non-exempt branch.
        reset = self.source.index('request_handle:headers():remove("x-cetirun-portal-id-token")')
        ownership_call = self.source.index("request_handle:httpCall(", reset)
        self.assertLess(reset, ownership_call)

        # Portal denial responds at the gateway. The ID token is then never
        # routed to the Agent; successful requests remove it before routing.
        denial = self.source.index('request_handle:respond({[":status"] = "403"}', ownership_call)
        route_credentials = self.source.index(
            "forward_native_agent_api_key(request_handle, path_only, authority)", denial
        )
        self.assertLess(ownership_call, denial)
        self.assertLess(denial, route_credentials)
        forward_helper = self.source[
            self.source.index("function forward_native_agent_api_key"):self.source.index("function is_exempt")
        ]
        self.assertIn('local identity_token = headers:get("authorization")', forward_helper)
        self.assertIn('headers:remove("authorization")', forward_helper)

    def test_native_api_credentials_use_a_route_bound_header(self):
        self.assertIn(
            'local AGENT_API_KEY_HEADER = "x-cetirun-agent-api-key"', self.source
        )
        self.assertIn(
            'path:match("^/ui/[^/]+/v1/models$") ~= nil', self.source
        )
        self.assertIn(
            'path:match("^/ui/[^/]+/v1/chat/completions$") ~= nil', self.source
        )
        self.assertIn(
            'return path == "/v1/models" or path == "/v1/chat/completions"',
            self.source,
        )
        self.assertIn(
            "if is_native_agent_api_path(path, authority) and valid_instance_key(api_key) then\n"
            '                  headers:replace("authorization", "Bearer " .. api_key)',
            self.source,
        )
        self.assertNotIn("CETIRUN_AUTH_KEY_HEADER", self.source)
        self.assertNotIn("is_cetirun_auth_path", self.source)
        self.assertEqual(
            self.source.count(
                'request_handle:headers():remove(AGENT_API_KEY_HEADER)'
            ),
            3,
        )
        self.assertIn(
            'if not instance_name or instance_name == "" then\n'
            '                  -- Do not let credentials reach a fallback route',
            self.source,
        )

    def test_native_ui_routes_restore_only_the_browser_authorization(self):
        # The security headers filter runs before ext-auth and records the
        # client's Authorization in dynamic metadata, never in a header.
        self.assertIn("name: envoy.filters.http.ext_authz", self.security_headers)
        self.assertIn("operation: INSERT_BEFORE", self.security_headers)
        self.assertIn(
            'local authorization = handle:headers():get("authorization")\n'
            '                if authorization and authorization ~= "" then\n'
            '                  handle:streamInfo():dynamicMetadata():set("cetirun", "client_authorization", authorization)',
            self.security_headers,
        )
        self.assertNotIn("client_authorization", self.security_headers.split("function envoy_on_response")[1])

        # The ownership filter restores it only after the dedicated route
        # credentials, only on owned /ui/<instance>/ routes of the Portal
        # host other than the native Agent API, and never when OAuth2 Proxy
        # accepted it as the identity.
        self.assertIn(
            '                  headers:replace("authorization", "Bearer " .. api_key)\n'
            "                elseif is_native_ui_path(path, authority) and not is_native_agent_api_path(path, authority) then\n",
            self.source,
        )
        self.assertIn(
            "                  if client and not same_credential(client, identity_token) then\n"
            '                    headers:replace("authorization", client)',
            self.source,
        )
        self.assertIn(
            "return request_host(authority) == PORTAL_DOMAIN and\n"
            '                  path:match("^/ui/[^/]+/") ~= nil',
            self.source,
        )
        identity = self.source.index('local identity_token = headers:get("authorization")')
        removal = self.source.index('headers:remove("authorization")', identity)
        restore = self.source.index('headers:replace("authorization", client)', removal)
        self.assertLess(identity, removal)
        self.assertLess(removal, restore)
        self.assertEqual(self.source.count('headers:replace("authorization", client)'), 1)

        # same_credential and bearer_token, as Python.
        def bearer_token(value):
            match = re.fullmatch(r"\s*[Bb][Ee][Aa][Rr][Ee][Rr]\s+(\S+)\s*", value or "")
            return match.group(1) if match else None

        def same_credential(a, b):
            if not a or not b:
                return False
            if a == b:
                return True
            token = bearer_token(a)
            return token is not None and token == bearer_token(b)

        self.assertIn(
            'return value:match("^%s*[Bb][Ee][Aa][Rr][Ee][Rr]%s+(%S+)%s*$")',
            self.source,
        )
        cases = {
            ("Bearer agent-token", "Bearer id-token"): False,
            ("Bearer id-token", "Bearer id-token"): True,
            ("bearer id-token", "Bearer id-token"): True,
            ("Basic dXNlcjpwYXNz", "Bearer id-token"): False,
            (None, "Bearer id-token"): False,
            ("Bearer agent-token", None): False,
        }
        for (client, identity), expected in cases.items():
            with self.subTest(client=client, identity=identity):
                self.assertEqual(same_credential(client, identity), expected)


if __name__ == "__main__":
    unittest.main()
