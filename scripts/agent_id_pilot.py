#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Provision a short-lived Microsoft Entra Agent ID pilot safely.

The script deliberately separates the retained provisioning client from each
short-lived pilot. OAuth tokens and Kubernetes assertions remain in memory.
The state file contains identifiers required for precise cleanup and therefore
must live outside the source tree with mode 0600.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile
import time
from typing import Any
from urllib import error, parse, request
import uuid


ROOT = Path(__file__).resolve().parents[1]
GRAPH_V1 = "https://graph.microsoft.com/v1.0"
GRAPH_BETA = "https://graph.microsoft.com/beta"
GRAPH_APP_ID = "00000003-0000-0000-c000-000000000000"
ARM_APP_ID = "797f4846-ba00-4fd7-ba43-dac1f8f63013"
# Public client ID of Microsoft Graph PowerShell. It is used only for the
# interactive bootstrap token and is never granted credentials by this script.
GRAPH_POWERSHELL_CLIENT_ID = "14d82eec-204b-4c2f-b7e8-296a70dab67e"
TOKEN_ASSERTION_TYPE = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
TOKEN_EXCHANGE_AUDIENCE = "api://AzureADTokenExchange"
PUBLIC_CLIENT_REDIRECT = "https://login.microsoftonline.com/common/oauth2/nativeclient"
AGENT_ID_POPUP_CALLBACK_PATH = "/agent-id-popup-callback.html"
PREFIX_RE = re.compile(r"^[a-z0-9][a-z0-9-]{4,54}[a-z0-9]$")
UUID_RE = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
)
GRAPH_ID_RE = re.compile(r"^[A-Za-z0-9._~-]{1,512}$")

PROVISIONING_SCOPES = (
    "AgentIdentityBlueprint.Create",
    "AgentIdentityBlueprint.AddRemoveCreds.All",
    "AgentIdentityBlueprint.UpdateAuthProperties.All",
    "AgentIdentityBlueprintPrincipal.Create",
    "Application.ReadWrite.All",
    "DelegatedPermissionGrant.ReadWrite.All",
    "User.Read",
)


class PilotError(RuntimeError):
    """An operator-safe error that contains no token or response body."""


def _json_bytes(value: dict[str, Any]) -> bytes:
    return json.dumps(value, separators=(",", ":")).encode("utf-8")


def _safe_error_code(body: bytes) -> str:
    try:
        value = json.loads(body.decode("utf-8"))
        error_value = value.get("error", "")
        code = error_value.get("code", "") if isinstance(error_value, dict) else error_value
        if isinstance(code, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,100}", code):
            return code
    except (UnicodeDecodeError, json.JSONDecodeError, AttributeError):
        pass
    return "unknown"


class HttpClient:
    def call(
        self,
        method: str,
        url: str,
        *,
        headers: dict[str, str] | None = None,
        json_body: dict[str, Any] | None = None,
        form: dict[str, str] | None = None,
        expected: tuple[int, ...] = (200,),
    ) -> dict[str, Any]:
        request_headers = dict(headers or {})
        data = None
        if json_body is not None:
            request_headers["Content-Type"] = "application/json"
            data = _json_bytes(json_body)
        elif form is not None:
            request_headers["Content-Type"] = "application/x-www-form-urlencoded"
            data = parse.urlencode(form).encode("ascii")
        outbound = request.Request(url, data=data, headers=request_headers, method=method)
        try:
            with request.urlopen(outbound, timeout=30) as response:
                body = response.read()
                status_code = response.status
        except error.HTTPError as exc:
            body = exc.read(65536)
            status_code = exc.code
            if status_code not in expected:
                raise PilotError(
                    f"HTTP {status_code} from {parse.urlsplit(url).netloc} "
                    f"({_safe_error_code(body)})"
                ) from None
        except (error.URLError, TimeoutError) as exc:
            raise PilotError(f"request to {parse.urlsplit(url).netloc} failed") from exc
        if status_code not in expected:
            raise PilotError(f"unexpected HTTP {status_code} from {parse.urlsplit(url).netloc}")
        if not body:
            return {}
        try:
            value = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise PilotError("dependency returned a non-JSON response") from exc
        if not isinstance(value, dict):
            raise PilotError("dependency returned an unexpected JSON response")
        return value


class GraphClient:
    def __init__(self, token: str, http: HttpClient | None = None):
        if not token:
            raise PilotError("a Graph access token is required")
        self._token = token
        self._http = http or HttpClient()

    def call(
        self,
        method: str,
        path: str,
        *,
        body: dict[str, Any] | None = None,
        expected: tuple[int, ...] = (200,),
        beta: bool = False,
    ) -> dict[str, Any]:
        base = GRAPH_BETA if beta else GRAPH_V1
        headers = {
            "Authorization": f"Bearer {self._token}",
            "OData-Version": "4.0",
        }
        return self._http.call(
            method, f"{base}{path}", headers=headers, json_body=body, expected=expected
        )


def _run_json(command: list[str]) -> dict[str, Any]:
    try:
        completed = subprocess.run(
            command,
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise PilotError(f"command failed: {command[0]}") from exc
    try:
        value = json.loads(completed.stdout)
    except json.JSONDecodeError as exc:
        raise PilotError(f"command returned invalid JSON: {command[0]}") from exc
    if not isinstance(value, dict):
        raise PilotError(f"command returned unexpected JSON: {command[0]}")
    return value


def _run_secret(command: list[str]) -> str:
    try:
        completed = subprocess.run(
            command,
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise PilotError(f"command failed: {command[0]}") from exc
    value = completed.stdout.strip()
    if not value:
        raise PilotError(f"command returned no value: {command[0]}")
    return value


def device_code_token(tenant_id: str, client_id: str, scopes: tuple[str, ...]) -> str:
    http = HttpClient()
    base = f"https://login.microsoftonline.com/{tenant_id}/oauth2/v2.0"
    qualified_scopes = tuple(
        scope if "://" in scope else f"https://graph.microsoft.com/{scope}"
        for scope in scopes
    )
    flow = http.call(
        "POST",
        f"{base}/devicecode",
        form={"client_id": client_id, "scope": " ".join(qualified_scopes)},
        expected=(200,),
    )
    device_code = flow.get("device_code")
    user_code = flow.get("user_code")
    verification_uri = flow.get("verification_uri")
    if not all(isinstance(value, str) and value for value in (device_code, user_code, verification_uri)):
        raise PilotError("Microsoft identity platform returned an invalid device flow")
    print(f"Open {verification_uri} and enter the one-time code shown below.", file=sys.stderr)
    print(user_code, file=sys.stderr)
    interval = max(int(flow.get("interval", 5)), 1)
    expires_at = time.monotonic() + min(int(flow.get("expires_in", 900)), 1800)
    while time.monotonic() < expires_at:
        time.sleep(interval)
        try:
            result = http.call(
                "POST",
                f"{base}/token",
                form={
                    "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                    "client_id": client_id,
                    "device_code": device_code,
                },
                expected=(200,),
            )
        except PilotError as exc:
            message = str(exc)
            if "authorization_pending" in message:
                continue
            if "slow_down" in message:
                interval += 5
                continue
            raise
        token = result.get("access_token")
        if isinstance(token, str) and token:
            return token
        raise PilotError("Microsoft identity platform returned no access token")
    raise PilotError("device authorization expired")


def validate_uuid(value: str, name: str) -> str:
    if not UUID_RE.fullmatch(value):
        raise PilotError(f"{name} must be a UUID")
    return value


def validate_graph_id(value: str, name: str) -> str:
    """Validate an opaque Microsoft Graph resource ID used as a path segment."""
    if not GRAPH_ID_RE.fullmatch(value):
        raise PilotError(f"{name} is not a safe Microsoft Graph ID")
    return value


def validate_prefix(value: str) -> str:
    if not PREFIX_RE.fullmatch(value):
        raise PilotError("prefix must be 6-56 lowercase letters, digits, or hyphens")
    return value


def portal_popup_redirect_uri(portal_origin: str) -> str:
    """Return the static MSAL popup callback URI for a Portal origin."""
    return f"{portal_origin.rstrip('/')}{AGENT_ID_POPUP_CALLBACK_PATH}"


def validate_state_path(path: Path) -> Path:
    resolved = path.expanduser().resolve()
    try:
        resolved.relative_to(ROOT)
    except ValueError:
        return resolved
    raise PilotError("state file must be outside the source checkout")


def load_state(path: Path) -> dict[str, Any]:
    path = validate_state_path(path)
    if not path.is_file() or path.is_symlink():
        raise PilotError("state file is missing or unsafe")
    mode = stat.S_IMODE(path.stat().st_mode)
    if mode & 0o077:
        raise PilotError("state file permissions must be 0600")
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise PilotError("state file is unreadable") from exc
    if not isinstance(value, dict):
        raise PilotError("state file has an invalid shape")
    return value


def save_state(path: Path, state: dict[str, Any]) -> None:
    path = validate_state_path(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(state, handle, indent=2, sort_keys=True)
            handle.write("\n")
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def _graph_permission_catalog(graph: GraphClient) -> tuple[str, dict[str, str]]:
    query = parse.urlencode(
        {
            "$filter": f"appId eq '{GRAPH_APP_ID}'",
            "$select": "id,oauth2PermissionScopes",
        },
        quote_via=parse.quote,
    )
    response = graph.call(
        "GET",
        f"/servicePrincipals?{query}",
    )
    values = response.get("value", [])
    if not isinstance(values, list) or len(values) != 1:
        raise PilotError("Microsoft Graph service principal was not found")
    principal = values[0]
    scopes = principal.get("oauth2PermissionScopes", [])
    catalog = {
        item["value"]: item["id"]
        for item in scopes
        if isinstance(item, dict)
        and isinstance(item.get("value"), str)
        and isinstance(item.get("id"), str)
    }
    principal_id = principal.get("id")
    if not isinstance(principal_id, str):
        raise PilotError("Microsoft Graph service principal response was invalid")
    return principal_id, catalog


def bootstrap_client(args: argparse.Namespace) -> None:
    state_path = validate_state_path(args.state_file)
    if state_path.exists():
        raise PilotError("refusing to overwrite an existing state file")
    tenant_id = validate_uuid(args.tenant_id, "tenant ID")
    # Do not expand the shared Azure CLI application's Graph consent. Use an
    # explicitly approved public provisioning client (Microsoft Graph
    # PowerShell by default) and request only the declared delegated scopes.
    bootstrap_client_id = validate_uuid(
        args.bootstrap_client_id, "bootstrap public client ID"
    )
    graph = GraphClient(
        device_code_token(tenant_id, bootstrap_client_id, PROVISIONING_SCOPES)
    )
    me = graph.call("GET", "/me?$select=id")
    operator_object_id = validate_uuid(str(me.get("id", "")), "operator object ID")
    graph_principal_id, catalog = _graph_permission_catalog(graph)
    missing = sorted(set(PROVISIONING_SCOPES) - set(catalog))
    if missing:
        raise PilotError("tenant Graph catalog lacks required Agent ID permissions")
    display_name = f"{validate_prefix(args.prefix)}-provisioner"
    required_access = [{"id": catalog[name], "type": "Scope"} for name in PROVISIONING_SCOPES]
    app = graph.call(
        "POST",
        "/applications",
        body={
            "displayName": display_name,
            "signInAudience": "AzureADMyOrg",
            "isFallbackPublicClient": True,
            "publicClient": {"redirectUris": [PUBLIC_CLIENT_REDIRECT]},
            "requiredResourceAccess": [
                {"resourceAppId": GRAPH_APP_ID, "resourceAccess": required_access}
            ],
        },
        expected=(201,),
    )
    app_object_id = app.get("id")
    client_id = app.get("appId")
    if not all(isinstance(value, str) for value in (app_object_id, client_id)):
        raise PilotError("provisioning client creation returned an invalid response")
    state: dict[str, Any] = {
        "schema": 1,
        "tenantId": tenant_id,
        "prefix": validate_prefix(args.prefix),
        "provisioningClient": {
            "applicationObjectId": app_object_id,
            "clientId": client_id,
            "operatorObjectId": operator_object_id,
        },
        "pilot": {},
    }
    save_state(state_path, state)
    try:
        principal = graph.call(
            "POST", "/servicePrincipals", body={"appId": client_id}, expected=(201,)
        )
        principal_id = principal.get("id")
        if not isinstance(principal_id, str):
            raise PilotError("provisioning principal creation returned an invalid response")
        state["provisioningClient"]["servicePrincipalObjectId"] = principal_id
        save_state(state_path, state)
        grant = graph.call(
            "POST",
            "/oauth2PermissionGrants",
            body={
                "clientId": principal_id,
                "consentType": "Principal",
                "principalId": operator_object_id,
                "resourceId": graph_principal_id,
                "scope": " ".join(PROVISIONING_SCOPES),
            },
            expected=(201,),
        )
        state["provisioningClient"]["permissionGrantObjectId"] = validate_graph_id(
            str(grant.get("id", "")), "provisioning permission grant ID"
        )
        state["provisioningClient"]["adminConsentGranted"] = True
        save_state(state_path, state)
    except Exception:
        state["provisioningClient"]["bootstrapIncomplete"] = True
        save_state(state_path, state)
        raise
    print("Provisioning client created and consented; identifiers are in the private state file.")


def _operator_graph(state: dict[str, Any]) -> GraphClient:
    tenant = validate_uuid(str(state.get("tenantId", "")), "tenant ID")
    client_id = validate_uuid(
        str(state.get("provisioningClient", {}).get("clientId", "")),
        "provisioning client ID",
    )
    token = device_code_token(tenant, client_id, PROVISIONING_SCOPES)
    return GraphClient(token)


def _repair_incomplete_bootstrap(
    graph: GraphClient, state_path: Path, state: dict[str, Any]
) -> None:
    """Adopt a grant created before an interrupted bootstrap state save."""
    provisioning = state.get("provisioningClient", {})
    if not isinstance(provisioning, dict) or not provisioning.get("bootstrapIncomplete"):
        return
    client_id = validate_uuid(
        str(provisioning.get("servicePrincipalObjectId", "")),
        "provisioning principal object ID",
    )
    principal_id = validate_uuid(
        str(provisioning.get("operatorObjectId", "")), "operator object ID"
    )
    graph_principal_id, _ = _graph_permission_catalog(graph)
    query = parse.urlencode(
        {
            "$filter": (
                f"clientId eq '{client_id}' and resourceId eq '{graph_principal_id}' "
                "and consentType eq 'Principal'"
            ),
            "$select": "id,principalId,scope",
        },
        quote_via=parse.quote,
    )
    response = graph.call("GET", f"/oauth2PermissionGrants?{query}")
    matches = [
        item
        for item in response.get("value", [])
        if isinstance(item, dict) and item.get("principalId") == principal_id
    ]
    if len(matches) != 1:
        raise PilotError("incomplete bootstrap permission grant was not resolved exactly")
    if set(str(matches[0].get("scope", "")).split()) != set(PROVISIONING_SCOPES):
        raise PilotError("incomplete bootstrap permission grant has unexpected scopes")
    provisioning["permissionGrantObjectId"] = validate_graph_id(
        str(matches[0].get("id", "")), "provisioning permission grant ID"
    )
    provisioning["adminConsentGranted"] = True
    provisioning.pop("bootstrapIncomplete", None)
    save_state(state_path, state)


def _deployment_graph_is_complete(deployment: dict[str, Any]) -> bool:
    blueprint = deployment.get("blueprint", {})
    authorization = deployment.get("authorizationClient", {})
    oidc_issuer = deployment.get("oidcIssuer")
    return (
        isinstance(blueprint, dict)
        and isinstance(authorization, dict)
        and isinstance(oidc_issuer, str)
        and bool(oidc_issuer)
        and bool(deployment.get("ownerObjectId"))
        and bool(deployment.get("armPermissionGrantId"))
        and bool(blueprint.get("principalObjectId"))
        and bool(blueprint.get("authorizationPropertiesConfigured"))
        and bool(blueprint.get("armInheritanceCreated"))
        and bool(blueprint.get("federatedCredentialId"))
        and blueprint.get("federatedCredentialIssuer") == oidc_issuer
        and bool(blueprint.get("operatorFederatedCredentialId"))
        and blueprint.get("operatorFederatedCredentialIssuer") == oidc_issuer
        and bool(authorization.get("principalObjectId"))
        and bool(authorization.get("federatedCredentialId"))
        and authorization.get("federatedCredentialIssuer") == oidc_issuer
    )


def _create_regular_application(
    graph: GraphClient, display_name: str, scope_name: str
) -> tuple[dict[str, Any], str]:
    scope_id = str(uuid.uuid4())
    app = graph.call(
        "POST",
        "/applications",
        body={"displayName": display_name, "signInAudience": "AzureADMyOrg"},
        expected=(201,),
    )
    app_id = app.get("appId")
    object_id = app.get("id")
    if not all(isinstance(value, str) for value in (app_id, object_id)):
        raise PilotError("application creation returned an invalid response")
    graph.call(
        "PATCH",
        f"/applications/{object_id}",
        body={
            "identifierUris": [f"api://{app_id}"],
            "api": {
                "oauth2PermissionScopes": [
                    {
                        "adminConsentDescription": f"Allow access to {display_name}.",
                        "adminConsentDisplayName": f"Access {display_name}",
                        "id": scope_id,
                        "isEnabled": True,
                        "type": "User",
                        "value": scope_name,
                    }
                ]
            },
        },
        expected=(204,),
    )
    return app, scope_id


def _create_service_principal(graph: GraphClient, app_id: str) -> dict[str, Any]:
    return graph.call(
        "POST", "/servicePrincipals", body={"appId": app_id}, expected=(201,)
    )


def _service_principal_and_scope(
    graph: GraphClient, app_id: str, scope_name: str
) -> tuple[str, str]:
    query = parse.urlencode(
        {
            "$filter": f"appId eq '{app_id}'",
            "$select": "id,oauth2PermissionScopes",
        },
        quote_via=parse.quote,
    )
    response = graph.call("GET", f"/servicePrincipals?{query}")
    values = response.get("value", [])
    if not isinstance(values, list) or len(values) != 1:
        raise PilotError("downstream service principal was not resolved exactly")
    principal = values[0]
    principal_id = principal.get("id")
    scopes = principal.get("oauth2PermissionScopes", [])
    matches = [
        item.get("id")
        for item in scopes
        if isinstance(item, dict) and item.get("value") == scope_name
    ]
    if not isinstance(principal_id, str) or len(matches) != 1 or not isinstance(matches[0], str):
        raise PilotError("downstream delegated permission was not resolved exactly")
    return validate_uuid(principal_id, "downstream principal object ID"), validate_uuid(
        matches[0], "downstream delegated scope ID"
    )


def _ensure_application_fic(
    graph: GraphClient,
    application: dict[str, Any],
    *,
    name: str,
    issuer: str,
    subject: str,
) -> None:
    object_id = validate_uuid(str(application.get("objectId", "")), "application object ID")
    desired = {
        "name": name,
        "issuer": issuer,
        "subject": subject,
        "audiences": [TOKEN_EXCHANGE_AUDIENCE],
    }
    response = graph.call("GET", f"/applications/{object_id}/federatedIdentityCredentials")
    matches = [
        item
        for item in response.get("value", [])
        if isinstance(item, dict) and item.get("name") == name
    ]
    if len(matches) > 1:
        raise PilotError("application FIC name is ambiguous")
    if matches:
        credential = matches[0]
        if any(credential.get(key) != value for key, value in desired.items()):
            credential_id = validate_uuid(str(credential.get("id", "")), "FIC object ID")
            graph.call(
                "PATCH",
                f"/applications/{object_id}/federatedIdentityCredentials/{credential_id}",
                body=desired,
                expected=(204,),
            )
        application["federatedCredentialId"] = credential.get("id")
        application["federatedCredentialIssuer"] = issuer
        return
    credential = graph.call(
        "POST",
        f"/applications/{object_id}/federatedIdentityCredentials",
        body=desired,
        expected=(201,),
    )
    application["federatedCredentialId"] = credential.get("id")
    application["federatedCredentialIssuer"] = issuer


def _az_json(*arguments: str) -> dict[str, Any]:
    return _run_json(["az", *arguments, "-o", "json"])


def _managed_identity_resource_id(
    subscription_id: str, resource_group: str, identity_name: str
) -> str:
    return (
        f"/subscriptions/{subscription_id}/resourceGroups/{resource_group}/"
        f"providers/Microsoft.ManagedIdentity/userAssignedIdentities/{identity_name}"
    )


def _require_unclaimed_identity_name(
    subscription_id: str, resource_group: str, identity_name: str
) -> None:
    response = _az_json(
        "identity", "list",
        "--subscription", subscription_id,
        "--resource-group", resource_group,
        "--query", "{identities: @}",
    )
    identities = response.get("identities")
    if not isinstance(identities, list) or any(
        not isinstance(identity, dict) for identity in identities
    ):
        raise PilotError("Azure returned an invalid managed identity inventory")
    expected_id = _managed_identity_resource_id(
        subscription_id, resource_group, identity_name
    ).casefold()
    if any(
        str(identity.get("name", "")).casefold() == identity_name.casefold()
        or str(identity.get("id", "")).casefold() == expected_id
        for identity in identities
    ):
        raise PilotError(
            "encryption identity already exists without a deployment ownership record"
        )


def _run_no_output(command: list[str]) -> None:
    try:
        subprocess.run(
            command,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise PilotError(f"command failed: {command[0]}") from exc


def provision_deployment(args: argparse.Namespace) -> None:
    """Provision deployment-level Agent ID resources without a sample Agent."""
    state_path = validate_state_path(args.state_file)
    state = load_state(state_path)
    prefix = validate_prefix(args.prefix or str(state.get("prefix", "")))
    for value, name in (
        (args.agent_broker_namespace, "Agent Identity Broker namespace"),
        (args.agent_broker_service_account, "Agent Identity Broker ServiceAccount"),
        (args.operator_namespace, "operator namespace"),
        (args.operator_service_account, "operator ServiceAccount"),
    ):
        if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", value):
            raise PilotError(f"{name} is not a valid Kubernetes name")
    requested = {
        "prefix": prefix,
        "subscriptionId": validate_uuid(args.subscription_id, "subscription ID"),
        "resourceGroup": args.resource_group,
        "location": args.location,
        "oidcIssuer": args.oidc_issuer,
        "portalOrigin": args.portal_origin.rstrip("/"),
        "agentBrokerNamespace": args.agent_broker_namespace,
        "agentBrokerServiceAccount": args.agent_broker_service_account,
        "operatorNamespace": args.operator_namespace,
        "operatorServiceAccount": args.operator_service_account,
        "encryptionIdentityName": args.encryption_identity_name,
        "keyVaultName": args.key_vault_name,
    }
    deployment = state.get("deployment")
    if deployment and not args.resume:
        raise PilotError("deployment state is not empty; use --resume")
    if deployment:
        if not isinstance(deployment, dict):
            raise PilotError("deployment state has an invalid shape")
        mismatches = [
            key
            for key, value in requested.items()
            if deployment.get(key) != value
            and not (
                key == "oidcIssuer"
                and str(deployment.get(key, "")).rstrip("/")
                == str(value).rstrip("/")
            )
        ]
        if mismatches:
            raise PilotError("resume arguments do not match recorded deployment")
        encryption = deployment.get("encryptionIdentity")
        if not isinstance(encryption, dict) or not encryption.get("resourceId"):
            _require_unclaimed_identity_name(
                args.subscription_id,
                args.resource_group,
                args.encryption_identity_name,
            )
        if deployment.get("oidcIssuer") != requested["oidcIssuer"]:
            deployment["oidcIssuer"] = requested["oidcIssuer"]
            save_state(state_path, state)
    else:
        _require_unclaimed_identity_name(
            args.subscription_id,
            args.resource_group,
            args.encryption_identity_name,
        )
        deployment = requested
        state["deployment"] = deployment
        save_state(state_path, state)

    graph = None
    if _deployment_graph_is_complete(deployment):
        owner_id = validate_uuid(
            str(deployment.get("ownerObjectId", "")), "owner object ID"
        )
    else:
        graph = _operator_graph(state)
        me = graph.call("GET", "/me?$select=id")
        owner_id = validate_uuid(str(me.get("id", "")), "owner object ID")
        _repair_incomplete_bootstrap(graph, state_path, state)
        deployment["ownerObjectId"] = owner_id

    blueprint = deployment.get("blueprint")
    if not isinstance(blueprint, dict) or not blueprint:
        access_scope_id = str(uuid.uuid4())
        created = graph.call(
            "POST",
            "/applications/microsoft.graph.agentIdentityBlueprint",
            body={
                "@odata.type": "Microsoft.Graph.AgentIdentityBlueprint",
                "displayName": f"{prefix}-blueprint",
                "sponsors@odata.bind": [f"{GRAPH_V1}/users/{owner_id}"],
                "owners@odata.bind": [f"{GRAPH_V1}/users/{owner_id}"],
            },
            expected=(201,),
        )
        blueprint = {
            "objectId": validate_uuid(str(created.get("id", "")), "Blueprint object ID"),
            "clientId": validate_uuid(str(created.get("appId", "")), "Blueprint client ID"),
            "scopeId": access_scope_id,
        }
        deployment["blueprint"] = blueprint
        save_state(state_path, state)
    blueprint_client_id = validate_uuid(str(blueprint.get("clientId", "")), "Blueprint client ID")
    access_scope_id = validate_uuid(str(blueprint.get("scopeId", "")), "Blueprint scope ID")

    if not blueprint.get("principalObjectId"):
        principal = graph.call(
            "POST",
            "/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal",
            body={"appId": blueprint_client_id},
            expected=(201,),
        )
        blueprint["principalObjectId"] = validate_uuid(
            str(principal.get("id", "")), "Blueprint principal object ID"
        )
        save_state(state_path, state)

    authorization = deployment.get("authorizationClient")
    if not isinstance(authorization, dict) or not authorization:
        redirect_uri = f"{args.portal_origin.rstrip('/')}/api/v1/connections/callback"
        created = graph.call(
            "POST",
            "/applications",
            body={
                "displayName": f"{prefix}-authorization",
                "signInAudience": "AzureADMyOrg",
                "web": {"redirectUris": [redirect_uri]},
                "requiredResourceAccess": [
                    {
                        "resourceAppId": blueprint_client_id,
                        "resourceAccess": [{"id": access_scope_id, "type": "Scope"}],
                    }
                ],
            },
            expected=(201,),
        )
        authorization = {
            "objectId": validate_uuid(str(created.get("id", "")), "authorization client object ID"),
            "clientId": validate_uuid(str(created.get("appId", "")), "authorization client ID"),
            "redirectUri": redirect_uri,
        }
        deployment["authorizationClient"] = authorization
        save_state(state_path, state)
    if not authorization.get("principalObjectId"):
        principal = _create_service_principal(graph, str(authorization["clientId"]))
        authorization["principalObjectId"] = validate_uuid(
            str(principal.get("id", "")), "authorization principal object ID"
        )
        save_state(state_path, state)

    scope_definition = {
        "adminConsentDescription": "Allow this CetiRun deployment to act for the signed-in user.",
        "adminConsentDisplayName": "Access CetiRun Agent Identity",
        "id": access_scope_id,
        "isEnabled": True,
        "type": "User",
        "userConsentDescription": "Allow this CetiRun deployment to act for you.",
        "userConsentDisplayName": "Access CetiRun Agent Identity",
        "value": "access_agent",
    }
    if not blueprint.get("authorizationPropertiesConfigured"):
        graph.call(
            "PATCH",
            f"/applications/{blueprint_client_id}",
            body={
                "identifierUris": [f"api://{blueprint_client_id}"],
                "api": {
                    "oauth2PermissionScopes": [scope_definition],
                },
            },
            expected=(204,),
        )
        blueprint["authorizationPropertiesConfigured"] = True
        save_state(state_path, state)

    if not blueprint.get("armInheritanceCreated"):
        _ensure_inheritable_permission(
            graph, str(blueprint["objectId"]), ARM_APP_ID
        )
        blueprint["armInheritanceCreated"] = True
        save_state(state_path, state)
    if not deployment.get("armPermissionGrantId"):
        arm_principal_id, _ = _service_principal_and_scope(
            graph, ARM_APP_ID, "user_impersonation"
        )
        deployment["armPermissionGrantId"] = _ensure_permission_grant(
            graph,
            client_id=str(blueprint["principalObjectId"]),
            resource_id=arm_principal_id,
            scope="user_impersonation",
            consent_type="AllPrincipals",
            principal_id=None,
        )
        save_state(state_path, state)

    broker_subject = f"system:serviceaccount:{args.agent_broker_namespace}:{args.agent_broker_service_account}"
    operator_subject = f"system:serviceaccount:{args.operator_namespace}:{args.operator_service_account}"
    if blueprint.get("federatedCredentialIssuer") != args.oidc_issuer:
        _ensure_blueprint_fic(
            graph, blueprint, name=f"{prefix}-broker",
            issuer=args.oidc_issuer, subject=broker_subject
        )
        save_state(state_path, state)
    # _ensure_blueprint_fic stores one legacy marker, so use the generic exact helper
    # shape for the second Blueprint credential through the Blueprint application path.
    if blueprint.get("operatorFederatedCredentialIssuer") != args.oidc_issuer:
        second = {
            "objectId": blueprint["clientId"],
            "clientId": blueprint["clientId"],
        }
        _ensure_application_fic(
            graph, second, name=f"{prefix}-operator",
            issuer=args.oidc_issuer, subject=operator_subject
        )
        blueprint["operatorFederatedCredentialId"] = second.get("federatedCredentialId")
        blueprint["operatorFederatedCredentialIssuer"] = second.get(
            "federatedCredentialIssuer"
        )
        save_state(state_path, state)
    if authorization.get("federatedCredentialIssuer") != args.oidc_issuer:
        _ensure_application_fic(
            graph,
            authorization,
            name=f"{prefix}-authorization",
            issuer=args.oidc_issuer,
            subject=broker_subject,
        )
        save_state(state_path, state)

    encryption = deployment.get("encryptionIdentity")
    if not isinstance(encryption, dict) or not encryption.get("resourceId"):
        created = _az_json(
            "identity", "create", "--subscription", args.subscription_id,
            "--resource-group", args.resource_group, "--name", args.encryption_identity_name,
            "--location", args.location,
        )
        resource_id = str(created.get("id", ""))
        expected_resource_id = _managed_identity_resource_id(
            args.subscription_id, args.resource_group, args.encryption_identity_name
        )
        if resource_id.casefold() != expected_resource_id.casefold():
            raise PilotError("encryption identity returned an unexpected resource ID")
        encryption = {
            "resourceId": resource_id,
            "clientId": validate_uuid(str(created.get("clientId", "")), "encryption identity client ID"),
            "principalId": validate_uuid(str(created.get("principalId", "")), "encryption identity principal ID"),
        }
        deployment["encryptionIdentity"] = encryption
        save_state(state_path, state)
    if encryption.get("federatedCredentialIssuer") != args.oidc_issuer:
        operation = (
            "update" if encryption.get("federatedCredentialCreated") else "create"
        )
        _az_json(
            "identity", "federated-credential", operation,
            "--subscription", args.subscription_id,
            "--resource-group", args.resource_group,
            "--identity-name", args.encryption_identity_name,
            "--name", f"{prefix}-broker",
            "--issuer", args.oidc_issuer,
            "--subject", broker_subject,
            "--audiences", TOKEN_EXCHANGE_AUDIENCE,
        )
        encryption["federatedCredentialCreated"] = True
        encryption["federatedCredentialIssuer"] = args.oidc_issuer
        save_state(state_path, state)

    key_vault = deployment.get("keyVault")
    if not isinstance(key_vault, dict) or not key_vault:
        created = _az_json(
            "keyvault", "create", "--subscription", args.subscription_id,
            "--resource-group", args.resource_group, "--name", args.key_vault_name,
            "--location", args.location, "--enable-rbac-authorization", "true",
        )
        key_vault = {"resourceId": str(created.get("id", "")), "name": args.key_vault_name}
        deployment["keyVault"] = key_vault
        save_state(state_path, state)
    if not key_vault.get("keyUrl") and not key_vault.get("bootstrapRoleAssignmentId"):
        assignment = _az_json(
            "role", "assignment", "create", "--subscription", args.subscription_id,
            "--assignee-object-id", owner_id,
            "--assignee-principal-type", "User",
            "--role", "Key Vault Crypto Officer",
            "--scope", str(key_vault["resourceId"]),
        )
        assignment_id = str(assignment.get("id", ""))
        if not assignment_id.startswith("/subscriptions/"):
            raise PilotError("Key Vault bootstrap role assignment returned an invalid ID")
        key_vault["bootstrapRoleAssignmentId"] = assignment_id
        save_state(state_path, state)
    if not key_vault.get("keyUrl"):
        key = None
        for attempt in range(12):
            try:
                key = _az_json(
                    "keyvault", "key", "create", "--subscription", args.subscription_id,
                    "--vault-name", args.key_vault_name,
                    "--name", "authorization-cache", "--kty", "RSA", "--size", "3072",
                    "--ops", "wrapKey", "unwrapKey",
                )
                break
            except PilotError:
                if attempt == 11:
                    raise PilotError(
                        "Key Vault key creation failed after RBAC propagation wait"
                    ) from None
                time.sleep(5)
        if not isinstance(key, dict):
            raise PilotError("Key Vault key creation returned an invalid response")
        key_vault["keyUrl"] = str(key.get("key", {}).get("kid", ""))
        if not key_vault["keyUrl"].startswith("https://"):
            raise PilotError("Key Vault key creation returned an invalid key URL")
        save_state(state_path, state)
    if key_vault.get("bootstrapRoleAssignmentId"):
        _run_no_output(
            [
                "az", "role", "assignment", "delete", "--subscription", args.subscription_id, "--ids",
                str(key_vault["bootstrapRoleAssignmentId"]),
            ]
        )
        key_vault.pop("bootstrapRoleAssignmentId", None)
        save_state(state_path, state)
    if not key_vault.get("roleAssignmentId"):
        assignment = _az_json(
            "role", "assignment", "create", "--subscription", args.subscription_id,
            "--assignee-object-id", str(encryption["principalId"]),
            "--assignee-principal-type", "ServicePrincipal",
            "--role", "Key Vault Crypto Service Encryption User",
            "--scope", f"{str(key_vault['resourceId']).rstrip('/')}/keys/authorization-cache",
        )
        key_vault["roleAssignmentId"] = str(assignment.get("id", ""))
        save_state(state_path, state)
    deployment["provisioningComplete"] = True
    save_state(state_path, state)
    print("Deployment Agent ID resources were provisioned; identifiers remain in the private state file.")


def cleanup_deployment(args: argparse.Namespace) -> None:
    state_path = validate_state_path(args.state_file)
    state = load_state(state_path)
    deployment = state.get("deployment", {})
    provisioning = state.get("provisioningClient", {})
    if not isinstance(deployment, dict) or not isinstance(provisioning, dict):
        raise PilotError("cleanup state has an invalid shape")
    if not deployment and not provisioning:
        print("No deployment Agent ID resources are recorded.")
        return
    graph = _operator_graph(state)

    grant_id = deployment.get("armPermissionGrantId")
    if grant_id:
        graph.call("DELETE", f"/oauth2PermissionGrants/{validate_graph_id(str(grant_id), 'grant ID')}", expected=(204, 404))
        deployment.pop("armPermissionGrantId", None)
        save_state(state_path, state)
    authorization = deployment.get("authorizationClient", {})
    if isinstance(authorization, dict) and authorization.get("objectId"):
        graph.call(
            "DELETE",
            f"/applications/{validate_uuid(str(authorization['objectId']), 'authorization application object ID')}",
            expected=(204, 404),
        )
        deployment.pop("authorizationClient", None)
        save_state(state_path, state)
    blueprint = deployment.get("blueprint", {})
    if isinstance(blueprint, dict) and blueprint.get("objectId"):
        graph.call(
            "DELETE",
            f"/applications/{validate_uuid(str(blueprint['objectId']), 'Blueprint object ID')}",
            expected=(204, 404),
        )
        deployment.pop("blueprint", None)
        save_state(state_path, state)
    key_vault = deployment.get("keyVault", {})
    if isinstance(key_vault, dict) and key_vault.get("name"):
        if key_vault.get("bootstrapRoleAssignmentId"):
            _run_no_output(
                [
                    "az", "role", "assignment", "delete",
                    "--subscription", str(deployment["subscriptionId"]), "--ids",
                    str(key_vault["bootstrapRoleAssignmentId"]),
                ]
            )
            key_vault.pop("bootstrapRoleAssignmentId", None)
            save_state(state_path, state)
        _run_no_output(
            [
                "az", "keyvault", "delete", "--subscription", str(deployment["subscriptionId"]),
                "--resource-group", str(deployment["resourceGroup"]), "--name", str(key_vault["name"]),
            ]
        )
        deployment.pop("keyVault", None)
        save_state(state_path, state)
    encryption = deployment.get("encryptionIdentity", {})
    if isinstance(encryption, dict) and encryption.get("resourceId"):
        _run_no_output(
            [
                "az", "identity", "delete", "--subscription", str(deployment["subscriptionId"]),
                "--ids", str(encryption["resourceId"]),
            ]
        )
        deployment.pop("encryptionIdentity", None)
        save_state(state_path, state)
    permission_grant_id = provisioning.get("permissionGrantObjectId")
    if permission_grant_id:
        graph.call(
            "DELETE",
            f"/oauth2PermissionGrants/{validate_graph_id(str(permission_grant_id), 'provisioning permission grant ID')}",
            expected=(204, 404),
        )
        provisioning.pop("permissionGrantObjectId", None)
        save_state(state_path, state)
    principal_id = provisioning.get("servicePrincipalObjectId")
    if principal_id:
        graph.call(
            "DELETE",
            f"/servicePrincipals/{validate_uuid(str(principal_id), 'provisioning principal object ID')}",
            expected=(204, 404),
        )
        provisioning.pop("servicePrincipalObjectId", None)
        save_state(state_path, state)
    application_id = provisioning.get("applicationObjectId")
    if application_id:
        graph.call(
            "DELETE",
            f"/applications/{validate_uuid(str(application_id), 'provisioning application object ID')}",
            expected=(204, 404),
        )
    state["deployment"] = {}
    state["provisioningClient"] = {}
    save_state(state_path, state)
    print("Recorded deployment Agent ID and provisioning resources were deleted; Key Vault soft-delete retention still applies.")


def _kubectl_create_service_account(namespace: str, service_account: str) -> None:
    try:
        subprocess.run(
            ["kubectl", "get", "namespace", namespace],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise PilotError("the target instance namespace does not exist") from exc
    exists = subprocess.run(
        ["kubectl", "get", "serviceaccount", service_account, "--namespace", namespace],
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    if exists.returncode == 0:
        raise PilotError("the dedicated pilot ServiceAccount already exists")
    service_account_manifest = {
        "apiVersion": "v1",
        "kind": "ServiceAccount",
        "metadata": {
            "name": service_account,
            "namespace": namespace,
            "labels": {"azure.workload.identity/use": "true"},
        },
    }
    try:
        subprocess.run(
            ["kubectl", "create", "-f", "-"],
            input=json.dumps(service_account_manifest),
            text=True,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise PilotError("failed to create the dedicated pilot ServiceAccount") from exc


def _kubectl_annotate_service_account(
    namespace: str, service_account: str, blueprint_client_id: str
) -> None:
    try:
        subprocess.run(
            [
                "kubectl",
                "annotate",
                "serviceaccount",
                service_account,
                "--namespace",
                namespace,
                f"azure.workload.identity/client-id={blueprint_client_id}",
                "--overwrite",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise PilotError("failed to annotate the dedicated pilot ServiceAccount") from exc


def _ensure_blueprint_fic(
    graph: GraphClient,
    blueprint_state: dict[str, Any],
    *,
    name: str,
    issuer: str,
    subject: str,
) -> None:
    validate_uuid(str(blueprint_state.get("objectId", "")), "Blueprint object ID")
    client_id = validate_uuid(
        str(blueprint_state.get("clientId", "")), "Blueprint client ID"
    )
    desired = {
        "name": name,
        "issuer": issuer,
        "subject": subject,
        "audiences": [TOKEN_EXCHANGE_AUDIENCE],
    }
    # Blueprint credential endpoints are addressed by Blueprint appId. Always
    # query before creating so a successful POST followed by a local state-file
    # failure can be resumed without creating a duplicate credential.
    response = graph.call(
        "GET", f"/applications/{client_id}/federatedIdentityCredentials"
    )
    matches = [
        item
        for item in response.get("value", [])
        if isinstance(item, dict) and item.get("name") == name
    ]
    if len(matches) > 1:
        raise PilotError("Blueprint FIC name is ambiguous")
    if matches:
        credential = matches[0]
        credential_id = validate_uuid(
            str(credential.get("id", "")), "Blueprint FIC object ID"
        )
        if any(credential.get(key) != value for key, value in desired.items()):
            graph.call(
                "PATCH",
                f"/applications/{client_id}/federatedIdentityCredentials/{credential_id}",
                body=desired,
                expected=(204,),
            )
        blueprint_state["federatedCredentialId"] = credential_id
        blueprint_state["federatedCredentialIssuerExact"] = True
        blueprint_state["federatedCredentialIssuer"] = issuer
        return
    credential = graph.call(
        "POST",
        f"/applications/{client_id}/federatedIdentityCredentials",
        body=desired,
        expected=(201,),
    )
    credential_id = credential.get("id")
    if isinstance(credential_id, str):
        blueprint_state["federatedCredentialId"] = validate_uuid(
            credential_id, "Blueprint FIC object ID"
        )
    blueprint_state["federatedCredentialCreated"] = True
    blueprint_state["federatedCredentialIssuerExact"] = True
    blueprint_state["federatedCredentialIssuer"] = issuer


def _ensure_inheritable_permission(
    graph: GraphClient, blueprint_object_id: str, resource_app_id: str
) -> None:
    blueprint_object_id = validate_uuid(blueprint_object_id, "Blueprint object ID")
    resource_app_id = validate_uuid(resource_app_id, "resource application ID")
    path = (
        f"/applications/{blueprint_object_id}/"
        "microsoft.graph.agentIdentityBlueprint/inheritablePermissions"
    )
    response = graph.call("GET", path)
    matches = [
        item
        for item in response.get("value", [])
        if isinstance(item, dict)
        and str(item.get("resourceAppId", "")).lower() == resource_app_id.lower()
    ]
    if len(matches) > 1:
        raise PilotError("inheritable permission is ambiguous")
    if matches:
        scopes = matches[0].get("inheritableScopes", {})
        if not isinstance(scopes, dict) or (
            str(scopes.get("@odata.type", "")).lstrip("#")
            != "microsoft.graph.allAllowedScopes"
            and scopes.get("kind") != "allAllowed"
        ):
            raise PilotError("existing inheritable permission is more restrictive than required")
        return
    graph.call(
        "POST",
        path,
        body={
            "resourceAppId": resource_app_id,
            "inheritableScopes": {
                "@odata.type": "microsoft.graph.allAllowedScopes"
            },
        },
        expected=(201,),
    )


def _ensure_permission_grant(
    graph: GraphClient,
    *,
    client_id: str,
    resource_id: str,
    scope: str,
    consent_type: str,
    principal_id: str | None,
) -> str:
    client_id = validate_uuid(client_id, "grant client principal ID")
    resource_id = validate_uuid(resource_id, "grant resource principal ID")
    if principal_id is not None:
        principal_id = validate_uuid(principal_id, "grant principal ID")
    query = parse.urlencode(
        {
            "$filter": (
                f"clientId eq '{client_id}' and resourceId eq '{resource_id}' "
                f"and consentType eq '{consent_type}'"
            ),
            "$select": "id,clientId,consentType,principalId,resourceId,scope",
        },
        quote_via=parse.quote,
    )
    response = graph.call("GET", f"/oauth2PermissionGrants?{query}")
    matches = [
        item
        for item in response.get("value", [])
        if isinstance(item, dict) and item.get("principalId") == principal_id
    ]
    if len(matches) > 1:
        raise PilotError("delegated permission grant is ambiguous")
    if matches:
        granted_scopes = set(str(matches[0].get("scope", "")).split())
        if granted_scopes != {scope}:
            raise PilotError("existing delegated permission grant has unexpected scopes")
        return validate_graph_id(str(matches[0].get("id", "")), "grant ID")
    grant = graph.call(
        "POST",
        "/oauth2PermissionGrants",
        body={
            "clientId": client_id,
            "consentType": consent_type,
            "principalId": principal_id,
            "resourceId": resource_id,
            "scope": scope,
        },
        expected=(201,),
    )
    return validate_graph_id(str(grant.get("id", "")), "grant ID")


def _blueprint_graph_token(
    tenant_id: str, blueprint_client_id: str, namespace: str, service_account: str
) -> str:
    assertion = _run_secret(
        [
            "kubectl",
            "create",
            "token",
            service_account,
            "--namespace",
            namespace,
            "--audience",
            TOKEN_EXCHANGE_AUDIENCE,
            "--duration",
            "10m",
        ]
    )
    response = HttpClient().call(
        "POST",
        f"https://login.microsoftonline.com/{tenant_id}/oauth2/v2.0/token",
        form={
            "client_id": blueprint_client_id,
            "scope": "https://graph.microsoft.com/.default",
            "client_assertion_type": TOKEN_ASSERTION_TYPE,
            "client_assertion": assertion,
            "grant_type": "client_credentials",
        },
        expected=(200,),
    )
    token = response.get("access_token")
    if not isinstance(token, str) or not token:
        raise PilotError("blueprint token exchange returned no access token")
    return token


def provision(args: argparse.Namespace) -> None:
    state_path = validate_state_path(args.state_file)
    state = load_state(state_path)
    prefix = validate_prefix(args.prefix or str(state.get("prefix", "")))
    namespace = args.namespace
    service_account = args.service_account
    if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", namespace):
        raise PilotError("namespace is not a valid Kubernetes name")
    if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", service_account):
        raise PilotError("service account is not a valid Kubernetes name")
    requested_pilot = {
        "prefix": prefix,
        "namespace": namespace,
        "serviceAccount": service_account,
        "oidcIssuer": args.oidc_issuer,
        "portalOrigin": args.portal_origin.rstrip("/"),
        "echoHost": args.echo_host,
    }
    recorded_pilot = state.get("pilot")
    if recorded_pilot and not args.resume:
        raise PilotError("pilot state is not empty; use --resume or clean it up")
    if recorded_pilot:
        if not isinstance(recorded_pilot, dict):
            raise PilotError("pilot state has an invalid shape")
        mismatches = [
            key
            for key, value in requested_pilot.items()
            if recorded_pilot.get(key) != value
            and not (
                key == "oidcIssuer"
                and str(recorded_pilot.get(key, "")).rstrip("/")
                == str(value).rstrip("/")
            )
        ]
        if mismatches:
            raise PilotError("resume arguments do not match the recorded pilot")
        pilot = recorded_pilot
        if pilot.get("oidcIssuer") != requested_pilot["oidcIssuer"]:
            pilot["oidcIssuer"] = requested_pilot["oidcIssuer"]
            save_state(state_path, state)
    else:
        pilot = requested_pilot
        state["pilot"] = pilot
        save_state(state_path, state)

    graph = _operator_graph(state)
    me = graph.call("GET", "/me?$select=id")
    user_id = me.get("id")
    if not isinstance(user_id, str):
        raise PilotError("Graph /me response was invalid")
    if pilot.get("provisioningComplete") is True:
        spa_application = pilot.get("spaApplication")
        if not isinstance(spa_application, dict) or not spa_application:
            raise PilotError("completed pilot state has no SPA application")
        if not spa_application.get("popupRedirectUriConfigured"):
            spa_object_id = validate_uuid(
                str(spa_application.get("objectId", "")), "SPA object ID"
            )
            graph.call(
                "PATCH",
                f"/applications/{spa_object_id}",
                body={
                    "spa": {
                        "redirectUris": [
                            portal_popup_redirect_uri(args.portal_origin)
                        ]
                    }
                },
                expected=(204,),
            )
            spa_application["popupRedirectUriConfigured"] = True
            save_state(state_path, state)
            print("Pilot SPA popup redirect URI updated.")
        else:
            print("Pilot identity provisioning is already complete.")
        return

    if not pilot.get("serviceAccountCreated"):
        _kubectl_create_service_account(namespace, service_account)
        pilot["serviceAccountCreated"] = True
        save_state(state_path, state)

    echo_application = pilot.get("echoApplication")
    if not isinstance(echo_application, dict) or not echo_application:
        echo_app, echo_scope_id = _create_regular_application(
            graph, f"{prefix}-echo", "Echo.Read"
        )
        echo_application = {
            "objectId": echo_app["id"],
            "clientId": echo_app["appId"],
            "scopeId": echo_scope_id,
        }
        pilot["echoApplication"] = echo_application
        save_state(state_path, state)
    echo_scope_id = validate_uuid(str(echo_application.get("scopeId", "")), "Echo scope ID")
    echo_client_id = validate_uuid(
        str(echo_application.get("clientId", "")), "Echo client ID"
    )
    if not echo_application.get("servicePrincipalObjectId"):
        echo_principal = _create_service_principal(graph, echo_client_id)
        echo_application["servicePrincipalObjectId"] = echo_principal["id"]
        save_state(state_path, state)

    blueprint_state = pilot.get("blueprint")
    if not isinstance(blueprint_state, dict) or not blueprint_state:
        access_scope_id = str(uuid.uuid4())
        blueprint = graph.call(
            "POST",
            "/applications/microsoft.graph.agentIdentityBlueprint",
            body={
                "@odata.type": "Microsoft.Graph.AgentIdentityBlueprint",
                "displayName": f"{prefix}-blueprint",
                "sponsors@odata.bind": [f"{GRAPH_V1}/users/{user_id}"],
                "owners@odata.bind": [f"{GRAPH_V1}/users/{user_id}"],
            },
            expected=(201,),
        )
        blueprint_id = blueprint.get("id")
        blueprint_client_id = blueprint.get("appId")
        if not all(isinstance(value, str) for value in (blueprint_id, blueprint_client_id)):
            raise PilotError("blueprint creation returned an invalid response")
        blueprint_state = {
            "objectId": blueprint_id,
            "clientId": blueprint_client_id,
            "scopeId": access_scope_id,
        }
        pilot["blueprint"] = blueprint_state
        save_state(state_path, state)
    blueprint_client_id = validate_uuid(
        str(blueprint_state.get("clientId", "")), "Blueprint client ID"
    )
    validate_uuid(str(blueprint_state.get("objectId", "")), "Blueprint object ID")
    access_scope_id = validate_uuid(
        str(blueprint_state.get("scopeId", "")), "Blueprint scope ID"
    )

    # The creation guide addresses the authorization-property operation by
    # appId. This pilot grants the two downstream scopes explicitly on the
    # Blueprint principal instead of expanding the provisioner to the broader
    # Blueprint permission needed to modify requiredResourceAccess.
    if not blueprint_state.get("authorizationPropertiesConfigured"):
        graph.call(
            "PATCH",
            f"/applications/{blueprint_client_id}",
            body={
                "identifierUris": [f"api://{blueprint_client_id}"],
                "api": {
                    "oauth2PermissionScopes": [
                        {
                            "adminConsentDescription": "Allow access to the pilot agent.",
                            "adminConsentDisplayName": "Access pilot agent",
                            "id": access_scope_id,
                            "isEnabled": True,
                            "type": "User",
                            "value": "access_agent",
                        }
                    ]
                },
            },
            expected=(204,),
        )
        blueprint_state["authorizationPropertiesConfigured"] = True
        save_state(state_path, state)
    blueprint_state["permissionDeclarationMode"] = "explicitDirectGrant"
    save_state(state_path, state)

    blueprint_principal_id = blueprint_state.get("servicePrincipalObjectId")
    if not blueprint_principal_id:
        blueprint_principal = graph.call(
            "POST",
            "/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal",
            body={"appId": blueprint_client_id},
            expected=(201,),
        )
        blueprint_principal_id = blueprint_principal.get("id")
        if not isinstance(blueprint_principal_id, str):
            raise PilotError("blueprint principal creation returned an invalid response")
        blueprint_state["servicePrincipalObjectId"] = blueprint_principal_id
        save_state(state_path, state)
    blueprint_principal_id = validate_uuid(
        str(blueprint_principal_id), "Blueprint principal object ID"
    )
    _ensure_blueprint_fic(
        graph,
        blueprint_state,
        name=f"{prefix}-aks",
        issuer=args.oidc_issuer,
        subject=f"system:serviceaccount:{namespace}:{service_account}",
    )
    save_state(state_path, state)
    if not pilot.get("serviceAccountAnnotated"):
        _kubectl_annotate_service_account(namespace, service_account, blueprint_client_id)
        pilot["serviceAccountAnnotated"] = True
        save_state(state_path, state)

    inherited_resources = blueprint_state.setdefault("inheritableResourceAppIds", [])
    for resource_app_id in (GRAPH_APP_ID, echo_client_id):
        if resource_app_id in inherited_resources:
            continue
        _ensure_inheritable_permission(
            graph, str(blueprint_state["objectId"]), resource_app_id
        )
        inherited_resources.append(resource_app_id)
        save_state(state_path, state)
    blueprint_state["inheritablePermissionsCreated"] = True
    save_state(state_path, state)

    graph_principal_id, _ = _graph_permission_catalog(graph)
    permission_grants = pilot.setdefault("permissionGrants", {})
    for grant_name, resource_id, scope in (
        ("graphUserRead", graph_principal_id, "User.Read"),
        (
            "echoRead",
            echo_application["servicePrincipalObjectId"],
            "Echo.Read",
        ),
    ):
        if grant_name in permission_grants:
            continue
        permission_grants[grant_name] = _ensure_permission_grant(
            graph,
            client_id=blueprint_principal_id,
            resource_id=str(resource_id),
            scope=scope,
            consent_type="AllPrincipals",
            principal_id=None,
        )
        save_state(state_path, state)

    spa_application = pilot.get("spaApplication")
    if not isinstance(spa_application, dict) or not spa_application:
        spa = graph.call(
            "POST",
            "/applications",
            body={
                "displayName": f"{prefix}-spa",
                "signInAudience": "AzureADMyOrg",
                "spa": {
                    "redirectUris": [portal_popup_redirect_uri(args.portal_origin)]
                },
                "requiredResourceAccess": [
                    {
                        "resourceAppId": blueprint_client_id,
                        "resourceAccess": [{"id": access_scope_id, "type": "Scope"}],
                    }
                ],
            },
            expected=(201,),
        )
        spa_application = {
            "objectId": spa["id"],
            "clientId": spa["appId"],
            "popupRedirectUriConfigured": True,
        }
        pilot["spaApplication"] = spa_application
        save_state(state_path, state)
    if not spa_application.get("servicePrincipalObjectId"):
        spa_principal = _create_service_principal(graph, spa_application["clientId"])
        spa_application["servicePrincipalObjectId"] = spa_principal["id"]
        save_state(state_path, state)
    if not blueprint_state.get("spaAuthorizationDeclared"):
        # requiredResourceAccess on the SPA declares this scope. The user
        # grants it during Connect; Agent ID Blueprint auth-property updates do
        # not support preAuthorizedApplications.
        blueprint_state["spaAuthorizationDeclared"] = True
        save_state(state_path, state)

    if not pilot.get("agentIdentity"):
        blueprint_token = _blueprint_graph_token(
            state["tenantId"], blueprint_client_id, namespace, service_account
        )
        agent_graph = GraphClient(blueprint_token)
        agent = agent_graph.call(
            "POST",
            "/serviceprincipals/Microsoft.Graph.AgentIdentity",
            body={
                "@odata.type": "#Microsoft.Graph.AgentIdentity",
                "displayName": f"{prefix}-agent",
                "agentIdentityBlueprintId": blueprint_client_id,
                "sponsors@odata.bind": [f"{GRAPH_V1}/users/{user_id}"],
            },
            expected=(201,),
            beta=True,
        )
        agent_object_id = agent.get("id")
        agent_client_id = agent.get("appId")
        if not all(isinstance(value, str) for value in (agent_object_id, agent_client_id)):
            raise PilotError("agent identity creation returned an invalid response")
        pilot["agentIdentity"] = {
            "objectId": agent_object_id,
            "clientId": agent_client_id,
        }
        save_state(state_path, state)
    pilot["provisioningComplete"] = True
    save_state(state_path, state)
    print("Pilot identity objects created; identifiers are in the private state file.")


def cleanup(args: argparse.Namespace) -> None:
    state_path = validate_state_path(args.state_file)
    state = load_state(state_path)
    pilot = state.get("pilot")
    if not isinstance(pilot, dict) or not pilot:
        print("No pilot resources are recorded.")
        return
    graph = _operator_graph(state)

    def delete(path: str, *, beta: bool = False) -> None:
        graph.call("DELETE", path, expected=(204, 404), beta=beta)

    agent = pilot.get("agentIdentity", {})
    if agent.get("objectId"):
        delete(f"/servicePrincipals/{agent['objectId']}", beta=True)
        agent.clear()
        save_state(state_path, state)
    grant_ids = list(pilot.get("permissionGrantObjectIds", []))
    permission_grants = pilot.get("permissionGrants", {})
    if isinstance(permission_grants, dict):
        grant_ids.extend(permission_grants.values())
    for grant_id in list(reversed(grant_ids)):
        delete(f"/oauth2PermissionGrants/{validate_graph_id(str(grant_id), 'grant ID')}")
        if grant_id in pilot.get("permissionGrantObjectIds", []):
            pilot["permissionGrantObjectIds"].remove(grant_id)
        if isinstance(permission_grants, dict):
            for key, value in list(permission_grants.items()):
                if value == grant_id:
                    del permission_grants[key]
        save_state(state_path, state)
    for key in ("spaApplication", "echoApplication"):
        application = pilot.get(key, {})
        if application.get("objectId"):
            delete(f"/applications/{application['objectId']}")
            application.clear()
            save_state(state_path, state)
    blueprint = pilot.get("blueprint", {})
    if blueprint.get("objectId"):
        delete(f"/applications/{blueprint['objectId']}")
        blueprint.clear()
        save_state(state_path, state)
    if pilot.get("serviceAccountCreated"):
        try:
            subprocess.run(
                [
                    "kubectl",
                    "delete",
                    "serviceaccount",
                    pilot["serviceAccount"],
                    "--namespace",
                    pilot["namespace"],
                    "--ignore-not-found",
                ],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        except (OSError, subprocess.CalledProcessError) as exc:
            raise PilotError("failed to delete the dedicated pilot ServiceAccount") from exc
    state["pilot"] = {}
    save_state(state_path, state)
    print("Pilot identity resources and dedicated ServiceAccount were deleted.")


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    subparsers = result.add_subparsers(dest="command", required=True)
    bootstrap = subparsers.add_parser("bootstrap-client")
    bootstrap.add_argument("--tenant-id", required=True)
    bootstrap.add_argument("--prefix", required=True)
    bootstrap.add_argument("--state-file", required=True, type=Path)
    bootstrap.add_argument(
        "--bootstrap-client-id",
        default=GRAPH_POWERSHELL_CLIENT_ID,
        help="approved public Graph client used only for interactive bootstrap",
    )
    bootstrap.set_defaults(function=bootstrap_client)

    provision_parser = subparsers.add_parser("provision")
    provision_parser.add_argument("--state-file", required=True, type=Path)
    provision_parser.add_argument("--prefix")
    provision_parser.add_argument("--namespace", required=True)
    provision_parser.add_argument("--service-account", required=True)
    provision_parser.add_argument("--oidc-issuer", required=True)
    provision_parser.add_argument("--portal-origin", required=True)
    provision_parser.add_argument("--echo-host", required=True)
    provision_parser.add_argument(
        "--resume",
        action="store_true",
        help="continue the exact pilot recorded in the private state file",
    )
    provision_parser.add_argument(
        "--graph-user-read-scope-id",
        default="e1fe6dd8-ba31-4d61-89e7-88639da4683d",
    )
    provision_parser.set_defaults(function=provision)

    deployment_parser = subparsers.add_parser("provision-deployment")
    deployment_parser.add_argument("--state-file", required=True, type=Path)
    deployment_parser.add_argument("--prefix")
    deployment_parser.add_argument("--resume", action="store_true")
    deployment_parser.add_argument("--subscription-id", required=True)
    deployment_parser.add_argument("--resource-group", required=True)
    deployment_parser.add_argument("--location", required=True)
    deployment_parser.add_argument("--oidc-issuer", required=True)
    deployment_parser.add_argument("--portal-origin", required=True)
    deployment_parser.add_argument("--agent-broker-namespace", required=True)
    deployment_parser.add_argument("--agent-broker-service-account", required=True)
    deployment_parser.add_argument("--operator-namespace", required=True)
    deployment_parser.add_argument("--operator-service-account", required=True)
    deployment_parser.add_argument("--encryption-identity-name", required=True)
    deployment_parser.add_argument("--key-vault-name", required=True)
    deployment_parser.set_defaults(function=provision_deployment)

    cleanup_parser = subparsers.add_parser("cleanup")
    cleanup_parser.add_argument("--state-file", required=True, type=Path)
    cleanup_parser.set_defaults(function=cleanup)
    cleanup_deployment_parser = subparsers.add_parser("cleanup-deployment")
    cleanup_deployment_parser.add_argument("--state-file", required=True, type=Path)
    cleanup_deployment_parser.set_defaults(function=cleanup_deployment)
    return result


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        args.function(args)
    except PilotError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
