# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import re
import unittest
from pathlib import Path


TEMPLATE = (
    Path(__file__).resolve().parents[2]
    / "infra/istio/templates/ingress/envoy-filter-security-headers.yaml"
)


class IstioSecurityHeadersTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = TEMPLATE.read_text(encoding="utf-8")
        cls.request, cls.response = cls.source.split("function envoy_on_response(handle)")

    def test_ui_csp_delegation_has_a_complete_instance_path(self):
        self.assertIn('path = path:match("^([^?]*)") or ""', self.response)
        self.assertIn('local is_ui_path = path:match("^/ui/[^/]+/") ~= nil', self.response)
        for path, expected in {
            "/ui/editor/": True,
            "/ui/editor/callback?code=synthetic": True,
            "/ui/chat-editor/": True,
            "/ui/editor/desktop/": True,
            "/ui/editor?next=/": False,
            "/ui": False,
            "/ui/": False,
            "/ui//": False,
            "/ui-other/editor/": False,
            "/api/v1/instances?next=/ui/editor/": False,
            "/?next=/ui/editor/": False,
        }.items():
            with self.subTest(path=path):
                self.assertEqual(bool(re.match(r"^/ui/[^/]+/", path.split("?", 1)[0])), expected)

    def test_ui_preserves_application_csp_before_legacy_iframe_rules(self):
        ui_branch = self.response.split("if is_ui_path then", 1)[1].split("elseif is_iframe_path then", 1)[0]
        self.assertNotIn("content-security-policy", ui_branch)
        self.assertIn('replace("x-frame-options", "{{ .Values.securityHeaders.xFrameOptions }}")', ui_branch)
        self.assertNotIn("content-security-policy-report-only", self.source)
        self.assertIn('replace("content-security-policy", "{{ .Values.securityHeaders.contentSecurityPolicy }}")', self.response)
        self.assertIn("frame-ancestors 'self'", self.response)

    def test_ui_does_not_skip_request_authentication_or_other_security_headers(self):
        self.assertNotIn("is_ui_path", self.request)
        self.assertIn('name: envoy.filters.http.ext_authz', self.request)
        self.assertIn('handle:headers():remove(h)', self.request)
        for header in ("x-auth-request-email", "x-auth-request-access-token", "x-internal-instance"):
            self.assertIn(f'"{header}"', self.request)
        ui_branch = self.response.index("if is_ui_path then")
        for header in ("strict-transport-security", "x-content-type-options", "referrer-policy"):
            self.assertLess(self.response.index(f'replace("{header}"'), ui_branch)
        self.assertIn('handle:headers():remove("server")', self.response)


if __name__ == "__main__":
    unittest.main()
