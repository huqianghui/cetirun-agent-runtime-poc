#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""DNS and cert-manager phases used by the CetiRun platform deployment."""

import base64
import copy
from datetime import datetime, timezone
import fcntl
import hashlib
import http.client
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import socket
import ssl
import subprocess
import tarfile
import tempfile
import time
from urllib.parse import parse_qs, urlencode, urlsplit
from urllib.request import Request, urlopen
from uuid import UUID

import yaml

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "app/cetirun-platform"
SUPPORT = ROOT / "infra/cert-manager"
SPEC = importlib.util.spec_from_file_location("ingress_yaml", ROOT / "scripts/ensure-oauth2-proxy-id-token-header.py")
YAML = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(YAML)
PROVIDERS = ("https://dns.google/resolve", "https://cloudflare-dns.com/dns-query")
CERT_CRDS = ("certificates.cert-manager.io", "certificaterequests.cert-manager.io", "issuers.cert-manager.io",
             "clusterissuers.cert-manager.io", "orders.acme.cert-manager.io", "challenges.acme.cert-manager.io")
CLUSTER_KINDS = {"Namespace", "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding",
                 "MutatingWebhookConfiguration", "ValidatingWebhookConfiguration", "GatewayClass"}
CERTIFICATES = {"staging": "cetirun-tls-staging", "production": "cetirun-tls"}


class IngressError(ValueError):
    """An actionable failure whose message contains no tool output or Secret data."""


def require(condition, message):
    if not condition:
        raise IngressError(message)


def load_yaml(path):
    try:
        return yaml.load(Path(path).read_text(), Loader=YAML.UniqueKeyLoader)
    except (OSError, yaml.YAMLError) as exc:
        mark = getattr(exc, "problem_mark", None)
        position = f" at line {mark.line + 1}" if mark else ""
        raise IngressError("Cannot read configuration YAML" + position) from None


def keys(value, allowed, name):
    require(isinstance(value, dict), name + " must be a mapping")
    require(not set(value) - set(allowed), name + " contains unsupported fields")


def text_field(value, name):
    require(isinstance(value, str) and bool(value) and value == value.strip()
            and not any(ord(c) < 32 for c in value), name + " must be a nonempty string without control characters")
    return value


def dns_name(value):
    return (isinstance(value, str) and len(value) <= 253 and "." in value
            and all(re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", p) for p in value.split(".")))


def label(value):
    return isinstance(value, str) and bool(re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", value))


def absolute_path(value, name):
    text_field(value, name)
    require(Path(value).is_absolute(), name + " must be an absolute path")
    return value


def normalize(data):
    keys(data, {"target", "ingress", "dns", "azure", "certManager", "acme", "stateDirectory", "timeoutSeconds"}, "configuration")
    c = copy.deepcopy(data)
    target = c.setdefault("target", {})
    keys(target, {"kubeconfig", "context", "cluster", "server", "clusterUID"}, "target")
    absolute_path(target.get("kubeconfig"), "target.kubeconfig")
    for key in ("context", "cluster", "server", "clusterUID"):
        text_field(target.get(key), "target." + key)
    server = urlsplit(target["server"])
    require(server.scheme == "https" and server.hostname and not server.username and not server.password
            and not server.query and not server.fragment and server.path in ("", "/"), "target.server must be an HTTPS API origin")
    require(bool(re.fullmatch(r"[A-Za-z0-9-]+", target["clusterUID"])), "target.clusterUID is invalid")
    ingress = c.setdefault("ingress", {})
    keys(ingress, {"namespace", "service", "release", "releaseNamespace", "gatewayName"}, "ingress")
    for key, default in {"namespace": "aks-istio-ingress", "service": "aks-istio-ingressgateway-external",
                         "release": "cetirun-platform", "releaseNamespace": "cetirun-system", "gatewayName": "cetirun-acme-http"}.items():
        ingress.setdefault(key, default)
        require(label(ingress[key]), "ingress." + key + " must be a DNS label")
    require(len(ingress["release"]) <= 53, "ingress.release exceeds Helm's name limit")
    dns = c.setdefault("dns", {})
    keys(dns, {"mode", "label", "domain"}, "dns")
    require(dns.get("mode") in ("azure-public-ip", "custom"), "dns.mode must be azure-public-ip or custom")
    if dns["mode"] == "custom":
        require(dns_name(dns.get("domain")) and not dns.get("label") and not c.get("azure"),
                "Custom DNS requires one domain and no Azure DNS configuration")
        try:
            ipaddress.ip_address(dns["domain"])
        except ValueError:
            pass
        else:
            raise IngressError("dns.domain must be a hostname, not an IP address")
    else:
        require(label(dns.get("label")) and not dns.get("domain"), "Azure DNS requires dns.label, without a predicted domain")
        azure = c.setdefault("azure", {})
        keys(azure, {"subscriptionId", "tenantId", "resourceGroup", "clusterName"}, "azure")
        for key in ("subscriptionId", "tenantId"):
            try:
                require(str(UUID(azure.get(key, ""))) == azure[key].lower(), "azure." + key + " must be a UUID")
            except (ValueError, TypeError, AttributeError, KeyError):
                raise IngressError("azure." + key + " must be a UUID") from None
        for key in ("resourceGroup", "clusterName"):
            require(isinstance(azure.get(key), str) and bool(re.fullmatch(r"[A-Za-z0-9_.()-]+", azure[key])), "azure." + key + " is invalid")
    cm = c.setdefault("certManager", {})
    keys(cm, {"mode", "namespace", "release", "chartArchive"}, "certManager")
    cm.setdefault("mode", "install")
    cm.setdefault("namespace", "cert-manager")
    cm.setdefault("release", "cert-manager")
    require(cm["mode"] in ("install", "existing"), "certManager.mode must be install or existing")
    require(label(cm["namespace"]) and label(cm["release"]) and len(cm["release"]) <= 53, "certManager namespace/release must be valid names")
    require(cm["namespace"] not in (ingress["namespace"], ingress["releaseNamespace"]), "cert-manager must use a separate namespace")
    if cm["mode"] == "install":
        absolute_path(cm.get("chartArchive"), "certManager.chartArchive")
    else:
        require(not cm.get("chartArchive"), "Existing cert-manager does not use chartArchive")
    acme = c.setdefault("acme", {})
    keys(acme, {"email"}, "acme")
    acme.setdefault("email", "")
    require(isinstance(acme["email"], str) and (not acme["email"] or bool(re.fullmatch(r"[^\s@]+@[^\s@]+\.[^\s@]+", acme["email"]))), "acme.email must be empty or an email address")
    c.setdefault("timeoutSeconds", 600)
    require(type(c["timeoutSeconds"]) is int and 30 <= c["timeoutSeconds"] <= 1800, "timeoutSeconds must be an integer from 30 to 1800")
    absolute_path(c.get("stateDirectory"), "stateDirectory")
    state = Path(c["stateDirectory"]).resolve()
    require(state != ROOT and ROOT not in state.parents, "stateDirectory must be outside the repository")
    return c


def chart_values(c, domain, production=False, bootstrap=True):
    i = c["ingress"]
    values = {"publicTLS": {"enabled": True, "bootstrapOnly": bootstrap,
              "domain": domain, "ingressNamespace": i["namespace"],
              "ingressServiceHostname": f"{i['service']}.{i['namespace']}.svc.cluster.local",
              "gateway": {"name": i["gatewayName"]},
              "acme": {"email": c["acme"]["email"], "stagingCertificateEnabled": True,
                       "productionCertificateEnabled": production}}}
    if bootstrap:
        values.update({name: {"enabled": False} for name in
                       ("cetirun-operator", "cetirun-portal", "cetirun-ai-gateway", "cetirun-connections-mcp")})
        values["istio"] = {"enabled": False}
        values["global"] = {"portalChat": {"enabled": False}}
    return values


def metadata_owned(obj, release, namespace):
    annotations = obj.get("metadata", {}).get("annotations", {})
    return (annotations.get("meta.helm.sh/release-name") == release
            and annotations.get("meta.helm.sh/release-namespace") == namespace
            and obj.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/managed-by") == "Helm")


def condition(obj, name):
    status = obj.get("status", {})
    generation = obj.get("metadata", {}).get("generation")
    return any(c.get("type") == name and c.get("status") == "True"
               and (generation is None or c.get("observedGeneration", status.get("observedGeneration")) == generation)
               for c in status.get("conditions", []))


def contains(expected, actual):
    """Accept API defaults while requiring every explicitly managed field."""
    if isinstance(expected, dict):
        return isinstance(actual, dict) and all(k in actual and contains(v, actual[k]) for k, v in expected.items())
    if isinstance(expected, list):
        return isinstance(actual, list) and len(expected) == len(actual) and all(contains(a, b) for a, b in zip(expected, actual))
    return expected == actual


def document_digest(documents):
    """Return a stable identity for rendered, non-secret Helm documents."""
    encoded = json.dumps(documents, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(encoded).hexdigest()


def option(args, name):
    matches = []
    for index, arg in enumerate(args):
        if arg.startswith(name + "="):
            matches.append(arg.split("=", 1)[1])
        elif arg == name:
            matches.append(args[index + 1] if index + 1 < len(args) and not args[index + 1].startswith("--") else "true")
    require(len(matches) <= 1, "Ambiguous cert-manager controller arguments")
    return matches[0] if matches else None


def gateway_api_enabled(document):
    """Match cert-manager v1.21.3's current setting and deprecated fallback."""
    if not isinstance(document, dict):
        return False
    gateway = document.get("gatewayAPI")
    if gateway is not None and not isinstance(gateway, dict):
        return False
    enabled = gateway.get("enabled") if isinstance(gateway, dict) else None
    if enabled is None:
        enabled = document.get("enableGatewayAPI")
    return enabled is True


def tls_context():
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    return context


def query_dns(provider, domain, record_type):
    req = Request(provider + "?" + urlencode({"name": domain, "type": record_type}), headers={"Accept": "application/dns-json"})
    with urlopen(req, timeout=15, context=tls_context()) as response:
        result = json.load(response)
    require(result.get("Status") == 0, "Public DNS lookup did not succeed")
    kind = {"A": 1, "AAAA": 28}[record_type]
    return {answer["data"] for answer in result.get("Answer", []) if answer.get("type") == kind}


def check_dns(domain, ip):
    # Exact final A answers allow CNAMEs but intentionally exclude CDN proxies,
    # stale addresses and unsupported IPv6 paths that could receive a challenge.
    for provider in PROVIDERS:
        require(query_dns(provider, domain, "A") == {ip} and not query_dns(provider, domain, "AAAA"),
                "Public DNS must resolve only to the IPv4 ingress address, without an AAAA record")


def service_identity(service):
    spec = service.get("spec", {})
    addresses = service.get("status", {}).get("loadBalancer", {}).get("ingress", [])
    require(spec.get("type") == "LoadBalancer" and len(addresses) == 1, "Expected one existing external LoadBalancer address")
    ip = addresses[0].get("ip", "")
    require(isinstance(ipaddress.ip_address(ip), ipaddress.IPv4Address), "Only an IPv4 public ingress is supported")
    require({80, 443}.issubset({p["port"] for p in spec.get("ports", []) if p.get("protocol", "TCP") == "TCP"}),
            "Existing ingress Service must expose TCP 80 and 443")
    require(bool(spec.get("selector")), "Ingress Service must select managed Istio Pods")
    return {"uid": service["metadata"]["uid"], "ip": ip, "ports": spec["ports"], "selector": spec["selector"]}


class Ingress:
    def __init__(self, config, scratch):
        self.config, self.scratch = config, Path(scratch)
        self.state_dir = Path(config["stateDirectory"])
        source = Path(tempfile.mkdtemp(prefix="chart-", dir=self.scratch))
        self.chart = source / "app/cetirun-platform"
        shutil.copytree(CHART, self.chart, ignore=shutil.ignore_patterns("charts", "__pycache__"))
        for dependency in load_yaml(CHART / "Chart.yaml")["dependencies"]:
            relative = dependency["repository"].removeprefix("file://")
            origin = (CHART / relative).resolve()
            require(ROOT in origin.parents, "Platform dependencies must be local repository charts")
            shutil.copytree(origin, self.chart / relative, ignore=shutil.ignore_patterns("charts", "__pycache__"))
        self.dependencies_built = False
        self.support = source / "infra/cert-manager"
        self.support.mkdir(parents=True)
        for filename in ("images.lock.json", "cert-manager-values.yaml"):
            shutil.copyfile(SUPPORT / filename, self.support / filename)
        digest = hashlib.sha256()
        for path in sorted([*self.chart.rglob("*"), *self.support.rglob("*")]):
            if path.is_file():
                content = path.read_bytes()
                digest.update(str(path.relative_to(source)).encode() + b"\0")
                digest.update(len(content).to_bytes(8, "big") + content)
        self.chart_sha256 = digest.hexdigest()
        self.cm_archive = None
        self.lock = json.loads((self.support / "images.lock.json").read_text())
        self.state = {}
        self.domain = config["dns"].get("domain")
        self.service = None
        self.cm_documents = []
        self.documents = []
        self.api_groups = set()
        self.production_exists = False
        self.cm_exists = False
        self.cm_needs_install = False
        self.cm_recovery = False
        self.cm_recovery_uids = {}
        self.cm_replacing = False
        self.public_ip = None
        self.helm_major = None

    def run(self, command, *, offline=False, input_text=None, timeout=60):
        env = {**os.environ, "KUBECONFIG": os.devnull if offline else self.config["target"]["kubeconfig"],
               "AZURE_EXTENSION_USE_DYNAMIC_INSTALL": "no", "GODEBUG": "http2client=0", "DISABLE_HTTP2": "1", "HELM_DRIVER": "secret"}
        if offline:
            env.update(HELM_REGISTRY_CONFIG=os.devnull, HELM_REPOSITORY_CONFIG=os.devnull)
        try:
            result = subprocess.run(list(map(str, command)), input=input_text, text=True, capture_output=True,
                                    env=env, timeout=timeout, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise IngressError(Path(command[0]).name + " failed or timed out; inspect actual state before retrying") from None
        require(result.returncode == 0, Path(command[0]).name + " command failed; tool output is withheld because it may contain credentials")
        return result.stdout

    def kube(self, *args, **kwargs):
        return self.run(["kubectl", "--context", self.config["target"]["context"], "--request-timeout=30s", *args], **kwargs)

    def helm(self, *args, **kwargs):
        return self.run(["helm", "--kube-context", self.config["target"]["context"], *args], **kwargs)

    def az(self, *args):
        return json.loads(self.run(["az", *args, "--subscription", self.config["azure"]["subscriptionId"], "--output", "json"]))

    def get(self, kind, name, namespace=None, *, metadata_only=False):
        args = ["get", kind, name, "--ignore-not-found", "-o", "jsonpath={.metadata}" if metadata_only else "json"]
        if namespace:
            args += ["--namespace", namespace]
        raw = self.kube(*args)
        obj = json.loads(raw) if raw.strip() else None
        return {"metadata": obj} if obj and metadata_only else obj

    def private_write(self, name, value):
        path = self.state_dir / name
        require(not path.is_symlink(), "Private output must not be a symbolic link")
        descriptor, temporary = tempfile.mkstemp(prefix=".ingress-", dir=self.state_dir)
        try:
            with os.fdopen(descriptor, "w") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(value)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
            directory = os.open(self.state_dir, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def save(self):
        self.private_write("state.json", json.dumps(self.state, indent=2) + "\n")

    def mutate(self, name, action):
        self.state["lastStep"] = {"name": name, "status": "started"}
        self.state.setdefault("steps", []).append(self.state["lastStep"])
        self.save()
        try:
            result = action()
        except Exception:
            self.state["lastStep"]["status"] = "uncertain"
            self.save()
            raise
        self.state["lastStep"]["status"] = "completed"
        self.save()
        return result

    def load_state(self, write=False):
        require(not self.state_dir.is_symlink(), "stateDirectory must not be a symbolic link")
        if self.state_dir.exists():
            require(self.state_dir.is_dir() and self.state_dir.stat().st_mode & 0o077 == 0,
                    "stateDirectory must be a private directory (0700)")
        elif write:
            self.state_dir.mkdir(parents=True, mode=0o700)
        path = self.state_dir / "state.json"
        if path.exists():
            require(not path.is_symlink() and path.stat().st_mode & 0o077 == 0, "state.json must be private and not a symbolic link")
            try:
                self.state = json.loads(path.read_text())
            except (OSError, ValueError):
                raise IngressError("Cannot read prior ingress state") from None
        coordinates = {k: self.config.get(k) for k in ("target", "ingress", "dns", "azure")}
        coordinates["certManager"] = {k: self.config["certManager"][k] for k in ("namespace", "release")}
        fingerprint = hashlib.sha256(json.dumps(coordinates, sort_keys=True).encode()).hexdigest()
        require(not self.state or self.state.get("identity") == fingerprint, "Private state belongs to a different target, domain or ingress")
        self.state.setdefault("identity", fingerprint)

    def archive_check(self):
        cm = self.config["certManager"]
        if cm["mode"] == "existing":
            return
        path = self.cm_archive or Path(cm["chartArchive"])
        require(path.is_file(), "Pinned cert-manager chart archive is missing; download it separately")
        content = path.read_bytes()
        require(hashlib.sha256(content).hexdigest() == self.lock["chart"]["archiveSHA256"],
                "cert-manager chart archive checksum does not match the supported artifact")
        if self.cm_archive is None:
            self.cm_archive = self.chart.parent / (self.chart.name + "-cert-manager.tgz")
            self.cm_archive.write_bytes(content)
        try:
            with tarfile.open(self.cm_archive) as archive:
                metadata = yaml.safe_load(archive.extractfile("cert-manager/Chart.yaml"))
            require(metadata["name"] == "cert-manager" and metadata["version"] == self.lock["chart"]["version"], "Unexpected cert-manager chart version")
        except (tarfile.TarError, KeyError, TypeError):
            raise IngressError("Invalid cert-manager chart archive") from None

    def render(self, domain=None):
        self.archive_check()
        if not self.dependencies_built:
            self.run(["helm", "dependency", "build", "--skip-refresh", str(self.chart)], offline=True)
            self.dependencies_built = True
        domain = domain or self.domain or "ingress.example.invalid"
        c = self.config
        values = self.scratch / "ingress-values.yaml"
        values.write_text(yaml.safe_dump(chart_values(c, domain, production=True)))
        raw = self.run(["helm", "template", c["ingress"]["release"], str(self.chart), "--namespace", c["ingress"]["releaseNamespace"],
                        "--values", str(values)], offline=True)
        self.documents = [d for d in yaml.safe_load_all(raw) if d]
        self.check_bootstrap_manifest(self.documents, production=True)
        if c["certManager"]["mode"] == "install":
            cm = c["certManager"]
            raw = self.run(["helm", "template", cm["release"], str(self.cm_archive), "--namespace", cm["namespace"],
                            "--include-crds", "--values", str(self.support / "cert-manager-values.yaml")], offline=True)
            self.cm_documents = [d for d in yaml.safe_load_all(raw) if d]
        return {"dnsMode": c["dns"]["mode"], "domain": self.domain,
                "domainResolution": "configured" if self.domain else "Azure-returned FQDN after label assignment",
                "certificateManager": c["certManager"]["mode"], "certificateManagerVersion": self.lock["chart"]["version"],
                "stages": ["target-and-ownership-preflight", "dns", "cert-manager", "staging", "production", "full-platform"],
                "resources": [{"kind": d["kind"], "name": d["metadata"]["name"]} for d in self.documents]}

    def release_status(self, release, namespace):
        # Helm 4 lists every release state by default and removed --all. Helm 3
        # still needs the flag so a failed or pending release is not hidden.
        if self.helm_major is None:
            version = self.helm("version", "--template", "{{ .Version }}").strip()
            match = re.fullmatch(r"v?([0-9]+)\.[0-9]+\.[0-9]+(?:[-+].*)?", version)
            require(match is not None, "Cannot determine Helm client version")
            major = int(match.group(1))
            require(major in (3, 4), "Only Helm 3 and Helm 4 are supported")
            self.helm_major = major
        arguments = ["list"]
        if self.helm_major == 3:
            arguments.append("--all")
        rows = json.loads(self.helm(*arguments, "--filter", "^" + re.escape(release) + "$", "--namespace", namespace, "-o", "json"))
        require(len(rows) <= 1, "Ambiguous Helm release")
        if rows:
            require(rows[0]["status"] in ("deployed", "failed"), "Helm release has an active or unsupported operation; inspect before retrying")
            return rows[0]
        return None

    def target_check(self):
        c, target = self.config, self.config["target"]
        kubeconfig = Path(target["kubeconfig"])
        require(kubeconfig.is_file(), "target.kubeconfig does not exist")
        selected = self.kube("config", "view", "--minify", "-o", 'jsonpath={.contexts[0].context.cluster}{"\\n"}{.clusters[0].cluster.server}')
        require(selected.splitlines() == [target["cluster"], target["server"]], "Kubeconfig target cluster/server mismatch")
        system = self.get("namespace", "kube-system")
        require(system and system["metadata"]["uid"] == target["clusterUID"], "Kubernetes cluster UID mismatch")
        i = c["ingress"]
        require(self.get("namespace", i["namespace"]), "Ingress namespace must already exist")
        self.service = service_identity(self.get("service", i["service"], i["namespace"]) or {})
        if self.state.get("service"):
            require(self.state["service"] == self.service, "Ingress Service identity/address/ports changed since preparation")
        self.state["service"] = self.service
        for crd in ("gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io"):
            obj = self.get("customresourcedefinition", crd)
            require(obj and any(v["name"] == "v1" and v.get("served") for v in obj["spec"]["versions"]), "Gateway API v1 must already be enabled")
        gateway_class = self.get("gatewayclass", "istio")
        require(gateway_class and gateway_class["spec"]["controllerName"] == "istio.io/gateway-controller"
                and condition(gateway_class, "Accepted"), "An accepted Istio GatewayClass is required")
        for crd in CERT_CRDS:
            if self.get("customresourcedefinition", crd):
                self.api_groups.add(crd)

    def azure_check(self):
        a = self.config["azure"]
        account = self.az("account", "show")
        require(account["id"].lower() == a["subscriptionId"].lower() and account["tenantId"].lower() == a["tenantId"].lower(), "Azure subscription/tenant mismatch")
        aks = self.az("aks", "show", "--resource-group", a["resourceGroup"], "--name", a["clusterName"])
        expected_id = f"/subscriptions/{a['subscriptionId']}/resourceGroups/{a['resourceGroup']}/providers/Microsoft.ContainerService/managedClusters/{a['clusterName']}"
        require(aks["id"].lower() == expected_id.lower(), "Azure AKS resource mismatch")
        require(urlsplit(self.config["target"]["server"]).hostname in (aks.get("fqdn"), aks.get("privateFqdn")), "AKS resource does not match kubeconfig API server")
        ips = self.az("network", "public-ip", "list", "--resource-group", aks["nodeResourceGroup"])
        matches = [ip for ip in ips if ip.get("ipAddress") == self.service["ip"]]
        require(len(matches) == 1, "Expected exactly one matching ingress Public IP in the AKS node resource group")
        ip = matches[0]
        require(ip["sku"]["name"] == "Standard" and ip["publicIPAllocationMethod"] == "Static", "Ingress Public IP must be Standard and Static")
        dns = ip.get("dnsSettings") or {}
        require(not dns.get("domainNameLabel") or dns["domainNameLabel"] == self.config["dns"]["label"], "Public IP already has another DNS label; migration is not automatic")
        if dns.get("domainNameLabel"):
            require(dns_name(dns.get("fqdn")), "Azure did not return a valid FQDN")
            self.domain = dns["fqdn"]
        self.public_ip = ip
        if self.state.get("publicIPId"):
            require(ip["id"].lower() == self.state["publicIPId"].lower(), "Azure Public IP resource changed")
        self.state["publicIPId"] = ip["id"]

    def resource_check(self, doc, release, namespace):
        group = doc["apiVersion"].split("/")[0] if "/" in doc["apiVersion"] else ""
        if group in ("cert-manager.io", "acme.cert-manager.io") and not self.api_groups.issuperset({"certificates.cert-manager.io"}):
            return None
        kind = doc["kind"].lower() + ("." + group if group else "")
        ns = None if doc["kind"] in CLUSTER_KINDS else doc["metadata"].get("namespace", namespace)
        existing = self.get(kind, doc["metadata"]["name"], ns, metadata_only=doc["kind"] == "Secret")
        if existing:
            owned = metadata_owned(existing, release, namespace)
            cm = self.config["certManager"]
            require(owned, "Resource ownership conflict: " + doc["kind"] + "/" + doc["metadata"]["name"])
        return existing

    def cm_values(self):
        return load_yaml(self.support / "cert-manager-values.yaml")

    def cm_hook(self, doc):
        return "helm.sh/hook" in doc.get("metadata", {}).get("annotations", {})

    def cm_post_install_hook(self, doc):
        annotations = doc.get("metadata", {}).get("annotations", {})
        return "post-install" in annotations.get("helm.sh/hook", "").split(",")

    def cm_manifest_documents(self):
        return [doc for doc in self.cm_documents if not self.cm_hook(doc)]

    def cm_hook_documents(self):
        return [doc for doc in self.cm_documents if self.cm_hook(doc)]

    def cm_resource_id(self, doc, namespace):
        ns = "" if doc["kind"] in CLUSTER_KINDS else doc["metadata"].get("namespace", namespace)
        return doc["apiVersion"] + ":" + doc["kind"] + ":" + ns + "/" + doc["metadata"]["name"]

    def cm_resource_metadata_matches(self, doc, existing, release, namespace):
        expected = doc.get("metadata", {})
        actual = existing.get("metadata", {})
        annotations = dict(expected.get("annotations", {}))
        annotations.update({
            "meta.helm.sh/release-name": release,
            "meta.helm.sh/release-namespace": namespace,
        })
        actual_annotations = dict(actual.get("annotations", {}))
        if doc["kind"] == "Deployment" and "deployment.kubernetes.io/revision" in actual_annotations:
            require(bool(re.fullmatch(r"[1-9][0-9]*", actual_annotations["deployment.kubernetes.io/revision"])),
                    "cert-manager Deployment revision annotation is invalid; recover it separately")
            del actual_annotations["deployment.kubernetes.io/revision"]
        return (actual.get("labels", {}) == expected.get("labels", {})
                and actual_annotations == annotations
                and not actual.get("ownerReferences") and not actual.get("finalizers"))

    def cm_expected_resource(self, doc, existing):
        """Return the server-admitted object for a no-persistence replacement."""
        desired = copy.deepcopy(doc)
        desired.setdefault("metadata", {})["resourceVersion"] = existing["metadata"].get("resourceVersion")
        require(desired["metadata"]["resourceVersion"],
                "Failed cert-manager retained resource has no resource version; recover it separately")
        if desired["kind"] == "Service":
            # ClusterIP allocation fields are immutable and omitted by Helm's
            # render. Preserve exactly the API-assigned values for a dry-run
            # replacement; all other Service fields remain chart-controlled.
            actual_spec = existing.get("spec", {})
            target_spec = desired.setdefault("spec", {})
            for name in ("clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "healthCheckNodePort"):
                if name in actual_spec:
                    target_spec[name] = actual_spec[name]
        try:
            raw = self.kube("replace", "--dry-run=server", "-f", "-", "-o", "json",
                            input_text=yaml.safe_dump(desired))
            return json.loads(raw)
        except (ValueError, KeyError):
            raise IngressError("Cannot dry-run the cert-manager retained resource; recover it separately") from None

    def cm_resource_matches(self, expected, existing):
        """Require the server-admitted Helm object, except documented injectors.

        cert-manager's cainjector is the sole exception: it writes CA bundles
        into annotated webhook/APIService objects after Helm creates them.
        """
        doc = expected
        expected = copy.deepcopy({key: value for key, value in expected.items() if key not in ("metadata", "status")})
        actual = copy.deepcopy({key: value for key, value in existing.items() if key not in ("metadata", "status")})
        annotations = doc.get("metadata", {}).get("annotations", {})
        injected = ("cert-manager.io/inject-ca-from-secret" in annotations
                    or "cert-manager.io/inject-ca-from" in annotations)
        if injected and doc["kind"] in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"):
            for webhook in actual.get("webhooks", []):
                bundle = webhook.get("clientConfig", {}).get("caBundle")
                require(isinstance(bundle, str) and bool(bundle),
                        "cert-manager webhook has no injected CA bundle; recover it separately")
                webhook["clientConfig"].pop("caBundle")
        if injected and doc["kind"] == "APIService":
            bundle = actual.get("spec", {}).get("caBundle")
            require(isinstance(bundle, str) and bool(bundle),
                    "cert-manager APIService has no injected CA bundle; recover it separately")
            actual["spec"].pop("caBundle")
        if doc["kind"] == "ValidatingWebhookConfiguration":
            # AKS admission appends exactly these exclusions to cert-manager's
            # webhook namespace selectors. They are platform defaults, not
            # chart drift. Do not relax any other selector expression.
            aks_defaults = (
                {"key": "control-plane", "operator": "NotIn", "values": ["true"]},
                {"key": "kubernetes.azure.com/managedby", "operator": "NotIn", "values": ["aks"]},
            )
            expected_webhooks = expected.get("webhooks", [])
            for index, webhook in enumerate(actual.get("webhooks", [])):
                selector = webhook.get("namespaceSelector", {})
                expressions = selector.get("matchExpressions")
                if isinstance(expressions, list):
                    expected_selector = expected_webhooks[index].get("namespaceSelector", {}) if index < len(expected_webhooks) else {}
                    expected_expressions = expected_selector.get("matchExpressions")
                    if expressions == expected_expressions:
                        continue
                    if not isinstance(expected_expressions, list):
                        return False
                    if expressions != expected_expressions + list(aks_defaults):
                        return False
                    selector["matchExpressions"] = expected_expressions
        return expected == actual

    def cm_release_documents(self, command):
        raw = self.helm(*command)
        try:
            return [doc for doc in yaml.safe_load_all(raw) if doc]
        except yaml.YAMLError:
            raise IngressError("Cannot parse cert-manager Helm release metadata; recover it separately") from None

    def cm_release_matches_render(self, cm):
        try:
            values = json.loads(self.helm("get", "values", cm["release"], "--namespace", cm["namespace"], "-o", "json"))
        except ValueError:
            raise IngressError("Cannot parse cert-manager Helm values; recover it separately") from None
        require(values == self.cm_values(), "Failed cert-manager release values differ; recover it separately")
        manifest = self.cm_release_documents(("get", "manifest", cm["release"], "--namespace", cm["namespace"]))
        hooks = self.cm_release_documents(("get", "hooks", cm["release"], "--namespace", cm["namespace"]))
        require(document_digest(manifest) == document_digest(self.cm_manifest_documents()),
                "Failed cert-manager release render differs; recover it separately")
        require(document_digest(hooks) == document_digest(self.cm_hook_documents()),
                "Failed cert-manager release hooks differ; recover it separately")

    def cm_recovery_check(self, existing):
        """Authorize only a retry of the recorded, failed first installation."""
        cm = self.config["certManager"]
        receipt = self.state.get("certManagerInstall")
        expected = {
            "archiveSHA256": self.lock["chart"]["archiveSHA256"],
            "chartVersion": self.lock["chart"]["version"],
            "valuesSHA256": document_digest(self.cm_values()),
            "renderSHA256": document_digest(self.cm_documents),
        }
        steps = [step for step in self.state.get("steps", []) if step.get("name") == "cert-manager-install"]
        replace_steps = [step for step in self.state.get("steps", []) if step.get("name") == "cert-manager-install-replace"]
        require(len(steps) == 1 and steps[0].get("status") in ("completed", "uncertain"),
                "Failed cert-manager release was not installed by this preparation; recover it separately")
        require(not replace_steps or (self.cm_replacing and len(replace_steps) == 1
                                     and replace_steps[0].get("status") == "started"),
                "cert-manager replacement outcome is unknown; inspect before retrying")
        require(receipt == expected and self.state.get("certManagerArchiveSHA256") == expected["archiveSHA256"],
                "Failed cert-manager release receipt does not match the pinned render; recover it separately")
        try:
            revision = int(existing.get("revision"))
        except (TypeError, ValueError):
            revision = 0
        require(existing.get("status") == "failed" and revision == 1,
                "Failed cert-manager release is not the first revision; recover it separately")
        self.cm_release_matches_render(cm)
        uids = {}
        for doc in self.cm_documents:
            ns = None if doc["kind"] in CLUSTER_KINDS else doc["metadata"].get("namespace", cm["namespace"])
            current = self.get(doc["kind"].lower() + ("." + doc["apiVersion"].split("/")[0] if "/" in doc["apiVersion"] else ""),
                               doc["metadata"]["name"], ns, metadata_only=doc["kind"] == "Secret")
            if self.cm_post_install_hook(doc):
                require(current is None, "Failed cert-manager post-install hook still exists; recover it separately")
                continue
            if current:
                require(metadata_owned(current, cm["release"], cm["namespace"]),
                        "Resource ownership conflict: " + doc["kind"] + "/" + doc["metadata"]["name"])
                require(self.cm_resource_metadata_matches(doc, current, cm["release"], cm["namespace"]),
                        "Failed cert-manager retained resource metadata differs; recover it separately")
                if doc["kind"] != "Secret":
                    require(self.cm_resource_matches(self.cm_expected_resource(doc, current), current),
                            "Failed cert-manager retained resource differs; recover it separately")
                uid = current.get("metadata", {}).get("uid")
                require(uid, "Failed cert-manager retained resource has no UID; recover it separately")
                uids[self.cm_resource_id(doc, cm["namespace"])] = uid
        self.cm_recovery_uids = uids

    def cm_replace_uncertain(self):
        steps = [step for step in self.state.get("steps", []) if step.get("name") == "cert-manager-install-replace"]
        return (len(steps) == 1 and steps[0].get("status") in ("started", "completed", "uncertain")
                and self.state.get("certManagerRecovery", {}).get("acceptedRevision") != 2)

    def cm_post_install_success(self, cm, revision):
        try:
            status = json.loads(self.helm("status", cm["release"], "--namespace", cm["namespace"],
                                          "--revision", str(revision), "-o", "json"))
        except ValueError:
            raise IngressError("Cannot parse cert-manager Helm hook status; inspect before retrying") from None
        expected = {(doc["kind"], doc["metadata"]["name"])
                    for doc in self.cm_hook_documents() if self.cm_post_install_hook(doc)}
        succeeded = {(hook.get("kind"), hook.get("name")) for hook in status.get("hooks", [])
                     if "post-install" in hook.get("events", [])
                     and hook.get("last_run", {}).get("phase") == "Succeeded"}
        require(expected and expected.issubset(succeeded),
                "cert-manager post-install startup check did not succeed")

    def cm_recovery_acceptance(self, existing):
        """Verify a completed or uncertain replacement without replaying it."""
        cm = self.config["certManager"]
        try:
            revision = int(existing.get("revision"))
        except (TypeError, ValueError):
            revision = 0
        require(existing.get("status") == "deployed" and revision == 2,
                "cert-manager replacement did not create deployed revision 2; inspect before retrying")
        self.cm_release_matches_render(cm)
        self.cm_post_install_success(cm, revision)
        baseline = self.state.get("certManagerRecovery", {}).get("resourceUIDs")
        require(isinstance(baseline, dict), "cert-manager replacement has no retained-resource baseline; inspect before retrying")
        for doc in self.cm_documents:
            if self.cm_post_install_hook(doc):
                continue
            ns = None if doc["kind"] in CLUSTER_KINDS else doc["metadata"].get("namespace", cm["namespace"])
            current = self.get(doc["kind"].lower() + ("." + doc["apiVersion"].split("/")[0] if "/" in doc["apiVersion"] else ""),
                               doc["metadata"]["name"], ns, metadata_only=doc["kind"] == "Secret")
            require(current and metadata_owned(current, cm["release"], cm["namespace"]),
                    "cert-manager replacement did not retain managed resource ownership; inspect before retrying")
            require(self.cm_resource_metadata_matches(doc, current, cm["release"], cm["namespace"]),
                    "cert-manager replacement retained resource metadata differs; inspect before retrying")
            if doc["kind"] != "Secret":
                require(self.cm_resource_matches(self.cm_expected_resource(doc, current), current),
                        "cert-manager replacement retained resource differs; inspect before retrying")
            resource_id = self.cm_resource_id(doc, cm["namespace"])
            if resource_id in baseline:
                require(current.get("metadata", {}).get("uid") == baseline[resource_id],
                        "cert-manager replacement changed a retained resource UID; inspect before retrying")
        self.check_controller()
        self.state["certManagerRecovery"]["acceptedRevision"] = 2
        self.save()

    def cert_manager_check(self):
        self.cm_needs_install = False
        cm = self.config["certManager"]
        existing = self.release_status(cm["release"], cm["namespace"])
        self.cm_exists = bool(existing)
        self.cm_recovery = False
        self.cm_recovery_uids = {}
        if cm["mode"] == "existing":
            require(existing and existing["status"] == "deployed", "Existing mode requires the named ready cert-manager Helm release")
        if existing:
            require(existing.get("chart") == "cert-manager-" + self.lock["chart"]["version"], "Existing cert-manager is not the supported pinned version; no automatic upgrade")
            if existing["status"] == "failed":
                require(cm["mode"] == "install", "Failed cert-manager release must be recovered separately")
                self.cm_recovery_check(existing)
                self.cm_recovery = True
                self.cm_needs_install = True
            else:
                if cm["mode"] == "install" and self.cm_replace_uncertain():
                    self.cm_recovery_acceptance(existing)
                else:
                    self.check_controller()
        else:
            require(not self.api_groups, "cert-manager resources already exist without the configured release; explicit existing ownership is required")
            self.cm_needs_install = True
        if cm["mode"] == "install":
            for doc in self.cm_documents:
                if self.cm_recovery and self.cm_post_install_hook(doc):
                    continue
                self.resource_check(doc, cm["release"], cm["namespace"])

    def check_controller(self):
        cm = self.config["certManager"]
        require(len(self.api_groups) == len(CERT_CRDS), "Existing cert-manager CRDs are incomplete")
        deployments = json.loads(self.kube("get", "deployments", "--namespace", cm["namespace"], "--selector",
                                         "app.kubernetes.io/instance=" + cm["release"], "-o", "json"))["items"]
        require(len(deployments) == 3, "Expected the three pinned cert-manager deployments")
        found = set()
        for dep in deployments:
            containers = dep["spec"]["template"]["spec"]["containers"]
            for container in containers:
                for role in ("controller", "webhook", "cainjector"):
                    if container["image"].split("@")[-1] == self.lock[role]["digest"]:
                        found.add(role)
                        if role == "controller":
                            args = container.get("args", [])
                            require(option(args, "--acme-http01-solver-image") == self.lock["acmesolver"]["image"],
                                    "cert-manager HTTP-01 solver must use the supported pinned image")
                            enabled = False
                            config_path = option(args, "--config")
                            for mount in container.get("volumeMounts", []):
                                if config_path and config_path.startswith(mount["mountPath"].rstrip("/") + "/"):
                                    filename = config_path[len(mount["mountPath"].rstrip("/")) + 1:]
                                    volume = next((v for v in dep["spec"]["template"]["spec"].get("volumes", [])
                                                   if v["name"] == mount["name"] and "configMap" in v), None)
                                    require(volume is not None and not mount.get("subPath"), "Unsupported cert-manager controller configuration mount")
                                    source = volume["configMap"]
                                    key = filename
                                    if "items" in source:
                                        key = next((item["key"] for item in source["items"] if item["path"] == filename), None)
                                    config = self.get("configmap", source["name"], cm["namespace"])
                                    require(config and key in config.get("data", {}), "cert-manager controller config was not resolved")
                                    document = yaml.load(config["data"][key], Loader=YAML.UniqueKeyLoader)
                                    enabled = gateway_api_enabled(document)
                            override = option(args, "--enable-gateway-api")
                            if override is not None:
                                enabled = override == "true"
                            require(enabled, "Existing cert-manager must enable Gateway API support")
            replicas = dep["spec"].get("replicas", 1)
            status = dep.get("status", {})
            require(replicas > 0 and status.get("observedGeneration") == dep["metadata"].get("generation")
                    and status.get("updatedReplicas", 0) == replicas and status.get("readyReplicas", 0) == replicas
                    and status.get("replicas", 0) == replicas, "cert-manager deployment is not ready at its current generation")
        require(found == {"controller", "webhook", "cainjector"}, "cert-manager deployment image digests differ from the supported version")

    def certificate_secret_check(self, certificate):
        name = certificate["spec"]["secretName"]
        secret = self.get("secret", name, self.config["ingress"]["namespace"], metadata_only=True)
        if secret:
            metadata = secret["metadata"]
            require(metadata.get("annotations", {}).get("cert-manager.io/certificate-name") == certificate["metadata"]["name"]
                    and not metadata.get("annotations", {}).get("meta.helm.sh/release-name"),
                    "TLS Secret is not owned by the expected cert-manager Certificate; migration is not automatic")
            for owner in metadata.get("ownerReferences", []):
                require(owner.get("kind") == "Certificate" and owner.get("uid") == certificate["metadata"]["uid"], "TLS Secret owner reference differs from the Certificate")

    def check_bootstrap_manifest(self, documents, production=False):
        i = self.config["ingress"]
        expected = {
            ("gateway.networking.k8s.io/v1", "Gateway", i["gatewayName"]),
            ("gateway.networking.k8s.io/v1", "HTTPRoute", "cetirun-http-redirect"),
            ("cert-manager.io/v1", "Issuer", "cetirun-letsencrypt-staging"),
            ("cert-manager.io/v1", "Issuer", "cetirun-letsencrypt-production"),
            ("cert-manager.io/v1", "Certificate", "cetirun-tls-staging"),
        }
        actual = []
        for doc in documents:
            metadata = doc.get("metadata", {})
            require(metadata.get("namespace") == i["namespace"]
                    and not metadata.get("annotations", {}).get("helm.sh/hook"),
                    "Certificate bootstrap contains unexpected namespaces or hooks")
            actual.append((doc.get("apiVersion"), doc.get("kind"), metadata.get("name")))
        prod = ("cert-manager.io/v1", "Certificate", "cetirun-tls")
        allowed = (expected | {prod},) if production else (expected, expected | {prod})
        require(len(actual) == len(set(actual)) and set(actual) in allowed,
                "Certificate bootstrap contains application or unexpected resources; refusing downgrade")

    def platform_release_state(self):
        """The latest Helm revision, never a local receipt, authorizes bootstrap."""
        i = self.config["ingress"]
        release = self.release_status(i["release"], i["releaseNamespace"])
        if not release:
            self.platform_revision = 0
            return "absent"
        require(re.fullmatch(r"cetirun-platform-[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?",
                             str(release.get("chart", ""))),
                "Platform release uses another chart; migration is not automatic")
        revision = str(release.get("revision", ""))
        require(revision.isdigit() and int(revision) > 0, "Cannot establish the latest platform revision")
        self.platform_revision = int(revision)
        args = (i["release"], "--namespace", i["releaseNamespace"], "--revision", revision)
        values = json.loads(self.helm("get", "values", *args, "-o", "json")) or {}
        tls = values.get("publicTLS", {})
        if tls.get("bootstrapOnly") is not True:
            require(tls.get("enabled") is True,
                    "Existing full platform does not manage publicTLS; migrate it separately")
            return "full"
        require(tls.get("enabled") is True, "Certificate bootstrap requires publicTLS.enabled=true")
        require(release["chart"] == "cetirun-platform-" + load_yaml(self.chart / "Chart.yaml")["version"],
                "Certificate bootstrap chart version changed; inspect before recovery")
        raw = self.helm("get", "manifest", *args)
        self.check_bootstrap_manifest([doc for doc in yaml.safe_load_all(raw) if doc])
        hooks = self.helm("get", "hooks", *args)
        require(not any(doc for doc in yaml.safe_load_all(hooks)), "Certificate bootstrap has unexpected Helm hooks")
        return "bootstrap"

    def ingress_ownership_check(self):
        self.production_exists = False
        i = self.config["ingress"]
        self.platform_state = self.platform_release_state()
        for doc in self.documents:
            existing = self.resource_check(doc, i["release"], i["releaseNamespace"])
            require(existing or self.platform_state != "full",
                    "Full platform is missing managed publicTLS resources; certificate-only recovery is forbidden")
            if existing:
                if doc["kind"] in ("Gateway", "HTTPRoute"):
                    require(contains(doc["spec"], existing["spec"]), "Existing ACME Gateway/HTTPRoute configuration differs; migration is not automatic")
                if doc["kind"] == "Certificate":
                    require(contains(doc["spec"], existing["spec"]), "Existing Certificate configuration differs; migration is not automatic")
                    self.certificate_secret_check(existing)
                    if doc["metadata"]["name"] == CERTIFICATES["production"]:
                        self.production_exists = True
                if doc["kind"] == "Issuer":
                    require(contains(doc["spec"], existing["spec"]), "Existing ACME Issuer configuration differs; change it separately")
                    account = self.get("secret", doc["spec"]["acme"]["privateKeySecretRef"]["name"], i["namespace"], metadata_only=True)
                    if account:
                        metadata = account["metadata"]
                        require(not metadata.get("annotations", {}).get("meta.helm.sh/release-name")
                                and all(owner.get("kind") == "Issuer" and owner.get("uid") == existing["metadata"]["uid"]
                                        for owner in metadata.get("ownerReferences", [])),
                                "ACME account Secret belongs to a different owner")
            elif doc["kind"] == "Issuer":
                account_name = doc["spec"]["acme"]["privateKeySecretRef"]["name"]
                require(not self.get("secret", account_name, i["namespace"], metadata_only=True),
                        "ACME account Secret already exists without the managed Issuer; adoption is not automatic")
            elif doc["kind"] == "Certificate":
                require(not self.get("secret", doc["spec"]["secretName"], i["namespace"], metadata_only=True), "TLS Secret already exists without the managed Certificate")
        # Another Certificate must not write either output Secret, even if this
        # release's Certificate has not yet been created.
        if self.api_groups.issuperset({"certificates.cert-manager.io"}):
            certificates = json.loads(self.kube("get", "certificates.cert-manager.io", "--namespace", i["namespace"], "-o", "json"))["items"]
            for cert in certificates:
                if cert["spec"].get("secretName") in CERTIFICATES.values():
                    require(cert["metadata"]["name"] in CERTIFICATES.values()
                            and metadata_owned(cert, i["release"], i["releaseNamespace"]), "Another Certificate already writes the target TLS Secret")

    def preflight(self):
        self.api_groups = set()
        self.target_check()
        if self.config["dns"]["mode"] == "azure-public-ip":
            self.azure_check()
        self.render(self.domain)
        self.cert_manager_check()
        self.ingress_ownership_check()
        if self.domain:
            require(not self.state.get("domain") or self.state["domain"] == self.domain, "Public hostname changed since preparation")
        if self.config["dns"]["mode"] == "custom":
            check_dns(self.domain, self.service["ip"])

    def assign_dns(self):
        if not self.public_ip:
            return
        if not self.domain:
            require(not any(step.get("name") == "azure-dns-label" for step in self.state.get("steps", [])),
                    "A previous Azure DNS write has no confirmed label; inspect the Public IP and receipt before retrying")
            before = self.public_ip
            current = self.az("network", "public-ip", "show", "--ids", before["id"])
            require(current.get("etag") and current["etag"] == before.get("etag") and current["ipAddress"] == self.service["ip"], "Public IP changed after preflight")
            # Network Public IP PATCH only updates tags. The supported CLI
            # update reads the complete resource and PUTs its writable fields.
            # Network RP exposes no documented If-Match contract here; these
            # checks detect drift but do not serialize other Azure writers.
            self.mutate("azure-dns-label", lambda: self.az("network", "public-ip", "update", "--ids", before["id"],
                        "--dns-name", self.config["dns"]["label"]))
            updated = self.az("network", "public-ip", "show", "--ids", before["id"])
            require(updated["id"].lower() == before["id"].lower() and updated["ipAddress"] == self.service["ip"]
                    and updated["dnsSettings"]["domainNameLabel"] == self.config["dns"]["label"],
                    "Azure DNS assignment could not be confirmed")
            self.domain = updated["dnsSettings"]["fqdn"]
            require(dns_name(self.domain), "Azure did not return a valid FQDN")
        self.state["domain"] = self.domain
        self.save()
        self.until(lambda: self.dns_ready(), "DNS did not converge to the ingress address")

    def dns_ready(self):
        try:
            check_dns(self.domain, self.service["ip"])
            return True
        except (IngressError, OSError, ValueError):
            return False

    def until(self, check, message):
        deadline = time.monotonic() + self.config["timeoutSeconds"]
        while True:
            if check():
                return
            require(time.monotonic() < deadline, message)
            time.sleep(min(5, max(0, deadline - time.monotonic())))

    def install_cert_manager(self):
        if self.cm_exists and not self.cm_needs_install:
            return
        cm = self.config["certManager"]
        require(cm["mode"] == "install", "Existing cert-manager was not found")
        self.archive_check()
        self.state["certManagerArchiveSHA256"] = self.lock["chart"]["archiveSHA256"]
        if self.cm_recovery:
            # Retain this baseline in the private journal before a mutation.
            # A second check in the action closes the read/modify gap.
            self.state["certManagerRecovery"] = {"resourceUIDs": self.cm_recovery_uids}
            self.save()

            def replace_failed_first_install():
                self.cm_replacing = True
                try:
                    self.cert_manager_check()
                    require(self.cm_recovery, "Failed cert-manager release changed before recovery; recover it separately")
                    require(self.state.get("certManagerRecovery", {}).get("resourceUIDs") == self.cm_recovery_uids,
                            "Failed cert-manager retained resources changed before recovery; recover it separately")
                    return self.helm("install", "--replace", cm["release"], str(self.cm_archive), "--namespace", cm["namespace"],
                                     "--create-namespace", "--values", str(self.support / "cert-manager-values.yaml"),
                                     "--wait", "--timeout", str(self.config["timeoutSeconds"]) + "s",
                                     timeout=self.config["timeoutSeconds"] + 60)
                finally:
                    self.cm_replacing = False

            self.mutate("cert-manager-install-replace", replace_failed_first_install)
            self.api_groups = {name for name in CERT_CRDS if self.get("customresourcedefinition", name)}
            released = self.release_status(cm["release"], cm["namespace"])
            self.cm_recovery_acceptance(released or {})
            self.cm_recovery = False
        else:
            self.state["certManagerInstall"] = {
                "archiveSHA256": self.lock["chart"]["archiveSHA256"],
                "chartVersion": self.lock["chart"]["version"],
                "valuesSHA256": document_digest(self.cm_values()),
                "renderSHA256": document_digest(self.cm_documents),
            }
            self.mutate("cert-manager-install", lambda: self.helm("upgrade", "--install", cm["release"], str(self.cm_archive),
                        "--namespace", cm["namespace"], "--create-namespace", "--values", str(self.support / "cert-manager-values.yaml"),
                        "--wait", "--timeout", str(self.config["timeoutSeconds"]) + "s", timeout=self.config["timeoutSeconds"] + 60))
        self.api_groups = {name for name in CERT_CRDS if self.get("customresourcedefinition", name)}
        self.check_controller()

    def apply_stage(self, production):
        c, i = self.config, self.config["ingress"]
        # Re-read the latest revision immediately before every minimal upgrade.
        # A failed full upgrade is still full: never delete its workloads on retry.
        self.ingress_ownership_check()
        if self.platform_state != "full":
            path = self.scratch / "apply-values.yaml"
            values = chart_values(c, self.domain, production)
            # Helm allocates its next revision before rendering, then atomically
            # creates that revision before applying resources. The chart checks
            # this expectation so a competing full upgrade cannot slip between
            # our classification and a certificate-only Helm invocation.
            values["publicTLS"]["bootstrapRevision"] = self.platform_revision + 1
            path.write_text(yaml.safe_dump(values))
            self.mutate("production-resources" if production else "staging-resources", lambda: self.helm(
                "upgrade", "--install", i["release"], str(self.chart), "--namespace", i["releaseNamespace"],
                "--create-namespace", "--reset-values", "--values", str(path),
                "--wait", "--timeout", str(c["timeoutSeconds"]) + "s", timeout=c["timeoutSeconds"] + 60))
        self.until(lambda: condition(self.get("gateway.gateway.networking.k8s.io", i["gatewayName"], i["namespace"]) or {}, "Programmed"), "Manual ACME Gateway did not become Programmed")
        self.until(self.route_ready, "HTTP redirect route did not become Accepted with ResolvedRefs")
        for stage in ("staging", "production"):
            self.until(lambda stage=stage: self.issuer_ready(stage), "ACME Issuer did not become Ready: " + stage)
        self.preserve_service()

    def route_ready(self):
        i = self.config["ingress"]
        route = self.get("httproute.gateway.networking.k8s.io", "cetirun-http-redirect", i["namespace"]) or {}
        for parent in route.get("status", {}).get("parents", []):
            ref = parent.get("parentRef", {})
            if (ref.get("name") == i["gatewayName"] and ref.get("namespace", i["namespace"]) == i["namespace"]
                    and ref.get("group", "gateway.networking.k8s.io") == "gateway.networking.k8s.io"
                    and ref.get("kind", "Gateway") == "Gateway" and ref.get("sectionName") == "acme-http" and parent.get("controllerName") == "istio.io/gateway-controller"):
                observed = {"metadata": route.get("metadata", {}), "status": parent}
                if condition(observed, "Accepted") and condition(observed, "ResolvedRefs"):
                    return True
        return False

    def preserve_service(self):
        i = self.config["ingress"]
        require(service_identity(self.get("service", i["service"], i["namespace"]) or {}) == self.service,
                "Managed ingress Service identity, ports or address changed")

    def certificate_ready(self, stage):
        name = CERTIFICATES[stage]
        certificate = self.get("certificate.cert-manager.io", name, self.config["ingress"]["namespace"])
        return bool(certificate and condition(certificate, "Ready"))

    def issuer_ready(self, stage):
        issuer = self.get("issuer.cert-manager.io", "cetirun-letsencrypt-" + stage, self.config["ingress"]["namespace"])
        return bool(issuer and condition(issuer, "Ready"))

    def renewal_check(self, certificate):
        status = certificate.get("status", {})
        try:
            before, renewal, expiry = (datetime.fromisoformat(status[k].replace("Z", "+00:00"))
                                       for k in ("notBefore", "renewalTime", "notAfter"))
            scheduled = before <= datetime.now(timezone.utc) < renewal < expiry
        except (KeyError, TypeError, ValueError):
            scheduled = False
        require(scheduled and not status.get("failedIssuanceAttempts"),
                "Production Certificate has no healthy future renewal schedule; inspect its Issuer and issuance status")

    def public_certificate(self):
        i = self.config["ingress"]
        cert = self.get("certificate.cert-manager.io", CERTIFICATES["production"], i["namespace"])
        require(cert and condition(cert, "Ready"), "Production Certificate is not Ready")
        self.renewal_check(cert)
        self.certificate_secret_check(cert)
        # kubectl receives the Secret, but only its public certificate field is
        # returned to this process. No private key is parsed or persisted.
        encoded = self.kube("get", "secret", "cetirun-tls", "--namespace", i["namespace"], "-o", r"jsonpath={.data.tls\.crt}")
        try:
            pem = base64.b64decode(encoded, validate=True).decode("ascii")
        except (ValueError, UnicodeError):
            raise IngressError("TLS Secret has no usable public certificate") from None
        chain = re.findall(r"-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----", pem, re.S)
        require(bool(chain), "TLS Secret has no public certificate chain")
        leaf, intermediates = self.scratch / "leaf.pem", self.scratch / "chain.pem"
        leaf.write_text(chain[0] + "\n")
        intermediates.write_text("\n".join(chain[1:]) + "\n")
        self.run(["openssl", "x509", "-in", str(leaf), "-noout", "-checkend", "0"])
        command = ["openssl", "verify", "-purpose", "sslserver", "-verify_hostname", self.domain]
        if len(chain) > 1:
            command += ["-untrusted", str(intermediates)]
        self.run([*command, str(leaf)])
        return hashlib.sha256(ssl.PEM_cert_to_DER_cert(chain[0])).hexdigest()

    def prepare(self, resolved=None, complete=None):
        self.load_state(write=True)
        # Only one local writer may use these receipts. Never remove the lock
        # inode: another process may already hold an open descriptor to it.
        descriptor = os.open(self.state_dir / ".prepare.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        try:
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise IngressError("Another ingress preparation is using this stateDirectory") from None
            self.state = {}
            self.load_state()
            return self.prepare_locked(resolved=resolved, complete=complete)
        finally:
            os.close(descriptor)

    def prepare_locked(self, resolved=None, complete=None):
        self.preflight()
        # Recheck the stored target before writing a new receipt.
        prior_chart = self.state.get("ingressChartSHA256")
        require(getattr(self, "platform_state", "absent") == "full" or not prior_chart or prior_chart == self.chart_sha256,
                "Ingress chart source changed since preparation; review the prior receipt before using a new state directory")
        self.state["ingressChartSHA256"] = self.chart_sha256
        self.state["ingressChartVersion"] = load_yaml(self.chart / "Chart.yaml")["version"]
        self.state["service"] = self.service
        if self.public_ip:
            self.state["publicIPId"] = self.public_ip["id"]
        self.save()
        self.assign_dns()
        self.state["domain"] = self.domain
        self.save()
        self.render(self.domain)
        if resolved:
            resolved(self.domain)
        self.preserve_service()
        self.cert_manager_check()
        self.ingress_ownership_check()
        self.install_cert_manager()
        self.apply_stage(self.production_exists)
        self.until(lambda: self.certificate_ready("staging"), "Staging certificate did not become Ready; inspect Certificate/Order/Challenge status")
        self.apply_stage(True)
        self.until(lambda: self.certificate_ready("production"), "Production certificate did not become Ready; inspect Certificate/Order/Challenge status")
        fingerprint = self.public_certificate()
        self.preserve_service()
        self.state.update(domain=self.domain, certificateSHA256=fingerprint, prepared=True)
        self.save()
        if complete:
            self.mutate("full-platform-upgrade", complete)
        return {"domain": self.domain, "certificateReady": True,
                "oauthCallback": f"https://{self.domain}/oauth2/callback",
                "brokerCallback": f"https://{self.domain}/api/v1/connections/callback"}

    def http(self, path, secure=True):
        connection = (http.client.HTTPSConnection(self.domain, timeout=15, context=tls_context())
                      if secure else http.client.HTTPConnection(self.domain, timeout=15))
        try:
            connection.request("GET", path)
            response = connection.getresponse()
            result = response.status, response.getheader("Location"), response.read(4096).decode(errors="replace")
            return result
        finally:
            connection.close()

    def verify(self):
        self.load_state()
        self.preflight()
        require(self.domain, "Azure Public IP does not have its configured DNS label")
        check_dns(self.domain, self.service["ip"])
        i = self.config["ingress"]
        require(condition(self.get("gateway.gateway.networking.k8s.io", i["gatewayName"], i["namespace"]) or {}, "Programmed"),
                "ACME Gateway is not Programmed at its current generation")
        require(self.route_ready(), "HTTP redirect route is not Accepted with ResolvedRefs at its current generation")
        for stage in ("staging", "production"):
            require(self.issuer_ready(stage), "ACME Issuer is not Ready: " + stage)
        expected = self.public_certificate()
        with socket.create_connection((self.service["ip"], 443), timeout=15) as raw:
            with tls_context().wrap_socket(raw, server_hostname=self.domain) as tls:
                require(hashlib.sha256(tls.getpeercert(binary_form=True)).hexdigest() == expected, "Ingress is not serving the current cert-manager certificate")
        status, location, _ = self.http("/", secure=False)
        require(status == 301 and location == f"https://{self.domain}/", "Ordinary HTTP must redirect to HTTPS")
        status, location, _ = self.http("/")
        redirect = urlsplit(location or "")
        require(status in (302, 303) and redirect.scheme == "https" and redirect.hostname == "login.microsoftonline.com",
                "Portal root does not redirect to Microsoft sign-in")
        query = parse_qs(redirect.query)
        require(query.get("redirect_uri") == [f"https://{self.domain}/oauth2/callback"]
                and query.get("code_challenge_method") == ["S256"] and len(query.get("code_challenge", [])) == 1
                and bool(re.fullmatch(r"[A-Za-z0-9_-]{43}", query["code_challenge"][0])),
                "Microsoft sign-in must use the exact public callback and S256 PKCE")
        require(self.http("/api/v1/me")[0] == 401, "Unauthenticated Portal API must return 401")
        for path in ("/healthz", "/readyz"):
            code, _, body = self.http(path)
            require(code == 200 and body.strip() == "ok", "Portal health/readiness did not succeed")
        self.preserve_service()
        return {"domain": self.domain, "dns": "verified", "tls": "verified", "certificateSHA256": expected,
                "portalReadiness": "verified", "signInRedirect": "verified", "interactiveLoginTested": False}
