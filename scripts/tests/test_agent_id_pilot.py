# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import argparse
import importlib.util
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "agent_id_pilot.py"
SPEC = importlib.util.spec_from_file_location("agent_id_pilot", SCRIPT)
pilot = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(pilot)


class AgentIDPilotTests(unittest.TestCase):
    def _resume_args(self, state_file):
        return argparse.Namespace(
            state_file=state_file,
            prefix="synthetic-pilot",
            namespace="cetirun-synthetic",
            service_account="synthetic-pilot-sa",
            oidc_issuer="https://issuer.example.test/tenant/cluster/",
            portal_origin="https://portal.example.test/",
            echo_host="echo.example.test",
            graph_user_read_scope_id="e1fe6dd8-ba31-4d61-89e7-88639da4683d",
            resume=True,
        )

    def _partial_pilot_state(self):
        return {
            "schema": 1,
            "tenantId": "00000000-0000-4000-8000-000000000001",
            "provisioningClient": {
                "clientId": "00000000-0000-4000-8000-000000000002"
            },
            "pilot": {
                "prefix": "synthetic-pilot",
                "namespace": "cetirun-synthetic",
                "serviceAccount": "synthetic-pilot-sa",
                "oidcIssuer": "https://issuer.example.test/tenant/cluster",
                "portalOrigin": "https://portal.example.test",
                "echoHost": "echo.example.test",
                "serviceAccountCreated": True,
                "echoApplication": {
                    "objectId": "00000000-0000-4000-8000-000000000003",
                    "clientId": "00000000-0000-4000-8000-000000000004",
                    "scopeId": "00000000-0000-4000-8000-000000000005",
                    "servicePrincipalObjectId": "00000000-0000-4000-8000-000000000006",
                },
                "blueprint": {
                    "objectId": "00000000-0000-4000-8000-000000000007",
                    "clientId": "00000000-0000-4000-8000-000000000008",
                    "scopeId": "00000000-0000-4000-8000-000000000009",
                },
            },
        }

    def _deployment_args(self, state_file):
        return argparse.Namespace(
            state_file=state_file,
            prefix="synthetic-pilot",
            resume=False,
            subscription_id="00000000-0000-4000-8000-000000000001",
            resource_group="synthetic-rg",
            location="example-region",
            oidc_issuer="https://issuer.example.test/cluster",
            portal_origin="https://portal.example.test",
            agent_broker_namespace="cetirun-agent-broker",
            agent_broker_service_account="cetirun-agent-broker",
            operator_namespace="cetirun-system",
            operator_service_account="cetirun-operator-controller-manager",
            encryption_identity_name="synthetic-encryption",
            key_vault_name="synthetickeyvault",
        )

    def test_state_file_must_be_outside_checkout(self):
        with self.assertRaisesRegex(pilot.PilotError, "outside"):
            pilot.validate_state_path(pilot.ROOT / "state.json")

    def test_state_file_is_private_and_round_trips(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            value = {"schema": 1, "pilot": {"prefix": "synthetic-pilot"}}
            pilot.save_state(path, value)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(pilot.load_state(path), value)

    def test_state_file_rejects_group_readability(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            path.write_text("{}", encoding="utf-8")
            os.chmod(path, 0o640)
            with self.assertRaisesRegex(pilot.PilotError, "0600"):
                pilot.load_state(path)

    def test_graph_error_does_not_include_sensitive_body(self):
        body = json.dumps(
            {
                "error": {
                    "code": "Authorization_RequestDenied",
                    "message": "token-and-private-object-id",
                }
            }
        ).encode()
        http_error = pilot.error.HTTPError(
            "https://graph.microsoft.com/v1.0/applications",
            403,
            "Forbidden",
            {},
            mock.Mock(read=mock.Mock(return_value=body)),
        )
        with mock.patch.object(pilot.request, "urlopen", side_effect=http_error):
            with self.assertRaises(pilot.PilotError) as context:
                pilot.HttpClient().call(
                    "POST",
                    "https://graph.microsoft.com/v1.0/applications",
                    json_body={"displayName": "synthetic"},
                )
        message = str(context.exception)
        self.assertIn("Authorization_RequestDenied", message)
        self.assertNotIn("token-and-private-object-id", message)

    def test_http_client_accepts_expected_404_for_cleanup_retry(self):
        http_error = pilot.error.HTTPError(
            "https://graph.microsoft.com/v1.0/applications/synthetic",
            404,
            "Not Found",
            {},
            mock.Mock(read=mock.Mock(return_value=b"")),
        )
        with mock.patch.object(pilot.request, "urlopen", side_effect=http_error):
            result = pilot.HttpClient().call(
                "DELETE",
                "https://graph.microsoft.com/v1.0/applications/synthetic",
                expected=(204, 404),
            )
        self.assertEqual(result, {})

    def test_oauth_error_code_is_safely_extracted(self):
        body = b'{"error":"authorization_pending","error_description":"private"}'
        self.assertEqual(pilot._safe_error_code(body), "authorization_pending")

    def test_provisioning_scope_contract(self):
        self.assertEqual(
            set(pilot.PROVISIONING_SCOPES),
            {
                "AgentIdentityBlueprint.Create",
                "AgentIdentityBlueprint.AddRemoveCreds.All",
                "AgentIdentityBlueprint.UpdateAuthProperties.All",
                "AgentIdentityBlueprintPrincipal.Create",
                "Application.ReadWrite.All",
                "DelegatedPermissionGrant.ReadWrite.All",
                "User.Read",
            },
        )

    def test_bootstrap_consent_is_scoped_to_current_operator(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            operator_id = "00000000-0000-4000-8000-000000000010"
            graph = mock.Mock()
            graph.call.side_effect = [
                {"id": operator_id},
                {
                    "value": [
                        {
                            "id": "00000000-0000-4000-8000-000000000011",
                            "oauth2PermissionScopes": [
                                {"value": scope, "id": f"00000000-0000-4000-8000-{index:012d}"}
                                for index, scope in enumerate(pilot.PROVISIONING_SCOPES, 20)
                            ],
                        }
                    ]
                },
                {
                    "id": "00000000-0000-4000-8000-000000000012",
                    "appId": "00000000-0000-4000-8000-000000000013",
                },
                {"id": "00000000-0000-4000-8000-000000000014"},
                {"id": "opaque-graph_grant.id~15"},
            ]
            args = argparse.Namespace(
                state_file=path,
                prefix="synthetic-pilot",
                tenant_id="00000000-0000-4000-8000-000000000001",
                bootstrap_client_id="00000000-0000-4000-8000-000000000099",
            )
            with mock.patch.object(pilot, "device_code_token", return_value="private-token") as token:
                with mock.patch.object(pilot, "GraphClient", return_value=graph):
                    pilot.bootstrap_client(args)
            token.assert_called_once_with(
                args.tenant_id, args.bootstrap_client_id, pilot.PROVISIONING_SCOPES
            )
            grant = graph.call.call_args_list[4]
            self.assertEqual(grant.args[:2], ("POST", "/oauth2PermissionGrants"))
            self.assertEqual(grant.kwargs["body"]["consentType"], "Principal")
            self.assertEqual(grant.kwargs["body"]["principalId"], operator_id)
            self.assertEqual(
                pilot.load_state(path)["provisioningClient"]["permissionGrantObjectId"],
                "opaque-graph_grant.id~15",
            )

    def test_cleanup_deletes_provisioning_grant_principal_then_application(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            pilot.save_state(
                path,
                {
                    "schema": 1,
                    "tenantId": "00000000-0000-4000-8000-000000000001",
                    "deployment": {
                        "subscriptionId": "00000000-0000-4000-8000-000000000002"
                    },
                    "provisioningClient": {
                        "clientId": "00000000-0000-4000-8000-000000000003",
                        "permissionGrantObjectId": "opaque-graph_grant.id~4",
                        "servicePrincipalObjectId": "00000000-0000-4000-8000-000000000005",
                        "applicationObjectId": "00000000-0000-4000-8000-000000000006",
                    },
                },
            )
            graph = mock.Mock()
            with mock.patch.object(pilot, "_operator_graph", return_value=graph):
                pilot.cleanup_deployment(argparse.Namespace(state_file=path))
            self.assertEqual(
                [call.args[1] for call in graph.call.call_args_list],
                [
                    "/oauth2PermissionGrants/opaque-graph_grant.id~4",
                    "/servicePrincipals/00000000-0000-4000-8000-000000000005",
                    "/applications/00000000-0000-4000-8000-000000000006",
                ],
            )
            self.assertEqual(pilot.load_state(path)["provisioningClient"], {})

    def test_cleanup_retries_incomplete_provisioning_client_deletion(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            pilot.save_state(
                path,
                {
                    "schema": 1,
                    "tenantId": "00000000-0000-4000-8000-000000000001",
                    "deployment": {
                        "subscriptionId": "00000000-0000-4000-8000-000000000002"
                    },
                    "provisioningClient": {
                        "clientId": "00000000-0000-4000-8000-000000000003",
                        "permissionGrantObjectId": "opaque-graph_grant.id~4",
                        "servicePrincipalObjectId": "00000000-0000-4000-8000-000000000005",
                        "applicationObjectId": "00000000-0000-4000-8000-000000000006",
                    },
                },
            )
            first_graph = mock.Mock()
            first_graph.call.side_effect = [
                {},
                {},
                pilot.PilotError("synthetic application deletion failure"),
            ]
            second_graph = mock.Mock()
            with mock.patch.object(
                pilot, "_operator_graph", side_effect=[first_graph, second_graph]
            ):
                with self.assertRaisesRegex(
                    pilot.PilotError, "synthetic application deletion failure"
                ):
                    pilot.cleanup_deployment(argparse.Namespace(state_file=path))

                partial = pilot.load_state(path)
                self.assertTrue(partial["deployment"])
                self.assertEqual(
                    partial["provisioningClient"]["applicationObjectId"],
                    "00000000-0000-4000-8000-000000000006",
                )
                pilot.cleanup_deployment(argparse.Namespace(state_file=path))

            second_graph.call.assert_called_once_with(
                "DELETE",
                "/applications/00000000-0000-4000-8000-000000000006",
                expected=(204, 404),
            )
            cleaned = pilot.load_state(path)
            self.assertEqual(cleaned["deployment"], {})
            self.assertEqual(cleaned["provisioningClient"], {})

    def test_cleanup_skips_unrecorded_identity_after_early_provisioning_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            pilot.save_state(path, {"schema": 1})
            with mock.patch.object(
                pilot,
                "_operator_graph",
                side_effect=pilot.PilotError("synthetic Graph failure"),
            ):
                with mock.patch.object(
                    pilot, "_az_json", return_value={"identities": []}
                ):
                    with self.assertRaisesRegex(
                        pilot.PilotError, "synthetic Graph failure"
                    ):
                        pilot.provision_deployment(self._deployment_args(path))

            self.assertNotIn(
                "encryptionIdentity", pilot.load_state(path)["deployment"]
            )
            graph = mock.Mock()
            with mock.patch.object(pilot, "_operator_graph", return_value=graph):
                with mock.patch.object(pilot, "_run_no_output") as run_no_output:
                    pilot.cleanup_deployment(argparse.Namespace(state_file=path))
            run_no_output.assert_not_called()

    def test_cleanup_deletes_recorded_identity_by_resource_id(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            resource_id = (
                "/subscriptions/00000000-0000-4000-8000-000000000001/"
                "resourceGroups/synthetic-rg/providers/Microsoft.ManagedIdentity/"
                "userAssignedIdentities/synthetic-encryption"
            )
            pilot.save_state(
                path,
                {
                    "schema": 1,
                    "deployment": {
                        "subscriptionId": "00000000-0000-4000-8000-000000000001",
                        "resourceGroup": "synthetic-rg",
                        "encryptionIdentityName": "untrusted-input-name",
                        "encryptionIdentity": {"resourceId": resource_id},
                    },
                },
            )
            graph = mock.Mock()
            with mock.patch.object(pilot, "_operator_graph", return_value=graph):
                with mock.patch.object(pilot, "_run_no_output") as run_no_output:
                    pilot.cleanup_deployment(argparse.Namespace(state_file=path))
            run_no_output.assert_called_once_with(
                ["az", "identity", "delete", "--subscription",
                 "00000000-0000-4000-8000-000000000001", "--ids", resource_id]
            )

    def test_provisioning_rejects_preexisting_unowned_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            pilot.save_state(path, {"schema": 1})
            args = self._deployment_args(path)
            resource_id = pilot._managed_identity_resource_id(
                args.subscription_id,
                args.resource_group,
                args.encryption_identity_name,
            )
            with mock.patch.object(
                pilot,
                "_az_json",
                return_value={
                    "identities": [
                        {
                            "id": resource_id,
                            "name": args.encryption_identity_name,
                        }
                    ]
                },
            ) as az_json:
                with mock.patch.object(pilot, "_operator_graph") as operator_graph:
                    with self.assertRaisesRegex(
                        pilot.PilotError, "without a deployment ownership record"
                    ):
                        pilot.provision_deployment(args)

            az_json.assert_called_once_with(
                "identity", "list",
                "--subscription", args.subscription_id,
                "--resource-group", args.resource_group,
                "--query", "{identities: @}",
            )
            operator_graph.assert_not_called()
            self.assertNotIn("deployment", pilot.load_state(path))

    def test_deployment_arm_commands_use_requested_subscription(self):
        for operation, previously_created in (("create", False), ("update", True)):
            with self.subTest(operation=operation):
                with tempfile.TemporaryDirectory() as directory:
                    path = Path(directory) / "state.json"
                    args = self._deployment_args(path)
                    args.resume = True
                    deployment = {
                        "prefix": args.prefix,
                        "subscriptionId": args.subscription_id,
                        "resourceGroup": args.resource_group,
                        "location": args.location,
                        "oidcIssuer": args.oidc_issuer,
                        "portalOrigin": args.portal_origin,
                        "agentBrokerNamespace": args.agent_broker_namespace,
                        "agentBrokerServiceAccount": args.agent_broker_service_account,
                        "operatorNamespace": args.operator_namespace,
                        "operatorServiceAccount": args.operator_service_account,
                        "encryptionIdentityName": args.encryption_identity_name,
                        "keyVaultName": args.key_vault_name,
                        "ownerObjectId": "00000000-0000-4000-8000-000000000010",
                        "armPermissionGrantId": "synthetic-grant",
                        "blueprint": {
                            "objectId": "00000000-0000-4000-8000-000000000013",
                            "clientId": "00000000-0000-4000-8000-000000000014",
                            "scopeId": "00000000-0000-4000-8000-000000000015",
                            "principalObjectId": "00000000-0000-4000-8000-000000000016",
                            "authorizationPropertiesConfigured": True,
                            "armInheritanceCreated": True,
                            "federatedCredentialId": "synthetic-broker-fic",
                            "federatedCredentialIssuer": args.oidc_issuer,
                            "operatorFederatedCredentialId": "synthetic-operator-fic",
                            "operatorFederatedCredentialIssuer": args.oidc_issuer,
                        },
                        "authorizationClient": {
                            "objectId": "00000000-0000-4000-8000-000000000017",
                            "clientId": "00000000-0000-4000-8000-000000000018",
                            "principalObjectId": "00000000-0000-4000-8000-000000000019",
                            "federatedCredentialId": "synthetic-authorization-fic",
                            "federatedCredentialIssuer": args.oidc_issuer,
                        },
                        "encryptionIdentity": {
                            "resourceId": pilot._managed_identity_resource_id(
                                args.subscription_id,
                                args.resource_group,
                                args.encryption_identity_name,
                            ),
                            "clientId": "00000000-0000-4000-8000-000000000011",
                            "principalId": "00000000-0000-4000-8000-000000000012",
                            "federatedCredentialCreated": previously_created,
                        },
                    }
                    pilot.save_state(path, {"schema": 1, "deployment": deployment})
                    vault_id = (
                        f"/subscriptions/{args.subscription_id}/resourceGroups/{args.resource_group}"
                        f"/providers/Microsoft.KeyVault/vaults/{args.key_vault_name}"
                    )

                    def az_json(*arguments):
                        if arguments[:3] == (
                            "identity",
                            "federated-credential",
                            operation,
                        ):
                            return {}
                        if arguments[:2] == ("keyvault", "create"):
                            return {"id": vault_id}
                        if arguments[:3] == ("role", "assignment", "create"):
                            return {"id": vault_id + "/providers/Microsoft.Authorization/roleAssignments/synthetic"}
                        if arguments[:3] == ("keyvault", "key", "create"):
                            return {"key": {"kid": "https://vault.example.test/keys/authorization-cache/version"}}
                        self.fail(f"Unexpected Azure operation: {arguments[:3]}")

                    with mock.patch.object(
                        pilot, "_az_json", side_effect=az_json
                    ) as az_json_mock, mock.patch.object(pilot, "_run_no_output") as run_no_output:
                        pilot.provision_deployment(args)

                    federated_call = az_json_mock.call_args_list[0].args
                    self.assertEqual(
                        federated_call[:5],
                        (
                            "identity",
                            "federated-credential",
                            operation,
                            "--subscription",
                            args.subscription_id,
                        ),
                    )
                    for call in az_json_mock.call_args_list:
                        arguments = call.args
                        self.assertIn("--subscription", arguments)
                        self.assertEqual(arguments[arguments.index("--subscription") + 1], args.subscription_id)
                    run_no_output.assert_called_once()
                    cleanup = run_no_output.call_args.args[0]
                    self.assertEqual(cleanup[1:4], ["role", "assignment", "delete"])
                    self.assertEqual(cleanup[cleanup.index("--subscription") + 1], args.subscription_id)
                    self.assertTrue(pilot.load_state(path)["deployment"]["provisioningComplete"])

    def test_graph_permission_catalog_encodes_odata_query(self):
        graph = mock.Mock()
        graph.call.return_value = {
            "value": [
                {
                    "id": "graph-principal",
                    "oauth2PermissionScopes": [{"value": "User.Read", "id": "scope"}],
                }
            ]
        }
        principal, catalog = pilot._graph_permission_catalog(graph)
        self.assertEqual(principal, "graph-principal")
        self.assertEqual(catalog, {"User.Read": "scope"})
        path = graph.call.call_args.args[1]
        self.assertNotIn(" ", path)
        self.assertIn("%24filter=appId%20eq%20%27", path)

    def test_incomplete_bootstrap_adopts_opaque_permission_grant_id(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            state = {
                "provisioningClient": {
                    "servicePrincipalObjectId": "00000000-0000-4000-8000-000000000011",
                    "operatorObjectId": "00000000-0000-4000-8000-000000000012",
                    "bootstrapIncomplete": True,
                }
            }
            pilot.save_state(path, state)
            graph = mock.Mock()
            graph.call.side_effect = [
                {
                    "value": [
                        {
                            "id": "00000000-0000-4000-8000-000000000013",
                            "oauth2PermissionScopes": [],
                        }
                    ]
                },
                {
                    "value": [
                        {
                            "id": "opaque-graph_grant.id~14",
                            "principalId": "00000000-0000-4000-8000-000000000012",
                            "scope": " ".join(reversed(pilot.PROVISIONING_SCOPES)),
                        }
                    ]
                },
            ]
            pilot._repair_incomplete_bootstrap(graph, path, state)
            repaired = pilot.load_state(path)["provisioningClient"]
            self.assertEqual(repaired["permissionGrantObjectId"], "opaque-graph_grant.id~14")
            self.assertTrue(repaired["adminConsentGranted"])
            self.assertNotIn("bootstrapIncomplete", repaired)

    def test_deployment_graph_checkpoint_requires_every_identity_binding(self):
        issuer = "https://issuer.example.test/tenant/cluster/"
        deployment = {
            "oidcIssuer": issuer,
            "ownerObjectId": "owner",
            "armPermissionGrantId": "grant",
            "blueprint": {
                "principalObjectId": "principal",
                "authorizationPropertiesConfigured": True,
                "armInheritanceCreated": True,
                "federatedCredentialId": "credential-fic",
                "federatedCredentialIssuer": issuer,
                "operatorFederatedCredentialId": "operator-fic",
                "operatorFederatedCredentialIssuer": issuer,
            },
            "authorizationClient": {
                "principalObjectId": "authorization-principal",
                "federatedCredentialId": "authorization-fic",
                "federatedCredentialIssuer": issuer,
            },
        }
        self.assertTrue(pilot._deployment_graph_is_complete(deployment))
        deployment["blueprint"]["operatorFederatedCredentialIssuer"] = issuer.rstrip("/")
        self.assertFalse(pilot._deployment_graph_is_complete(deployment))

    @mock.patch.object(pilot.time, "sleep")
    def test_device_flow_qualifies_graph_scopes(self, _sleep):
        http = mock.Mock()
        http.call.side_effect = [
            {
                "device_code": "private-device-code",
                "user_code": "one-time-code",
                "verification_uri": "https://microsoft.com/devicelogin",
                "interval": 0,
                "expires_in": 60,
            },
            {"access_token": "private-access-token"},
        ]
        with mock.patch.object(pilot, "HttpClient", return_value=http):
            with mock.patch.object(pilot.sys, "stderr"):
                token = pilot.device_code_token(
                    "00000000-0000-4000-8000-000000000001",
                    "00000000-0000-4000-8000-000000000002",
                    ("User.Read", "https://graph.microsoft.com/Scope.Two"),
                )
        self.assertEqual(token, "private-access-token")
        flow_form = http.call.call_args_list[0].kwargs["form"]
        self.assertEqual(
            flow_form["scope"],
            "https://graph.microsoft.com/User.Read "
            "https://graph.microsoft.com/Scope.Two",
        )

    def test_prefix_rejects_shell_metacharacters(self):
        for value in ("short", "pilot;rm", "Pilot-name", "pilot_name"):
            with self.subTest(value=value):
                with self.assertRaises(pilot.PilotError):
                    pilot.validate_prefix(value)

    def test_portal_popup_redirect_uri_uses_static_callback(self):
        self.assertEqual(
            pilot.portal_popup_redirect_uri("https://portal.example.test/"),
            "https://portal.example.test/agent-id-popup-callback.html",
        )

    def test_completed_pilot_reconciles_popup_redirect_uri(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            state = self._partial_pilot_state()
            state["pilot"]["spaApplication"] = {
                "objectId": "00000000-0000-4000-8000-000000000011",
                "clientId": "00000000-0000-4000-8000-000000000012",
            }
            state["pilot"]["provisioningComplete"] = True
            pilot.save_state(path, state)
            graph = mock.Mock()
            graph.call.side_effect = [
                {"id": "00000000-0000-4000-8000-000000000010"},
                {},
            ]

            with mock.patch.object(pilot, "_operator_graph", return_value=graph):
                pilot.provision(self._resume_args(path))

            update = graph.call.call_args_list[1]
            self.assertEqual(
                update.args[:2],
                (
                    "PATCH",
                    "/applications/00000000-0000-4000-8000-000000000011",
                ),
            )
            self.assertEqual(
                update.kwargs["body"],
                {
                    "spa": {
                        "redirectUris": [
                            "https://portal.example.test/agent-id-popup-callback.html"
                        ]
                    }
                },
            )
            updated = pilot.load_state(path)
            self.assertTrue(
                updated["pilot"]["spaApplication"]["popupRedirectUriConfigured"]
            )

    def test_resume_rejects_environment_mismatch_before_login(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            pilot.save_state(path, self._partial_pilot_state())
            args = self._resume_args(path)
            args.namespace = "cetirun-wrong"
            with mock.patch.object(pilot, "_operator_graph") as operator_graph:
                with self.assertRaisesRegex(pilot.PilotError, "do not match"):
                    pilot.provision(args)
            operator_graph.assert_not_called()

    def test_resume_uses_blueprint_client_id_for_authorization_update(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "state.json"
            pilot.save_state(path, self._partial_pilot_state())
            graph = mock.Mock()
            patch_paths = []

            def graph_call(method, request_path, **_kwargs):
                if method == "GET" and request_path.startswith("/me?"):
                    return {"id": "00000000-0000-4000-8000-000000000010"}
                if method == "PATCH":
                    patch_paths.append(request_path)
                    self.assertNotIn(
                        "requestedAccessTokenVersion", _kwargs["body"]["api"]
                    )
                    self.assertNotIn(
                        "preAuthorizedApplications", _kwargs["body"]["api"]
                    )
                    return {}
                if method == "POST" and request_path.startswith(
                    "/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal"
                ):
                    raise pilot.PilotError("stop after authorization update")
                self.fail(f"unexpected Graph call: {method} {request_path}")

            graph.call.side_effect = graph_call
            with mock.patch.object(pilot, "_operator_graph", return_value=graph):
                with self.assertRaisesRegex(pilot.PilotError, "stop after"):
                    pilot.provision(self._resume_args(path))
            self.assertEqual(
                patch_paths,
                ["/applications/00000000-0000-4000-8000-000000000008"],
            )

    def test_existing_fic_is_updated_when_issuer_trailing_slash_differs(self):
        graph = mock.Mock()
        graph.call.side_effect = [
            {
                "value": [
                    {
                        "id": "00000000-0000-4000-8000-000000000011",
                        "name": "synthetic-pilot-aks",
                        "issuer": "https://issuer.example.test/tenant/cluster",
                        "subject": "system:serviceaccount:namespace:service-account",
                        "audiences": [pilot.TOKEN_EXCHANGE_AUDIENCE],
                    }
                ]
            },
            {},
        ]
        blueprint = {
            "objectId": "00000000-0000-4000-8000-000000000007",
            "clientId": "00000000-0000-4000-8000-000000000008",
            "federatedCredentialCreated": True,
        }
        pilot._ensure_blueprint_fic(
            graph,
            blueprint,
            name="synthetic-pilot-aks",
            issuer="https://issuer.example.test/tenant/cluster/",
            subject="system:serviceaccount:namespace:service-account",
        )
        update = graph.call.call_args_list[1]
        self.assertEqual(update.args[0], "PATCH")
        self.assertEqual(
            update.args[1],
            "/applications/00000000-0000-4000-8000-000000000008/"
            "federatedIdentityCredentials/00000000-0000-4000-8000-000000000011",
        )
        self.assertTrue(update.kwargs["body"]["issuer"].endswith("/"))
        self.assertTrue(blueprint["federatedCredentialIssuerExact"])
        self.assertEqual(
            blueprint["federatedCredentialIssuer"],
            "https://issuer.example.test/tenant/cluster/",
        )

    def test_deployment_parser_requires_explicit_azure_targets(self):
        args = pilot.parser().parse_args(
            [
                "provision-deployment",
                "--state-file", "/tmp/private-state.json",
                "--subscription-id", "00000000-0000-4000-8000-000000000001",
                "--resource-group", "synthetic-rg",
                "--location", "example-region",
                "--oidc-issuer", "https://issuer.example.test/cluster",
                "--portal-origin", "https://portal.example.test",
                "--agent-broker-namespace", "cetirun-agent-broker",
                "--agent-broker-service-account", "cetirun-agent-broker",
                "--operator-namespace", "cetirun-system",
                "--operator-service-account", "cetirun-operator-controller-manager",
                "--encryption-identity-name", "synthetic-encryption",
                "--key-vault-name", "synthetickeyvault",
            ]
        )
        self.assertIs(args.function, pilot.provision_deployment)
        self.assertEqual(args.key_vault_name, "synthetickeyvault")

    def test_application_fic_is_bound_to_exact_service_account(self):
        graph = mock.Mock()
        graph.call.side_effect = [
            {"value": []},
            {"id": "00000000-0000-4000-8000-000000000099"},
        ]
        application = {"objectId": "00000000-0000-4000-8000-000000000098"}
        pilot._ensure_application_fic(
            graph,
            application,
            name="synthetic-credential",
            issuer="https://issuer.example.test/cluster/",
            subject="system:serviceaccount:cetirun-agent-broker:cetirun-agent-broker",
        )
        create = graph.call.call_args_list[1]
        self.assertEqual(
            create.args[1],
            "/applications/00000000-0000-4000-8000-000000000098/federatedIdentityCredentials",
        )
        self.assertEqual(
            create.kwargs["body"]["subject"],
            "system:serviceaccount:cetirun-agent-broker:cetirun-agent-broker",
        )
        self.assertEqual(create.kwargs["body"]["audiences"], [pilot.TOKEN_EXCHANGE_AUDIENCE])
        self.assertEqual(
            create.kwargs["body"]["issuer"],
            "https://issuer.example.test/cluster/",
        )
        self.assertEqual(
            application["federatedCredentialIssuer"],
            "https://issuer.example.test/cluster/",
        )

    def test_inheritable_permission_uses_documented_object_id_path_and_adopts(self):
        graph = mock.Mock()
        graph.call.return_value = {
            "value": [
                {
                    "resourceAppId": pilot.ARM_APP_ID,
                    "inheritableScopes": {
                        "@odata.type": "#microsoft.graph.allAllowedScopes"
                    },
                }
            ]
        }
        blueprint_object_id = "00000000-0000-4000-8000-000000000097"
        pilot._ensure_inheritable_permission(
            graph, blueprint_object_id, pilot.ARM_APP_ID
        )
        graph.call.assert_called_once_with(
            "GET",
            f"/applications/{blueprint_object_id}/"
            "microsoft.graph.agentIdentityBlueprint/inheritablePermissions",
        )

    def test_inheritable_permission_create_is_resume_safe(self):
        graph = mock.Mock()
        graph.call.side_effect = [
            {"value": []},
            {"resourceAppId": pilot.ARM_APP_ID},
        ]
        blueprint_object_id = "00000000-0000-4000-8000-000000000097"
        pilot._ensure_inheritable_permission(
            graph, blueprint_object_id, pilot.ARM_APP_ID
        )
        create = graph.call.call_args_list[1]
        self.assertEqual(create.args[0], "POST")
        self.assertIn(blueprint_object_id, create.args[1])
        self.assertNotIn("inheritableRoles", create.kwargs["body"])

    def test_permission_grant_adopts_only_exact_scope(self):
        graph = mock.Mock()
        graph.call.return_value = {
            "value": [
                {
                    "id": "opaque-graph_grant.id~96",
                    "principalId": None,
                    "scope": "user_impersonation",
                }
            ]
        }
        grant_id = pilot._ensure_permission_grant(
            graph,
            client_id="00000000-0000-4000-8000-000000000095",
            resource_id="00000000-0000-4000-8000-000000000094",
            scope="user_impersonation",
            consent_type="AllPrincipals",
            principal_id=None,
        )
        self.assertEqual(grant_id, "opaque-graph_grant.id~96")
        self.assertEqual(graph.call.call_count, 1)
        self.assertNotIn(" ", graph.call.call_args.args[1])

    def test_permission_grant_rejects_broader_existing_scope(self):
        graph = mock.Mock()
        graph.call.return_value = {
            "value": [
                {
                    "id": "00000000-0000-4000-8000-000000000096",
                    "principalId": None,
                    "scope": "user_impersonation additional.scope",
                }
            ]
        }
        with self.assertRaisesRegex(pilot.PilotError, "unexpected scopes"):
            pilot._ensure_permission_grant(
                graph,
                client_id="00000000-0000-4000-8000-000000000095",
                resource_id="00000000-0000-4000-8000-000000000094",
                scope="user_impersonation",
                consent_type="AllPrincipals",
                principal_id=None,
            )


if __name__ == "__main__":
    unittest.main()
