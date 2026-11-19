# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline exec-policy renders; installation bindings are not Chart values."""

import unittest

from test_cetiadmin import objects, ordinary_values, prepared_values, render


class ExecPolicyTests(unittest.TestCase):
    def assert_restricted(self, values):
        result = render(values)
        self.assertEqual(result.returncode, 0, result.stderr)
        policy = next(obj for obj in objects(result) if obj["kind"] == "ValidatingAdmissionPolicy")
        binding = next(obj for obj in objects(result) if obj["kind"] == "ValidatingAdmissionPolicyBinding")
        self.assertNotIn("matchConditions", policy["spec"])
        self.assertEqual(policy["spec"]["failurePolicy"], "Fail")
        self.assertEqual(policy["spec"]["validations"][0]["expression"], "false")
        self.assertEqual(policy["spec"]["matchConstraints"]["resourceRules"], [{
            "apiGroups": [""], "apiVersions": ["v1"], "operations": ["CONNECT"], "resources": ["pods/exec"],
        }])
        self.assertEqual(binding["spec"], {"policyName": policy["metadata"]["name"], "validationActions": ["Deny"]})
        self.assertNotIn("exclude-adminclaw", result.stdout)
        self.assertNotIn("system:serviceaccount:cetirun-admin:openclaw-admin-cetirun", result.stdout)

    def test_native_defaults_have_no_admin_exception(self):
        for runtime in ("hermes", "openclaw"):
            with self.subTest(runtime=runtime):
                values = ordinary_values()
                values["agentType"] = runtime
                self.assert_restricted(values)

    def test_prepared_cetiadmin_does_not_grant_an_exception(self):
        self.assert_restricted(prepared_values())

    def test_values_cannot_inject_match_conditions(self):
        values = ordinary_values()
        values["execRestriction"] = {"enabled": True, "matchConditions": [{"name": "skip", "expression": "false"}]}
        result = render(values)
        if result.returncode == 0:
            self.assert_restricted(values)
        else:
            self.assertEqual(result.stdout, "")

    def test_disabled_keeps_existing_behavior(self):
        values = ordinary_values()
        values["execRestriction"] = {"enabled": False}
        result = render(values)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(obj["kind"].startswith("ValidatingAdmissionPolicy") for obj in objects(result)))


if __name__ == "__main__":
    unittest.main()
