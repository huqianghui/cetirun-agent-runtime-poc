#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Bounded, opt-in checks for one explicitly owned LOCAL Kind control-plane.

Run from the checkout, supplying --cluster-name, --node-container, --context,
--cluster-uid-file, --kubeconfig, --kubectl, --docker-config, --probe-image,
--runtime-class, --runtime-handler, --storage-class, --output and
--allow-local-test-resources. Use digest-pinned Python 3 images.

Only fresh labelled synthetic namespaces are changed. Docker uses the local
Unix socket; node exec only reads runtime/cgroup information. The one hostNetwork
listener binds high ports on the owned Kind node, not the machine's network.
No cloud metadata, protected Agent exec, runtime reconfiguration, force deletion,
or finalizer editing is performed. Cleanup uses recorded UIDs/resourceVersions.

Each probe and its separate cleanup budget are at most 180 seconds; individual
commands are at most 90 seconds. Failed probes do not skip independent checks.
The private observation receipt includes invocation paths and deployment
metadata. It is NOT capability registration, physical fencing, platform
qualification, or permission to start an Agent. Review it before sharing.
"""

import argparse
from contextlib import contextmanager
import copy
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation
import hashlib
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit
import uuid


ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location(
    "local_platform_qualification", ROOT / "scripts/sandbox-platform-qualify.py"
)
qualify = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(qualify)
Error = qualify.QualificationError
PROBE_SECONDS = 180
REQUEST_SECONDS = 90
CAPABILITIES = (
    "hostResourceLimits", "readOnlyRootFilesystem", "guestExternalNetwork",
    "denyPrecedence", "nodeTraffic", "dnsIPRestrictions",
)
BASELINE = tuple(name for name in CAPABILITIES if name != "readOnlyRootFilesystem")
HOST_FIX = {
    "setting": "[runtime] sandbox_cgroup_only = true",
    "configuration": "/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml",
    "requirements": (
        "Administrator must configure appropriate RuntimeClass PodOverhead, then "
        "create a fresh sandbox and recheck actual VMM/thread cgroup ancestors. "
        "VM sizing and guest-only cgroups are not host enforcement."
    ),
    "documentation": "https://github.com/kata-containers/kata-containers/blob/4.2.0/docs/design/host-cgroups.md",
    "systemdImplementation": "https://github.com/kata-containers/kata-containers/blob/4.2.0/src/runtime/pkg/resourcecontrol/cgroups.go",
    "appliedByHelper": False,
}
GUEST_RESOURCES = """
import json,os
from pathlib import Path
print(json.dumps({
    "kernel":os.uname().release,
    "cpuMax":Path("/sys/fs/cgroup/cpu.max").read_text().strip(),
    "memoryMax":Path("/sys/fs/cgroup/memory.max").read_text().strip(),
}))
"""
FILESYSTEM = """
import errno,json,os,secrets
from pathlib import Path
nonce=secrets.token_hex(16)
root=Path("/tmp")/("cetirun-readonly-"+nonce)
result={"rootErrno":None,"homeWritable":False,"scratchWritable":False}
try:
    with root.open("x") as stream:
        stream.write(nonce)
except OSError as error:
    result["rootErrno"]=error.errno
else:
    root.unlink()
for key,folder in (("homeWritable",os.environ["HOME"]),("scratchWritable","/scratch")):
    path=Path(folder)/("cetirun-writable-"+nonce)
    with path.open("x") as stream:
        stream.write(nonce)
        stream.flush()
        os.fsync(stream.fileno())
    result[key]=path.read_text()==nonce
    path.unlink()
result["rootMountReadOnly"]=bool(os.statvfs("/").f_flag & os.ST_RDONLY)
result["expectedEROFS"]=result["rootErrno"]==errno.EROFS
print(json.dumps(result))
"""
DNS_COMMON = """
import ipaddress,secrets,socket,struct
def exact(stream,size):
    data=b""
    while len(data)<size:
        part=stream.recv(size-len(data))
        if not part: raise SystemExit("short synthetic DNS stream")
        data+=part
    return data
"""
DNS_SERVER = DNS_COMMON + """
import socketserver,threading,time
def response(data):
    if len(data)<17 or data[4:6]!=b"\\x00\\x01":
        raise ValueError("invalid synthetic DNS question")
    return (data[:2]+b"\\x81\\x80\\x00\\x01\\x00\\x01\\x00\\x00\\x00\\x00"+
            data[12:]+b"\\xc0\\x0c\\x00\\x01\\x00\\x01\\x00\\x00\\x00\\x01\\x00\\x04"+
            ipaddress.ip_address("192.0.2.123").packed)
class UDP(socketserver.BaseRequestHandler):
    def handle(self):
        data,endpoint=self.request
        endpoint.sendto(response(data),self.client_address)
class TCP(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(2)
        data=exact(self.request,struct.unpack("!H",exact(self.request,2))[0])
        answer=response(data)
        self.request.sendall(struct.pack("!H",len(answer))+answer)
for port in (53,5353):
    for server,handler in ((socketserver.ThreadingUDPServer,UDP),(socketserver.ThreadingTCPServer,TCP)):
        instance=server(("0.0.0.0",port),handler)
        threading.Thread(target=instance.serve_forever,daemon=True).start()
time.sleep(3600)
"""
DNS_PROBE = DNS_COMMON + """
import json,sys
def skip_name(data,pos):
    for _ in range(128):
        length=data[pos]
        if length & 192 == 192: return pos+2
        pos+=1
        if length==0: return pos
        if length>63: raise SystemExit("invalid DNS label")
        pos+=length
    raise SystemExit("invalid DNS name")
def query(address,port,protocol,name,expected):
    transaction=secrets.token_bytes(2)
    question=b"".join(bytes([len(label)])+label.encode() for label in name.split("."))+b"\\x00\\x00\\x01\\x00\\x01"
    request=transaction+b"\\x01\\x00\\x00\\x01\\x00\\x00\\x00\\x00\\x00\\x00"+question
    kind=socket.SOCK_STREAM if protocol=="TCP" else socket.SOCK_DGRAM
    with socket.socket(socket.AF_INET,kind) as endpoint:
        endpoint.settimeout(2)
        try:
            endpoint.connect((address,port))
            if protocol=="TCP":
                endpoint.sendall(struct.pack("!H",len(request))+request)
                data=exact(endpoint,struct.unpack("!H",exact(endpoint,2))[0])
            else:
                if endpoint.send(request)!=len(request): raise SystemExit("short DNS datagram")
                data=endpoint.recv(4096)
        except TimeoutError:
            return False
    if len(data)<12 or data[:2]!=transaction or not data[2]&128 or data[3]&15:
        raise SystemExit("unexpected DNS response")
    questions,answers=struct.unpack("!HH",data[4:8])
    if questions!=1 or data[12:12+len(question)]!=question:
        raise SystemExit("DNS question mismatch")
    pos=skip_name(data,12)+4
    found=[]
    for _ in range(answers):
        pos=skip_name(data,pos)
        kind,cls,ttl,size=struct.unpack("!HHIH",data[pos:pos+10])
        pos+=10
        if kind==1 and cls==1 and size==4:
            found.append(str(ipaddress.ip_address(data[pos:pos+size])))
        pos+=size
    if expected not in found: raise SystemExit("DNS answer mismatch")
    return True
targets=json.loads(sys.argv[1])
print(json.dumps([query(*target) for target in targets]))
"""


def require(condition, message):
    if not condition:
        raise Error(message)


def quantity(raw):
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(m|Ki|Mi|Gi)?", str(raw))
    require(match is not None, "Unsupported resource quantity; no limit inferred")
    try:
        return Decimal(match[1]) * {
            None: Decimal(1), "m": Decimal("0.001"),
            "Ki": Decimal(1024), "Mi": Decimal(1024**2), "Gi": Decimal(1024**3),
        }[match[2]]
    except InvalidOperation as error:
        raise Error("Invalid resource quantity") from error


def booleans(raw, count):
    try:
        values = json.loads(raw)
    except json.JSONDecodeError as error:
        raise Error("Synthetic probe returned invalid JSON") from error
    require(isinstance(values, list) and len(values) == count
            and all(type(value) is bool for value in values), "Invalid probe result shape")
    return values


def local_endpoint(endpoint):
    parsed = urlsplit(endpoint.strip())
    try:
        address = ipaddress.ip_address(parsed.hostname or "")
    except ValueError as error:
        raise Error("Kubernetes endpoint must be an explicit loopback IP") from error
    require(parsed.scheme == "https" and address.is_loopback and parsed.port
            and not parsed.username and not parsed.password, "Not a local HTTPS Kubernetes endpoint")


class LocalKubernetes(qualify.Kubernetes):
    def __init__(self, args):
        require(re.fullmatch(r"cetirun-local-[a-z0-9-]+", args.cluster_name),
                "Use an explicitly task-owned cetirun-local Kind cluster")
        require(args.node_container == args.cluster_name + "-control-plane"
                and args.context == "kind-" + args.cluster_name, "Kind target names do not match")
        try:
            cluster_uid = Path(args.cluster_uid_file).read_text().strip()
            uuid.UUID(cluster_uid)
        except (OSError, ValueError) as error:
            raise Error("Cannot read a valid expected cluster UID") from error
        super().__init__(args.kubeconfig, args.context, cluster_uid)
        self.prefix[0] = str(Path(args.kubectl).resolve())
        self.prefix[-1] = "--request-timeout=90s"
        self.args = args
        self.deadline = time.monotonic() + PROBE_SECONDS
        self.namespaces = {}
        self.pods = {}
        self.volumes = {}
        self.uncertain_creates = []
        self.stage_namespaces = []
        local_endpoint(self.command(self.prefix + [
            "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}",
        ]))
        self.check_target()

    def command(self, command, document=None, timeout=REQUEST_SECONDS):
        remaining = self.deadline - time.monotonic()
        require(remaining > 0, "Probe deadline exceeded")
        try:
            result = subprocess.run(
                command, input=None if document is None else json.dumps(document),
                capture_output=True, text=True, check=False,
                timeout=min(timeout, REQUEST_SECONDS, remaining),
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise Error("Local probe command unavailable or deadline exceeded") from error
        require(result.returncode == 0, "Local probe command failed; diagnostics withheld")
        return result.stdout

    def docker(self, arguments):
        return self.command([
            "docker", "--config", self.args.docker_config,
            "--host", "unix:///var/run/docker.sock", *arguments,
        ])

    def node_read(self, arguments):
        return self.docker(["exec", self.args.node_container, *arguments])

    def check_target(self):
        super().check_target()
        raw = self.docker([
            "inspect", "--type", "container", self.args.node_container,
            "--format", "{{json .Config.Labels}}",
        ])
        labels = json.loads(raw)
        require(labels.get("io.x-k8s.kind.cluster") == self.args.cluster_name
                and labels.get("io.x-k8s.kind.role") == "control-plane",
                "Kind ownership mismatch; no writes allowed")

    def check_namespace(self, namespace):
        require(namespace in self.namespaces, "Namespace was not created by this invocation")
        current = self.get("namespace", namespace)
        uid, run = self.namespaces[namespace]
        require(current["metadata"].get("uid") == uid
                and current["metadata"].get("labels", {}).get(qualify.RUN_LABEL) == run
                and not current["metadata"].get("deletionTimestamp"),
                "Namespace ownership changed; no writes allowed")

    def check_pod(self, namespace, name):
        self.check_namespace(namespace)
        require((namespace, name) in self.pods, "Exec is restricted to this invocation's synthetic Pods")
        uid, runtime = self.pods[namespace, name]
        value = self.get("pod", name, namespace)
        qualify.verify_pod_identity(value, uid, runtime)
        return value

    def run(self, args, document=None, timeout=REQUEST_SECONDS):
        require(args and args[0] in ("get", "create", "delete", "exec"),
                "Unsupported Kubernetes operation")
        identity = None
        if args[0] in ("create", "delete", "exec"):
            self.check_target()
        if args[0] == "exec":
            require(args[1] == "--namespace", "Exec needs an explicit owned namespace")
            identity = (args[2], args[3])
            self.check_pod(*identity)
        if args[0] == "delete":
            require(args[:2] == ["delete", "--raw"] and isinstance(document, dict),
                    "Only conditional raw deletion is allowed")
            conditions = document.get("preconditions", {})
            require(conditions.get("uid") and conditions.get("resourceVersion")
                    and "gracePeriodSeconds" not in document,
                    "Deletion requires UID/resourceVersion without force options")
            parts = args[2].split("/")
            require(parts[:4] == ["", "api", "v1", "namespaces"], "Deletion path is outside owned namespaces")
            namespace = parts[4]
            require(namespace in self.namespaces, "Deletion is outside owned namespaces")
            if len(parts) == 5:
                require(conditions["uid"] == self.namespaces[namespace][0], "Namespace deletion UID mismatch")
            else:
                require(len(parts) == 7 and parts[5] == "pods"
                        and (namespace, parts[6]) in self.pods, "Only recorded Pods may be deleted")
                self.check_namespace(namespace)
                require(conditions["uid"] == self.pods[namespace, parts[6]][0], "Pod deletion UID mismatch")
        raw = self.command(self.prefix + args, document, timeout)
        if identity:
            self.check_pod(*identity)
        return raw

    def create(self, document):
        kind, metadata = document["kind"], document["metadata"]
        if kind == "Namespace":
            require(re.fullmatch(r"cetirun-(?:local-platform|sandbox-probe)-[0-9a-f]{12}", metadata["name"])
                    and re.fullmatch(r"[0-9a-f]{32}", metadata.get("labels", {}).get(qualify.RUN_LABEL, "")),
                    "Namespace must have a fresh synthetic name and run label")
        else:
            require(kind in ("Pod", "PersistentVolumeClaim", "CiliumNetworkPolicy", "NetworkPolicy"),
                    "Only synthetic probe resources may be created")
            self.check_namespace(metadata["namespace"])
        intent = {"kind": kind, "name": metadata["name"], "namespace": metadata.get("namespace")}
        self.uncertain_creates.append(intent)
        value = super().create(document)
        uid = value.get("metadata", {}).get("uid")
        require(isinstance(uid, str) and uid, "Creation UID unknown; resource will not be adopted")
        self.uncertain_creates.remove(intent)
        if kind == "Namespace":
            self.namespaces[metadata["name"]] = (uid, metadata["labels"][qualify.RUN_LABEL])
            self.stage_namespaces.append(metadata["name"])
        if kind == "Pod":
            self.pods[metadata["namespace"], metadata["name"]] = (uid, document["spec"].get("runtimeClassName"))
        return value

    def namespace(self):
        run = uuid.uuid4().hex
        name = "cetirun-local-platform-" + run[:12]
        self.create({"apiVersion": "v1", "kind": "Namespace",
                     "metadata": {"name": name, "labels": {qualify.RUN_LABEL: run}}})
        return name, run

    def exec(self, namespace, name, script, *arguments):
        return self.run(["exec", "--namespace", namespace, name, "--",
                         "python3", "-c", script, *arguments])


def create_pod(kube, namespace, run, name="probe", claim=None, document=None):
    value = document or qualify.pod(
        namespace, run, "probe", kube.args.probe_image, kube.args.runtime_class
    )
    value["metadata"]["name"] = name
    spec = value["spec"]
    spec["terminationGracePeriodSeconds"] = 5
    if document is None:
        spec["securityContext"]["fsGroup"] = 65532
        spec["volumes"] = [
            {"name": "home", **({"persistentVolumeClaim": {"claimName": claim}} if claim
                               else {"emptyDir": {"sizeLimit": "64Mi"}})},
            {"name": "scratch", "emptyDir": {"sizeLimit": "64Mi"}},
        ]
        spec["containers"][0]["env"] = [{"name": "HOME", "value": "/home/probe"}]
        spec["containers"][0]["volumeMounts"] = [
            {"name": "home", "mountPath": "/home/probe"}, {"name": "scratch", "mountPath": "/scratch"},
        ]
    return kube.create(value)


def ready(kube, namespace, name):
    uid, runtime = kube.pods[namespace, name]
    return qualify.wait_ready(kube, namespace, name, uid, runtime_class=runtime)


def record(check, observed, evidence, failure=None):
    check.update({"observed": observed is True, "status": "passed" if observed is True else "failed",
                  "evidence": evidence})
    if observed is not True:
        check["failure"] = failure or "Required positive/negative controls did not all pass"


def cgroup_bounds(groups, pod_uid, cpu, memory):
    evidence = []
    passed = bool(groups)
    for group in groups:
        quotas, memories = [], []
        chain = group["ancestors"]
        for ancestor in chain:
            if pod_uid not in ancestor["path"] and pod_uid.replace("-", "_") not in ancestor["path"]:
                continue
            maximum, period = ancestor["cpuMax"].split()
            if maximum != "max":
                require(int(period) > 0 and int(maximum) > 0, "Invalid host CPU bound")
                quotas.append(Decimal(maximum) / Decimal(period))
            if ancestor["memoryMax"] != "max":
                require(int(ancestor["memoryMax"]) > 0, "Invalid host memory bound")
                memories.append(int(ancestor["memoryMax"]))
        bounded = bool(quotas and memories) and min(quotas) <= cpu and min(memories) <= memory
        passed = passed and bounded
        evidence.append({**group, "withinSelectedPodPlusOverhead": bool(bounded)})
    return bool(passed), evidence


def host_resources(kube, namespace):
    pod = kube.check_pod(namespace, "probe")
    uid = pod["metadata"]["uid"]
    items = json.loads(kube.node_read(["crictl", "pods", "--namespace", namespace, "-o", "json"]))["items"]
    matching = [item for item in items if item["metadata"].get("uid") == uid
                and item.get("runtimeHandler") == kube.args.runtime_handler]
    require(len(matching) == 1, "Cannot uniquely bind the Pod UID to its Kata sandbox")
    sandbox = matching[0]["id"]
    require(re.fullmatch(r"[0-9a-f]{64}", sandbox), "Invalid sandbox identity")
    info = json.loads(kube.node_read(["crictl", "inspectp", sandbox]))["info"]
    pid = info.get("pid")
    require(type(pid) is int and pid > 1, "Missing VMM PID")
    identity = kube.node_read(["cat", f"/proc/{pid}/stat"])
    command = kube.node_read(["cat", f"/proc/{pid}/cmdline"]).split("\0")
    require(command and "qemu-system" in command[0]
            and any(arg.split(",")[0] == "sandbox-" + sandbox for arg in command), "PID is not the selected QEMU VMM")
    paths = kube.node_read([
        "sh", "-c", 'for f in /proc/"$1"/task/*/cgroup; do cat "$f" || exit; done', "probe", str(pid),
    ])
    groups = []
    for path in sorted({line.split(":", 2)[2] for line in paths.splitlines() if line.startswith("0::")}):
        require(path.startswith("/") and ".." not in path.split("/"), "Invalid VMM cgroup path")
        ancestors = []
        current = path
        while True:
            raw = kube.node_read(["sh", "-c",
                                 'cat "/sys/fs/cgroup$1/cpu.max" "/sys/fs/cgroup$1/memory.max"',
                                 "probe", current]).splitlines()
            require(len(raw) == 2, "Incomplete host cgroup controls")
            ancestors.append({"path": current, "cpuMax": raw[0], "memoryMax": raw[1]})
            if current == "/":
                break
            current = str(Path(current).parent)
        groups.append({"ancestors": ancestors})
    require(kube.node_read(["cat", f"/proc/{pid}/stat"]).split(") ", 1)[1].split()[19]
            == identity.split(") ", 1)[1].split()[19], "VMM PID identity changed")
    limits = pod["spec"]["containers"][0]["resources"]["limits"]
    overhead = pod["spec"].get("overhead", {})
    cpu = quantity(limits["cpu"]) + quantity(overhead.get("cpu", "0"))
    memory = quantity(limits["memory"]) + quantity(overhead.get("memory", "0"))
    observed, groups = cgroup_bounds(groups, uid, cpu, memory)
    config = info.get("runtimeOptions", {}).get("config_path", "")
    require(config.startswith("/opt/kata/") and ".." not in config.split("/")
            and re.fullmatch(r"[/a-zA-Z0-9_.-]+", config), "Unknown Kata configuration path")
    settings = kube.node_read([
        "grep", "-E", "^(sandbox_cgroup_only|static_sandbox_resource_mgmt|default_vcpus|default_memory) =", config,
    ]).splitlines()
    guest = json.loads(kube.exec(namespace, "probe", GUEST_RESOURCES))
    kube.check_pod(namespace, "probe")
    for group in groups:
        for ancestor in group["ancestors"]:
            ancestor["path"] = ancestor["path"].replace(uid, "<pod-uid>").replace(
                uid.replace("-", "_"), "<pod-uid>").replace(sandbox, "<sandbox-id>")
    evidence = {
        "selectedLimits": limits, "runtimeOverhead": overhead,
        "expectedMaximumCPU": str(cpu), "expectedMaximumMemoryBytes": str(memory),
        "vmmBoundToExactPodUID": True, "allVMMThreadCgroups": groups,
        "guest": guest, "kataSettings": settings,
        "threshold": "Every VMM thread needs finite Pod-scoped host CPU and memory bounds <= selected limits plus declared overhead",
    }
    if not observed:
        evidence["requiredAdministratorChange"] = HOST_FIX
    return observed, evidence


def resource_probe(kube, results):
    namespace, run = kube.namespace()
    create_pod(kube, namespace, run)
    ready(kube, namespace, "probe")
    try:
        observed, evidence = host_resources(kube, namespace)
        record(results["hostResourceLimits"], observed, evidence, "VMM host bounds do not satisfy selected Pod limits")
    except (Error, KeyError, ValueError) as error:
        results["hostResourceLimits"].update({"status": "failed", "failure": sanitized_error(error)})
    data = json.loads(kube.exec(namespace, "probe", FILESYSTEM))
    record(results["readOnlyRootFilesystem"],
           all(data.get(key) is True for key in (
               "expectedEROFS", "rootMountReadOnly", "homeWritable", "scratchWritable")),
           data, "EROFS and writable HOME/scratch controls did not all pass")


def remap(script, base):
    return re.sub(r"\b808[012]\b", lambda match: str(base + int(match[0]) - 8080), script)


def network_sample(kube, namespace, pod_name, address, protocol, base=8080):
    ipaddress.ip_address(address)
    script = remap(qualify.network_scripts(protocol)[2], base)
    return booleans(kube.exec(namespace, pod_name, script, address), 3)


def wait_sample(sample, expected, seconds=45):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        values = sample()
        if values == expected:
            return values
        time.sleep(1)
    raise Error("Positive/negative network controls did not converge")


def policy(kube, namespace, run, name, egress, deny):
    kube.create({
        "apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {"endpointSelector": {"matchLabels": {qualify.RUN_LABEL: run, "role": "probe"}},
                 "egress": egress, "egressDeny": deny},
    })


def ports(numbers):
    return [{"ports": [{"port": str(number), "protocol": protocol}
                       for number in numbers for protocol in ("TCP", "UDP")]}]


def overlapping_allow(kube, namespace, run):
    kube.create({
        "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
        "metadata": {"name": "overlapping-allow", "namespace": namespace},
        "spec": {"podSelector": {"matchLabels": {qualify.RUN_LABEL: run, "role": "probe"}},
                 "policyTypes": ["Egress"], "egress": [{}]},
    })


def node_probe(kube, check):
    namespace, run = kube.namespace()
    base = 20000 + secrets.randbelow(8000)
    server = qualify.pod(namespace, run, "server", kube.args.probe_image, kube.args.runtime_class)
    server["spec"].update({"hostNetwork": True, "nodeName": kube.args.node_container})
    tcp = server["spec"]["containers"][0]
    tcp["name"] = "tcp"
    tcp["command"][-1] = remap(qualify.SERVER, base)
    tcp["readinessProbe"]["exec"]["command"][-1] = remap(qualify.LISTENER_READINESS, base)
    udp = copy.deepcopy(tcp)
    udp["name"] = "udp"
    udp["command"][-1] = remap(qualify.UDP_SERVER, base)
    udp["readinessProbe"]["exec"]["command"][-1] = remap(qualify.UDP_LISTENER_READINESS, base)
    server["spec"]["containers"].append(udp)
    create_pod(kube, namespace, run, "node-listener", document=server)
    create_pod(kube, namespace, run)
    address = ready(kube, namespace, "node-listener")["status"]["podIP"]
    node = kube.get("node", kube.args.node_container)
    require(address in [entry["address"] for entry in node["status"]["addresses"]
                        if entry["type"] == "InternalIP"], "Listener is not on the approved node IP")
    ready(kube, namespace, "probe")
    evidence = {"protocols": ["TCP", "UDP"], "nodeAddressMatched": True, "ports": [base, base + 1, base + 2]}
    check["evidence"] = evidence

    def sample(pod_name="probe"):
        return {protocol: network_sample(kube, namespace, pod_name, address, protocol, base)
                for protocol in ("TCP", "UDP")}

    all_allowed = {protocol: [True, True, True] for protocol in ("TCP", "UDP")}
    evidence["baseline"] = wait_sample(sample, all_allowed)
    policy(kube, namespace, run, "node-ports",
           [{"toEntities": ["host"], "toPorts": ports([base + 1])}],
           [{"toEntities": ["host", "remote-node"], "toPorts": ports([base])}])
    evidence["restricted"] = wait_sample(sample, {p: [False, True, False] for p in all_allowed})
    overlapping_allow(kube, namespace, run)
    evidence["overlappingAllow"] = wait_sample(sample, {p: [False, True, True] for p in all_allowed})
    policy(kube, namespace, run, "deny-node", [], [{"toEntities": ["host", "remote-node"]}])
    evidence["nodeDeny"] = wait_sample(sample, {p: [False, False, False] for p in all_allowed})
    evidence["listenersStillReachable"] = wait_sample(lambda: sample("node-listener"), all_allowed)
    for name in ("probe", "node-listener"):
        kube.check_pod(namespace, name)
    evidence["scope"] = "Owned node IPv4 egress only; no second node, ingress, or metadata endpoints tested"
    record(check, True, evidence)


def dns_probe(kube, check):
    namespace, run = kube.namespace()
    addresses = {}
    for name in ("approved-dns", "unapproved-dns"):
        value = qualify.pod(namespace, run, "server", kube.args.probe_image, kube.args.runtime_class)
        value["metadata"]["labels"]["role"] = name
        container = value["spec"]["containers"][0]
        container["command"] = ["python3", "-c", DNS_SERVER]
        container["securityContext"]["capabilities"]["add"] = ["NET_BIND_SERVICE"]
        readiness = DNS_PROBE.replace(
            "print(json.dumps([query(*target) for target in targets]))",
            "raise SystemExit(0 if all(query(*target) for target in targets) else 1)",
        )
        container["readinessProbe"] = {
            "exec": {"command": ["python3", "-c", readiness, json.dumps([
                ["127.0.0.1", port, protocol, "ready.example.test", "192.0.2.123"]
                for port in (53, 5353) for protocol in ("TCP", "UDP")
            ])]},
            "periodSeconds": 1, "timeoutSeconds": 10, "failureThreshold": 3,
        }
        create_pod(kube, namespace, run, name, document=value)
    create_pod(kube, namespace, run)
    for name in ("approved-dns", "unapproved-dns", "probe"):
        addresses[name] = ready(kube, namespace, name)["status"]["podIP"]
    dns = kube.get("service", "kube-dns", "kube-system")
    kubernetes = kube.get("service", "kubernetes", "default")
    resolver = dns["spec"]["clusterIP"]
    service_address = kubernetes["spec"]["clusterIP"]
    for address in [*addresses.values(), resolver, service_address]:
        require(ipaddress.ip_address(address).version == 4, "This local DNS probe supports IPv4 only")
    targets = []
    for address, port, name, answer in (
        (addresses["approved-dns"], 53, "approved.example.test", "192.0.2.123"),
        (addresses["approved-dns"], 5353, "wrong-port.example.test", "192.0.2.123"),
        (addresses["unapproved-dns"], 53, "unapproved.example.test", "192.0.2.123"),
        (resolver, 53, "kubernetes.default.svc.cluster.local", service_address),
    ):
        targets.extend([[address, port, protocol, name, answer] for protocol in ("TCP", "UDP")])
    evidence = {"order": ["approvedTCP", "approvedUDP", "wrongPortTCP", "wrongPortUDP",
                           "unapprovedIPTCP", "unapprovedIPUDP", "clusterDNSTCP", "clusterDNSUDP"]}
    check["evidence"] = evidence

    def sample(name="probe"):
        return booleans(kube.exec(namespace, name, DNS_PROBE, json.dumps(targets)), 8)

    evidence["baseline"] = wait_sample(sample, [True] * 8)
    approved = {"matchLabels": {qualify.RUN_LABEL: run, "role": "approved-dns"}}
    unapproved = {"matchLabels": {qualify.RUN_LABEL: run, "role": "unapproved-dns"}}
    core_dns = {"matchLabels": {"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}
    policy(kube, namespace, run, "dns-restrictions",
           [{"toEndpoints": [approved, core_dns], "toPorts": ports([53])}],
           [{"toEndpoints": [unapproved], "toPorts": ports([53])},
            {"toEndpoints": [approved], "toPorts": ports([5353])}])
    expected = [True, True, False, False, False, False, True, True]
    evidence["restricted"] = wait_sample(sample, expected)
    overlapping_allow(kube, namespace, run)
    evidence["overlappingAllow"] = wait_sample(sample, expected)
    evidence["listenersStillReachable"] = wait_sample(lambda: sample("approved-dns"), [True] * 8)
    for name in ("probe", "approved-dns", "unapproved-dns"):
        kube.check_pod(namespace, name)
    require(kube.get("service", "kube-dns", "kube-system")["metadata"]["uid"] == dns["metadata"]["uid"],
            "Cluster DNS Service identity changed")
    evidence["scope"] = "IPv4 DNS IP/port controls through exact live synthetic peers and Cilium endpoint selectors; local CoreDNS TCP+UDP allowed"
    record(check, True, evidence)


def delete_pod(kube, namespace, name):
    value = kube.check_pod(namespace, name)
    metadata = value["metadata"]
    kube.run(["delete", "--raw", f"/api/v1/namespaces/{namespace}/pods/{name}", "-f", "-"], {
        "apiVersion": "v1", "kind": "DeleteOptions",
        "preconditions": {"uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"]},
    })
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        current = kube.get("pod", name, namespace, absent_ok=True)
        if current is None:
            return
        require(current["metadata"]["uid"] == metadata["uid"], "Deleted Pod was replaced; replacement retained")
        time.sleep(1)
    raise Error("Ordinary Pod deletion timed out; no force deletion attempted")


def storage_probe(kube, check):
    namespace, run = kube.namespace()
    storage = kube.get("storageclass", kube.args.storage_class)
    require(storage.get("provisioner") == "hostpath.csi.k8s.io"
            and storage.get("reclaimPolicy") == "Delete", "Probe requires task-local CSI hostpath Delete storage")
    claim = kube.create({
        "apiVersion": "v1", "kind": "PersistentVolumeClaim",
        "metadata": {"name": "home", "namespace": namespace, "labels": {qualify.RUN_LABEL: run}},
        "spec": {"accessModes": ["ReadWriteOncePod"], "storageClassName": kube.args.storage_class,
                 "resources": {"requests": {"storage": "1Gi"}}},
    })
    create_pod(kube, namespace, run, "writer", claim="home")
    ready(kube, namespace, "writer")
    bound = kube.get("pvc", "home", namespace)
    require(bound["metadata"]["uid"] == claim["metadata"]["uid"]
            and bound["status"].get("phase") == "Bound"
            and bound["spec"].get("accessModes") == ["ReadWriteOncePod"], "Fresh RWOP claim binding changed")
    volume = kube.get("pv", bound["spec"]["volumeName"])
    require(volume["spec"]["csi"]["driver"] == "hostpath.csi.k8s.io"
            and volume["spec"]["claimRef"]["uid"] == claim["metadata"]["uid"], "CSI volume ownership mismatch")
    kube.volumes[namespace] = (volume["metadata"]["name"], volume["metadata"]["uid"])
    nonce = secrets.token_hex(32)
    kube.exec(namespace, "writer",
              "import os,sys;f=open('/home/probe/persistence','x');f.write(sys.argv[1]);f.flush();os.fsync(f.fileno());f.close()",
              nonce)
    create_pod(kube, namespace, run, "contender", claim="home")
    deadline = time.monotonic() + 45
    pending = False
    while time.monotonic() < deadline:
        value = kube.check_pod(namespace, "contender")
        require(value["status"].get("phase") == "Pending"
                and not any(c.get("state", {}).get("running")
                            for c in value["status"].get("containerStatuses", [])),
                "Second RWOP Pod unexpectedly started")
        pending = any(condition.get("type") == "PodScheduled" and condition.get("status") == "False"
                      and condition.get("reason") == "Unschedulable"
                      and "ReadWriteOncePod" in condition.get("message", "")
                      for condition in value["status"].get("conditions", []))
        if pending:
            break
        time.sleep(1)
    require(pending, "No explicit RWOP scheduler contention evidence within deadline")
    ready(kube, namespace, "writer")
    read_marker = "import json,sys;from pathlib import Path;print(json.dumps(Path('/home/probe/persistence').read_text()==sys.argv[1]))"
    require(json.loads(kube.exec(namespace, "writer", read_marker, nonce)) is True, "Writer marker changed")
    evidence = {"freshCSIClaim": True, "accessMode": "ReadWriteOncePod",
                "ordinarySecondPodContention": True, "firstWriterStillReady": True,
                "reopenPersistence": False, "physicalFencing": False}
    check["evidence"] = evidence
    delete_pod(kube, namespace, "writer")
    ready(kube, namespace, "contender")
    require(json.loads(kube.exec(namespace, "contender", read_marker, nonce)) is True, "Reopened HOME marker mismatch")
    current = kube.get("pvc", "home", namespace)
    require(current["metadata"]["uid"] == claim["metadata"]["uid"]
            and current["spec"].get("volumeName") == volume["metadata"]["name"], "Reopened claim identity changed")
    evidence["reopenPersistence"] = True
    evidence["scope"] = "Ordinary scheduler contention and reopen after graceful deletion, NOT stale-writer/physical fencing"
    record(check, True, evidence)


def sanitized_error(error):
    if isinstance(error, Error):
        return str(error)
    return "Invalid or incomplete local probe data (" + type(error).__name__ + ")"


@contextmanager
def stage(kube, receipt, name, checks):
    kube.deadline = time.monotonic() + PROBE_SECONDS
    kube.stage_namespaces = []
    entry = {"status": "running", "cleanupComplete": False}
    receipt["probes"][name] = entry
    try:
        yield entry
        entry["status"] = "completed"
    except (Error, KeyError, ValueError, TypeError) as error:
        entry.update({"status": "failed", "failure": sanitized_error(error)})
        for check in checks:
            if check["status"] == "not-tested":
                check.update({"status": "failed", "failure": entry["failure"]})
    finally:
        kube.deadline = time.monotonic() + PROBE_SECONDS
        try:
            for namespace in kube.stage_namespaces:
                uid, run = kube.namespaces[namespace]
                qualify.cleanup(kube, namespace, uid, run, timeout=90)
                if namespace in kube.volumes:
                    volume_name, volume_uid = kube.volumes[namespace]
                    deadline = time.monotonic() + 60
                    while time.monotonic() < deadline:
                        current = kube.get("pv", volume_name, absent_ok=True)
                        if current is None:
                            break
                        require(current["metadata"]["uid"] == volume_uid, "PV replaced; replacement retained")
                        time.sleep(1)
                    else:
                        raise Error("Owned CSI PV reclaim not confirmed; no finalizers modified")
            entry["cleanupComplete"] = True
        except (Error, KeyError, ValueError) as error:
            entry["cleanupFailure"] = sanitized_error(error)
            for check in checks:
                check["observed"] = False
                check["status"] = "failed"
                check["failure"] = "Cleanup not confirmed; no capability accepted"
        entry["namespaces"] = list(kube.stage_namespaces)
        if kube.uncertain_creates:
            entry["unknownCreationOutcomes"] = copy.deepcopy(kube.uncertain_creates)
            entry["cleanupComplete"] = False


def save_receipt(path, receipt):
    receipt["capabilities"] = {name: receipt["checks"][name]["observed"] is True for name in CAPABILITIES}
    receipt["baselineRequiredSatisfied"] = all(receipt["capabilities"][name] for name in BASELINE)
    receipt["updatedAt"] = datetime.now(timezone.utc).isoformat()
    require(path.parent.is_dir() and not path.is_symlink(), "Receipt directory missing or output is a symlink")
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, prefix=".platform-probe-", delete=False) as stream:
            temporary = Path(stream.name)
            json.dump(receipt, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if temporary is not None and temporary.exists():
            temporary.unlink()


def receipt_path(raw):
    requested = Path(raw)
    require(not requested.is_symlink(), "Receipt output must not be a symlink")
    output = requested.resolve()
    require(output != ROOT and ROOT not in output.parents, "Receipt must stay outside the source checkout")
    require(output.parent.is_dir() and not output.is_dir(), "Receipt output directory is unavailable")
    return output


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("cluster-name", "node-container", "context", "cluster-uid-file", "kubeconfig",
                 "kubectl", "docker-config", "probe-image", "runtime-class", "runtime-handler",
                 "storage-class", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--allow-local-test-resources", action="store_true")
    args = parser.parse_args(argv)
    receipt = {
        "schemaVersion": "local-platform-probes/v1", "profile": "native-single-start",
        "qualified": False, "qualifiesExecution": False,
        "enforcementObservation": False, "restrictiveFailure": False,
        "authenticatedIngress": {"status": "not-tested", "owner": "separate owner-authentication validation"},
        "requestTimeoutSeconds": REQUEST_SECONDS, "probeTimeoutSeconds": PROBE_SECONDS,
        "cleanupTimeoutSeconds": PROBE_SECONDS,
        "invocation": shlex.join(["python3", "-B", "examples/native-sandbox/local/platform-probe.py",
                                 *(sys.argv[1:] if argv is None else argv)]),
        "source": {"helperSHA256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                   "networkHelperSHA256": hashlib.sha256((ROOT / "scripts/sandbox-platform-qualify.py").read_bytes()).hexdigest()},
        "checks": {name: {"observed": False, "status": "not-tested"}
                   for name in (*CAPABILITIES, "rwopAndPersistence")},
        "probes": {},
        "limits": ["IPv4 only", "No Agent namespace access", "No cloud or metadata requests",
                   "No physical fencing", "No authenticated ingress evidence collected",
                   "No admission/compiler/operator integration or full qualification implied"],
    }
    try:
        output = receipt_path(args.output)
    except (Error, OSError) as error:
        print(json.dumps({"status": "blocked", "qualified": False, "failure": sanitized_error(error)}))
        return 2
    try:
        require(args.allow_local_test_resources, "Explicit local test-resource permission is required")
        require(re.fullmatch(qualify.PINNED_IMAGE, args.probe_image), "A digest-pinned Python image is required")
        kube = LocalKubernetes(args)
        receipt["target"] = {"loopbackAPI": True, "clusterUIDMatched": True, "kindOwnershipMatched": True}
        receipt["preflight"] = qualify.preflight(kube, args.runtime_class, args.runtime_handler, args.storage_class)
        receipt["runtime"] = {"class": args.runtime_class, "handler": args.runtime_handler, "image": args.probe_image}
        save_receipt(output, receipt)
        checks = receipt["checks"]
        with stage(kube, receipt, "resourcesAndFilesystem",
                   [checks["hostResourceLimits"], checks["readOnlyRootFilesystem"]]):
            resource_probe(kube, checks)
        save_receipt(output, receipt)
        for protocol in ("TCP", "UDP"):
            with stage(kube, receipt, "network" + protocol, []) as result:
                qualify.network_smoke(kube, args.probe_image, args.runtime_class, result, protocol)
            save_receipt(output, receipt)
        network = [receipt["probes"]["network" + protocol] for protocol in ("TCP", "UDP")]
        expected = ("baselineAllPorts", "destinationPortDeny", "witnessInitiallyBlocked",
                    "overlappingAllowObserved", "denyOverOverlappingAllow",
                    "unchangedServerIdentity", "unchangedProbeIdentity")
        network_passed = all(result["status"] == "completed" and result["cleanupComplete"]
                             and all(result.get("networkChecks", {}).get(key) is True for key in expected)
                             for result in network)
        for name in ("guestExternalNetwork", "denyPrecedence"):
            record(checks[name], network_passed, {"sourceProbes": ["networkTCP", "networkUDP"],
                                                 "requiredControls": list(expected)})
        for name, function in (("nodeTraffic", node_probe), ("dnsIPRestrictions", dns_probe),
                               ("rwopAndPersistence", storage_probe)):
            with stage(kube, receipt, name, [checks[name]]):
                function(kube, checks[name])
            save_receipt(output, receipt)
        receipt["status"] = "completed" if all(c["observed"] for c in checks.values()) else "partial"
    except (Error, OSError, KeyError, ValueError, TypeError) as error:
        receipt.update({"status": "blocked", "failure": sanitized_error(error)})
    save_receipt(output, receipt)
    print(json.dumps({"status": receipt["status"], "capabilities": receipt["capabilities"],
                      "baselineRequiredSatisfied": receipt["baselineRequiredSatisfied"],
                      "qualified": False}, sort_keys=True))
    return 0 if receipt["status"] == "completed" else 1


if __name__ == "__main__":
    sys.exit(main())
