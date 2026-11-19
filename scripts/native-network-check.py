#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Compare native Agent network behavior without executing in protected Pods."""

import argparse
import hashlib
import http.client
import importlib.util
import inspect
import ipaddress
import json
import math
import multiprocessing
import os
from pathlib import Path
import re
import shlex
import signal
import socket
import sys
import time
import uuid


PHASES = ("control-before", "cidr", "ports", "control-after")
PORTS = (("TCP", 18080), ("TCP", 18082), ("UDP", 18081), ("UDP", 18083))
HEX = r"[0-9a-f]{32}"
MAX_BYTES = 1024 * 1024
MAX_AGE = 1800
PROBE_TIMEOUT = 60
RESULT_PREFIX = "CETIRUN_NETWORK_RESULT="
PROTECTED = "cetirun.microsoft.com/sandbox-protected"
AGENT_UID = "sandbox.cetirun.microsoft.com/agent-uid"


class CheckError(Exception):
    pass


def require(condition, reason):
    if not condition:
        raise CheckError(reason)


def document(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "duplicate-json-field")
            result[key] = value
        return result

    require(isinstance(raw, str) and 0 < len(raw.encode("utf-8")) <= MAX_BYTES, "invalid-document-size")
    try:
        result = json.loads(raw, object_pairs_hook=pairs,
                            parse_constant=lambda _: require(False, "nonfinite-json-number"))
    except (ValueError, RecursionError) as error:
        raise CheckError("invalid-json") from error
    require(isinstance(result, dict), "expected-json-object")
    return result


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def checker_digest():
    return fingerprint({
        path.name: hashlib.sha256(path.read_bytes()).hexdigest()
        for path in (Path(__file__), Path(__file__).with_name("sandbox-platform-qualify.py"))
    })


def remaining_timeout(deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError
    return remaining


class WitnessSocket(socket.socket):
    def __init__(self, address, deadline):
        family = socket.AF_INET6 if ipaddress.ip_address(address).version == 6 else socket.AF_INET
        super().__init__(family, socket.SOCK_STREAM)
        self.deadline = deadline

    def connect(self, address):
        self.settimeout(remaining_timeout(self.deadline))
        return super().connect(address)

    def sendall(self, data, flags=0):
        self.settimeout(remaining_timeout(self.deadline))
        return super().sendall(data, flags)

    def recv_into(self, buffer, nbytes=0, flags=0):
        # HTTPResponse's buffered makefile reads status, headers and bodies here.
        self.settimeout(remaining_timeout(self.deadline))
        return super().recv_into(buffer, nbytes, flags)


class WitnessConnection(http.client.HTTPConnection):
    def __init__(self, address, port, timeout):
        super().__init__(address, port, timeout=timeout)
        self.deadline = time.monotonic() + timeout

    def connect(self):
        # Witness targets are literal IPs; no DNS resolver can outlive the budget.
        self.sock = WitnessSocket(self.host, self.deadline)
        self.sock.connect((self.host, self.port))


def request_json(address, path, port=18082, timeout=5):
    require(type(timeout) in (int, float) and math.isfinite(timeout) and timeout > 0, "invalid-witness-timeout")
    connection = WitnessConnection(address, port, timeout)
    response = None
    try:
        connection.request("GET", path, headers={"Connection": "close"})
        response = connection.getresponse()
        require(response.status == 200, "witness-http-status")
        raw = response.read(MAX_BYTES + 1)
        require(len(raw) <= MAX_BYTES, "witness-response-too-large")
        result = document(raw.decode("utf-8"))
        remaining_timeout(connection.deadline)
        return result
    except TimeoutError as error:
        raise CheckError("witness-deadline-exceeded") from error
    except (OSError, http.client.HTTPException, UnicodeError) as error:
        raise CheckError("witness-request-failed") from error
    finally:
        try:
            if response is not None:
                response.close()
        finally:
            connection.close()


def receipt(value, run, nonce, protocol, port):
    require(isinstance(value, dict) and
            set(value) == {"run", "nonce", "protocol", "port", "instance", "sequence"},
            "invalid-witness-receipt")
    require(value["run"] == run and value["nonce"] == nonce and value["protocol"] == protocol and
            type(value["port"]) is int and value["port"] == port and
            isinstance(value["instance"], str) and re.fullmatch(HEX, value["instance"]) and
            type(value["sequence"]) is int and value["sequence"] > 0, "witness-receipt-mismatch")
    return value


def exchange(address, protocol, port, run, nonce, timeout=3):
    if protocol == "TCP":
        connection = http.client.HTTPConnection(address, port, timeout=timeout)
        try:
            try:
                connection.connect()
            except TimeoutError:
                return {"outcome": "connect-timeout"}
            connection.request("GET", "/witness/" + run + "/" + nonce)
            response = connection.getresponse()
            require(response.status == 200, "witness-http-status")
            raw = response.read(4097)
            require(len(raw) <= 4096, "witness-response-too-large")
            observed = document(raw.decode("utf-8"))
        except (OSError, http.client.HTTPException, UnicodeError) as error:
            raise CheckError("tcp-transport-or-service-failure") from error
        finally:
            connection.close()
    else:
        require(protocol == "UDP", "invalid-protocol")
        family = socket.AF_INET6 if ipaddress.ip_address(address).version == 6 else socket.AF_INET
        try:
            with socket.socket(family, socket.SOCK_DGRAM) as endpoint:
                endpoint.settimeout(timeout)
                endpoint.connect((address, port))
                payload = (run + ":" + nonce).encode()
                require(endpoint.send(payload) == len(payload), "incomplete-datagram")
                try:
                    raw = endpoint.recv(4097)
                except TimeoutError:
                    return {"outcome": "receive-timeout"}
                require(len(raw) <= 4096, "witness-response-too-large")
                observed = document(raw.decode("utf-8"))
        except (OSError, UnicodeError) as error:
            raise CheckError("udp-transport-or-service-failure") from error
    return {"outcome": "allowed", "witness": receipt(observed, run, nonce, protocol, port)}


def phase_role(phase):
    return "control" if phase.startswith("control-") else phase


def run_probe(payload):
    require(socket.gethostname() == payload["hostname"], "probe-hostname-mismatch")
    markers = []
    checks = []
    for position in ("before", "after"):
        marker = exchange(payload["targets"]["outside"], "TCP", 18082,
                          payload["run"], payload["markers"][position])
        require(marker["outcome"] == "allowed", "positive-marker-unavailable")
        markers.append(marker["witness"])
        if position == "after":
            break
        try:
            require(bool(socket.getaddrinfo(payload["dnsName"], None)), "dns-resolution-failed")
        except socket.gaierror as error:
            raise CheckError("dns-resolution-failed") from error
        for case in payload["cases"]:
            observed = exchange(payload["targets"][case["target"]], case["protocol"], case["port"],
                                payload["run"], case["nonce"])
            checks.append({**case, **observed})
    return {"status": "observed", "run": payload["run"], "phase": payload["phase"],
            "planDigest": payload["planDigest"], "agentUID": payload["agentUID"],
            "podUID": payload["podUID"], "hostname": payload["hostname"],
            "dnsResolved": True, "markers": markers, "checks": checks}


def probe_worker(receiver, sender, payload):
    receiver.close()
    with sender, open(os.devnull, "wb") as output:
        os.dup2(output.fileno(), 1)
        os.dup2(output.fileno(), 2)
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        raw = json.dumps(run_probe(payload), separators=(",", ":")).encode("utf-8")
        require(len(raw) <= MAX_BYTES, "probe-result-too-large")
        sender.sendall(raw)


def supervised_probe(payload):
    require("fork" in multiprocessing.get_all_start_methods(), "probe-supervision-unavailable")
    expires = time.monotonic() + PROBE_TIMEOUT
    receiver, sender = socket.socketpair()
    worker = multiprocessing.get_context("fork").Process(target=probe_worker, args=(receiver, sender, payload))
    cancelled = None

    def cancel(signum, _frame):
        nonlocal cancelled
        # Finish process creation and reap the child before reporting cancellation.
        if cancelled is None:
            cancelled = signum

    previous = {sig: signal.signal(sig, cancel) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        worker.start()
        sender.close()
        data = bytearray()
        # Drain bounded output while the child runs; a full buffer must not prevent its exit.
        while cancelled is None:
            receiver.settimeout(min(remaining_timeout(expires), 0.05))
            try:
                chunk = receiver.recv(min(65536, MAX_BYTES + 1 - len(data)))
            except TimeoutError:
                continue
            if not chunk:
                break
            data.extend(chunk)
            require(len(data) <= MAX_BYTES, "probe-result-too-large")
        while worker.is_alive() and cancelled is None:
            worker.join(timeout=min(remaining_timeout(expires), 0.05))
        if cancelled is None:
            remaining_timeout(expires)
            require(worker.exitcode == 0, "probe-worker-failed")
            result = document(data.decode("utf-8"))
            require(result.get("status") == "observed", "probe-worker-result-invalid")
            remaining_timeout(expires)
    except TimeoutError as error:
        raise CheckError("probe-deadline-exceeded") from error
    finally:
        try:
            if worker.pid is not None:
                if worker.is_alive():
                    worker.kill()
                worker.join(timeout=1)
                require(not worker.is_alive(), "probe-cleanup-unconfirmed")
            worker.close()
        finally:
            receiver.close()
            sender.close()
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    if cancelled is not None:
        raise SystemExit(128 + cancelled)
    return result


def validate_targets(targets, dns_name):
    require(isinstance(targets, dict) and set(targets) == {"restricted", "outside"} and
            all(isinstance(value, str) for value in targets.values()) and len(set(targets.values())) == 2,
            "distinct-witnesses-required")
    for address in targets.values():
        try:
            ip = ipaddress.ip_address(address)
        except ValueError as error:
            raise CheckError("literal-witness-ip-required") from error
        require(str(ip) == address and "%" not in address and
                not (ip.is_multicast or ip.is_unspecified or ip.is_link_local),
                "invalid-witness-address")
    require(isinstance(dns_name, str) and len(dns_name) <= 253 and
            all(re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label)
                for label in dns_name.split(".")), "invalid-dns-name")


def validate_bindings(bindings):
    require(isinstance(bindings, dict) and set(bindings) == {"control", "cidr", "ports"},
            "three-agent-bindings-required")
    for value in bindings.values():
        require(isinstance(value, dict) and all(
            isinstance(value.get(key), str) and 0 < len(value[key]) <= 253 and value[key].strip() == value[key]
            for key in ("name", "uid", "podUID", "hostname", "fingerprint")), "invalid-agent-binding")
        require(re.fullmatch(r"[0-9a-f]{64}", value["fingerprint"]), "invalid-source-fingerprint")
    require(len({value["uid"] for value in bindings.values()}) == 3 and
            len({value["podUID"] for value in bindings.values()}) == 3, "distinct-agents-required")


def make_plan(bindings, targets, dns_name, health):
    validate_bindings(bindings)
    validate_targets(targets, dns_name)
    require(isinstance(health, dict) and set(health) == set(targets), "two-witnesses-required")
    for name in targets:
        value = health[name]
        require(isinstance(value, dict) and set(value) == {"instance", "uptime", "overflow", "tcp", "udp"} and
                value.get("overflow") is False and isinstance(value.get("instance"), str) and
                re.fullmatch(HEX, value["instance"]) and value.get("tcp") == [18080, 18082] and
                value.get("udp") == [18081, 18083], "witness-not-ready")
        require(type(value.get("uptime")) in (int, float) and
                math.isfinite(value["uptime"]) and value["uptime"] >= 0, "invalid-witness-clock")
    plan = {"version": 1, "checkerDigest": checker_digest(), "run": uuid.uuid4().hex,
            "agents": bindings, "targets": targets,
            "dnsName": dns_name, "witnesses": health, "phases": {}}
    for phase in PHASES:
        plan["phases"][phase] = {
            "markers": {position: uuid.uuid4().hex for position in ("before", "after")},
            "cases": [{"target": target, "protocol": protocol, "port": port, "nonce": uuid.uuid4().hex}
                      for target in targets for protocol, port in PORTS],
        }
    plan["digest"] = fingerprint(plan)
    return plan


def validate_plan(plan):
    require(isinstance(plan, dict) and type(plan.get("version")) is int and plan["version"] == 1 and
            isinstance(plan.get("run"), str) and re.fullmatch(HEX, plan["run"]), "invalid-plan")
    require(plan.get("digest") == fingerprint({k: v for k, v in plan.items() if k != "digest"}),
            "plan-digest-mismatch")
    require(plan.get("checkerDigest") == checker_digest(), "checker-source-changed")
    require(isinstance(plan.get("phases"), dict) and set(plan["phases"]) == set(PHASES), "invalid-plan-phases")
    validate_targets(plan["targets"], plan["dnsName"])
    validate_bindings(plan.get("agents"))
    nonces = []
    expected = {(target, protocol, port) for target in ("restricted", "outside") for protocol, port in PORTS}
    for phase in PHASES:
        value = plan["phases"][phase]
        require(isinstance(value, dict) and isinstance(value.get("markers"), dict) and
                set(value["markers"]) == {"before", "after"}, "missing-plan-markers")
        cases = value.get("cases")
        require(isinstance(cases, list) and len(cases) == 8 and all(
            isinstance(case, dict) and set(case) == {"target", "protocol", "port", "nonce"} and
            type(case["port"]) is int for case in cases),
            "invalid-plan-cases")
        require({(case["target"], case["protocol"], case["port"]) for case in cases} == expected,
                "invalid-plan-cases")
        nonces.extend(value["markers"].values())
        nonces.extend(case["nonce"] for case in cases)
    require(all(isinstance(value, str) and re.fullmatch(HEX, value) for value in nonces) and
            len(set(nonces)) == len(nonces), "invalid-or-repeated-nonce")


def probe_payload(plan, phase):
    validate_plan(plan)
    require(phase in PHASES, "invalid-phase")
    agent = plan["agents"][phase_role(phase)]
    return {**plan["phases"][phase], "run": plan["run"], "phase": phase,
            "planDigest": plan["digest"], "agentUID": agent["uid"], "podUID": agent["podUID"],
            "hostname": agent["hostname"], "targets": plan["targets"], "dnsName": plan["dnsName"]}


def verify_observations(plan, reports, witnesses):
    validate_plan(plan)
    require(isinstance(witnesses, dict) and set(witnesses) == {"restricted", "outside"},
            "two-witness-observations-required")
    require(isinstance(reports, list) and len(reports) == 4 and
            all(isinstance(report, dict) for report in reports), "four-phase-results-required")
    require({report.get("phase") for report in reports} == set(PHASES), "missing-or-duplicate-phase")
    observed = {}
    for target in ("restricted", "outside"):
        value = witnesses[target]
        require(isinstance(value, dict) and set(value) == {"instance", "uptime", "overflow", "events"} and
                value.get("instance") == plan["witnesses"][target]["instance"] and
                value.get("overflow") is False, "witness-replaced-or-overflowed")
        age = value.get("uptime")
        require(type(age) in (int, float) and math.isfinite(age) and
                0 <= age - plan["witnesses"][target]["uptime"] <= MAX_AGE, "stale-witness-observation")
        require(isinstance(value.get("events"), list), "invalid-witness-events")
        by_nonce = {}
        sequences = set()
        for item in value["events"]:
            require(isinstance(item, dict), "invalid-witness-event")
            receipt(item, plan["run"], item.get("nonce"), item.get("protocol"), item.get("port"))
            require(item["instance"] == value["instance"] and isinstance(item["nonce"], str) and
                    re.fullmatch(HEX, item["nonce"]) and item["nonce"] not in by_nonce and
                    item["sequence"] not in sequences, "duplicate-or-invalid-witness-event")
            by_nonce[item["nonce"]] = item
            sequences.add(item["sequence"])
        observed[target] = by_nonce
    consumed = {target: set() for target in observed}
    windows = {}
    denials = 0
    for report in reports:
        phase = report["phase"]
        payload = probe_payload(plan, phase)
        require(report.get("status") == "observed" and report.get("dnsResolved") is True and all(
            report.get(key) == payload[key] for key in ("run", "phase", "planDigest", "agentUID", "podUID", "hostname")),
            "result-binding-mismatch")
        markers = report.get("markers")
        require(isinstance(markers, list) and len(markers) == 2, "missing-phase-markers")
        for position, marker in zip(("before", "after"), markers):
            nonce = payload["markers"][position]
            require(marker == observed["outside"].get(nonce), "marker-not-independently-observed")
            receipt(marker, plan["run"], nonce, "TCP", 18082)
            consumed["outside"].add(nonce)
        require(markers[0]["sequence"] < markers[1]["sequence"], "invalid-phase-order")
        windows[phase] = (markers[0]["sequence"], markers[1]["sequence"])
        rows = report.get("checks")
        require(isinstance(rows, list) and len(rows) == 8 and all(isinstance(row, dict) for row in rows),
                "eight-network-checks-required")
        require({row.get("nonce") for row in rows} == {case["nonce"] for case in payload["cases"]},
                "missing-or-duplicate-network-check")
        for case in payload["cases"]:
            row = next(row for row in rows if row["nonce"] == case["nonce"])
            require(type(row.get("port")) is int and all(row.get(key) == value for key, value in case.items()),
                    "network-case-mismatch")
            denied = (phase == "cidr" and case["target"] == "restricted") or (
                phase == "ports" and (case["protocol"], case["port"]) in (("TCP", 18080), ("UDP", 18081)))
            wanted = ("connect-timeout" if case["protocol"] == "TCP" else "receive-timeout") if denied else "allowed"
            require(row.get("outcome") == wanted, "unexpected-network-outcome")
            event = observed[case["target"]].get(case["nonce"])
            if denied:
                require(event is None and "witness" not in row, "denied-packet-was-observed")
                denials += 1
            else:
                require(event is not None and row.get("witness") == event, "allowed-packet-not-observed")
                receipt(event, plan["run"], case["nonce"], case["protocol"], case["port"])
                consumed[case["target"]].add(case["nonce"])
                if case["target"] == "outside":
                    require(markers[0]["sequence"] < event["sequence"] < markers[1]["sequence"],
                            "packet-outside-phase")
    for phase in ("cidr", "ports"):
        require(windows["control-before"][1] < windows[phase][0] and
                windows[phase][1] < windows["control-after"][0], "positive-controls-must-bracket-denials")
    require(all(set(observed[target]) == consumed[target] for target in observed), "unexpected-witness-event")
    return {"status": "passed", "checks": 32, "denials": denials, "qualifiesExecution": False,
            "ownerAccess": "not-tested", "modelTool": "not-tested", "cleanup": "not-tested"}


def qualification():
    spec = importlib.util.spec_from_file_location(
        "sandbox_qualification", Path(__file__).with_name("sandbox-platform-qualify.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


QUALIFICATION = qualification()


def client(args):
    require(Path(args.kubeconfig).is_absolute() and Path(args.kubeconfig).is_file(),
            "absolute-kubeconfig-required")
    module = QUALIFICATION
    try:
        module.DeploymentKubernetes(args.kubeconfig, args.context, args.cluster_uid).credentials()
        kube = module.Kubernetes(args.kubeconfig, args.context, args.cluster_uid)
        kube.check_target()
    except module.QualificationError as error:
        raise CheckError("explicit-target-validation-failed") from error
    return kube


def identity(value, kind, name):
    api = ("v1" if kind in ("Pod", "Namespace") else
           "node.k8s.io/v1" if kind == "RuntimeClass" else "cetirun.microsoft.com/v1alpha1")
    require(isinstance(value, dict) and value.get("kind") == kind and value.get("apiVersion") == api,
            "resource-kind-mismatch")
    meta = value.get("metadata", {})
    require(meta.get("name") == name and isinstance(meta.get("uid"), str) and meta["uid"] and
            not meta.get("deletionTimestamp"), "resource-identity-incomplete-or-terminating")
    return meta


def snapshot(kube, name):
    require(isinstance(name, str) and re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,52}[a-z0-9])?", name),
            "invalid-agent-name")
    agent = kube.get("agent", name)
    meta = identity(agent, "Agent", name)
    require(agent.get("status", {}).get("phase") == "Ready" and
            agent["spec"]["agent"]["type"] == "openclaw", "ready-native-openclaw-required")
    namespace = "cetirun-" + name
    ns = kube.get("namespace", namespace)
    ns_meta = identity(ns, "Namespace", namespace)
    require(ns_meta.get("labels", {}).get(PROTECTED) == "true" and any(
        owner.get("apiVersion") == "cetirun.microsoft.com/v1alpha1" and
        owner.get("kind") == "Agent" and owner.get("name") == name and owner.get("uid") == meta["uid"]
        for owner in ns_meta.get("ownerReferences", [])), "protected-namespace-owner-mismatch")
    require(ns_meta["labels"].get("control-plane") != "true" and
            ns_meta["labels"].get("kubernetes.azure.com/managedby") != "aks", "namespace-excludes-admission")
    listing = document(kube.run(["get", "pods", "--namespace", namespace, "-o", "json"]))
    require(listing.get("kind") in ("List", "PodList") and isinstance(listing.get("items"), list),
            "invalid-pod-list")
    pods = [pod for pod in listing["items"] if not pod.get("metadata", {}).get("deletionTimestamp")]
    require(len(pods) == 1, "one-native-pod-required")
    pod = pods[0]
    pod_meta = identity(pod, "Pod", pod["metadata"]["name"])
    require(pod_meta.get("namespace") == namespace and
            pod_meta.get("labels", {}).get(AGENT_UID) == meta["uid"] and
            pod.get("status", {}).get("phase") == "Running" and
            pod["spec"].get("runtimeClassName") and any(
                condition.get("type") == "Ready" and condition.get("status") == "True"
                for condition in pod.get("status", {}).get("conditions", [])), "native-pod-not-ready-or-owned")
    runtime = kube.get("runtimeclass", pod["spec"]["runtimeClassName"])
    runtime_meta = identity(runtime, "RuntimeClass", pod["spec"]["runtimeClassName"])
    require(isinstance(runtime.get("handler"), str) and runtime["handler"], "runtime-handler-required")
    statuses = pod.get("status", {}).get("containerStatuses", [])
    require(statuses and all(item.get("ready") is True and isinstance(item.get("imageID"), str) and
                            re.search(r"sha256:[0-9a-f]{64}$", item["imageID"]) for item in statuses),
            "ready-image-digests-required")
    team_name, sandbox_name = agent["spec"].get("team"), agent["spec"].get("sandboxRef")
    require(QUALIFICATION.resource_name(team_name) and QUALIFICATION.resource_name(sandbox_name),
            "explicit-team-sandbox-required")
    team, sandbox = kube.get("team", team_name), kube.get("sandbox", sandbox_name)
    policy_name = team.get("spec", {}).get("agentPolicyRef")
    require(QUALIFICATION.resource_name(policy_name), "single-policy-reference-required")
    policy = kube.get("agentpolicy", policy_name)
    objects = ((agent, "Agent", name), (team, "Team", team_name),
               (sandbox, "Sandbox", sandbox_name), (policy, "AgentPolicy", policy_name))
    pins = []
    for value, kind, expected_name in objects:
        metadata = identity(value, kind, expected_name)
        require(type(metadata.get("generation")) is int, "resource-generation-required")
        pins.append({"kind": kind, "uid": metadata["uid"], "generation": metadata["generation"],
                     "specDigest": fingerprint(value["spec"])})
    require(sandbox["spec"].get("type") == "kata", "kata-sandbox-required")
    data = {"objects": pins, "namespaceUID": ns_meta["uid"], "podUID": pod_meta["uid"],
            "podSpec": fingerprint(pod["spec"]), "runtimeUID": runtime_meta["uid"],
            "podLabels": pod_meta.get("labels", {}), "namespaceLabels": ns_meta.get("labels", {}),
            "runtimeHandler": runtime["handler"],
            "runtimeSpec": fingerprint({key: runtime.get(key) for key in ("handler", "overhead", "scheduling")}),
            "images": [{key: item.get(key) for key in ("name", "imageID", "restartCount")} for item in statuses]}
    network = policy["spec"].get("network", {})
    return {"name": name, "uid": meta["uid"], "podUID": pod_meta["uid"],
            "hostname": pod["spec"].get("hostname") or pod_meta["name"],
            "fingerprint": fingerprint(data),
            "policy": {key: network.get(key, []) for key in ("blockedCIDRs", "blockedPorts")},
            "policyUID": policy["metadata"]["uid"], "teamUID": team["metadata"]["uid"]}


def policy_matrix(bindings, targets):
    require(len({value["policyUID"] for value in bindings.values()}) == 3 and
            len({value["teamUID"] for value in bindings.values()}) == 3, "separate-policy-team-chains-required")
    for role, binding in bindings.items():
        network = binding["policy"]
        cidrs, ports = network.get("blockedCIDRs", []), network.get("blockedPorts", [])
        require(isinstance(cidrs, list) and isinstance(ports, list), "invalid-network-controls")
        if role == "cidr":
            require(len(cidrs) == 1 and ports == [], "cidr-comparison-policy-required")
            try:
                block = ipaddress.ip_network(cidrs[0], strict=True)
                require(ipaddress.ip_address(targets["restricted"]) in block and
                        ipaddress.ip_address(targets["outside"]) not in block, "cidr-witness-scope-mismatch")
            except ValueError as error:
                raise CheckError("invalid-policy-cidr") from error
        elif role == "ports":
            require(cidrs == [] and len(ports) == 2 and
                    {(item.get("protocol"), item.get("port")) for item in ports} == {("TCP", 18080), ("UDP", 18081)},
                    "port-comparison-policy-required")
        else:
            require(cidrs == [] and ports == [], "unrestricted-control-policy-required")


def verify_bindings(kube, bindings):
    kube.check_target()
    for value in bindings.values():
        require(snapshot(kube, value["name"]) == value, "agent-or-configuration-changed")


def command(plan, phase):
    payload = probe_payload(plan, phase)
    functions = (require, document, remaining_timeout, receipt, exchange, run_probe, probe_worker, supervised_probe)
    program = "import http.client,ipaddress,json,multiprocessing,os,re,signal,socket,sys,time\n"
    program += f"MAX_BYTES={MAX_BYTES}\nHEX={HEX!r}\nPROBE_TIMEOUT={PROBE_TIMEOUT}\n"
    program += "class CheckError(Exception): pass\n"
    program += "\n".join(inspect.getsource(function) for function in functions)
    program += (
        "\ntry:\n"
        " result=supervised_probe(json.loads(sys.argv[1]))\n"
        f" print({RESULT_PREFIX!r}+json.dumps(result,separators=(',',':')))\n"
        "except (CheckError,KeyError,TypeError,ValueError,OSError,KeyboardInterrupt,SystemExit) as error:\n"
        " print('Native network probe failed; no result accepted.',file=sys.stderr)\n"
        " sys.exit(error.code if isinstance(error,SystemExit) else 130 if isinstance(error,KeyboardInterrupt) else 1)\n"
    )
    return shlex.join(["python3", "-c", program, json.dumps(payload, separators=(",", ":"))])


def read_file(path, *, report=False):
    try:
        with open(path, encoding="utf-8") as stream:
            raw = stream.read(MAX_BYTES + 1)
    except (OSError, UnicodeError) as error:
        raise CheckError("input-file-unavailable") from error
    require(len(raw.encode("utf-8")) <= MAX_BYTES, "input-file-too-large")
    if report:
        matches = [line[len(RESULT_PREFIX):] for line in raw.splitlines() if line.startswith(RESULT_PREFIX)]
        if matches:
            require(len(matches) == 1, "duplicate-probe-output")
            raw = matches[0]
    return document(raw)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    prepare = modes.add_parser("prepare", help="Read current Agent bindings and create a fresh private plan")
    for role in ("control", "cidr", "ports"):
        prepare.add_argument("--" + role + "-agent", required=True)
    prepare.add_argument("--restricted-witness", required=True)
    prepare.add_argument("--outside-witness", required=True)
    prepare.add_argument("--dns-name", required=True)
    prepare.add_argument("--output", required=True)
    emit = modes.add_parser("command", help="Print a credential-free command for the normal owner terminal")
    emit.add_argument("--phase", required=True, choices=PHASES)
    verify = modes.add_parser("verify", help="Check current bindings and independent witness observations")
    verify.add_argument("--result", action="append", required=True)
    for subparser in (emit, verify):
        subparser.add_argument("--plan", required=True)
    for subparser in (prepare, verify):
        subparser.add_argument("--kubeconfig", required=True)
        subparser.add_argument("--context", required=True)
        subparser.add_argument("--cluster-uid", required=True)
    args = parser.parse_args(argv)
    try:
        if args.mode == "command":
            print(command(read_file(args.plan), args.phase))
            return 0
        if args.mode == "prepare":
            targets = {"restricted": args.restricted_witness, "outside": args.outside_witness}
            validate_targets(targets, args.dns_name)
        kube = client(args)
        if args.mode == "prepare":
            bindings = {role: snapshot(kube, getattr(args, role + "_agent")) for role in ("control", "cidr", "ports")}
            policy_matrix(bindings, targets)
            health = {name: request_json(address, "/healthz") for name, address in targets.items()}
            plan = make_plan(bindings, targets, args.dns_name, health)
            plan["clusterUID"] = args.cluster_uid
            plan["digest"] = fingerprint({key: value for key, value in plan.items() if key != "digest"})
            verify_bindings(kube, bindings)
            descriptor = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(plan, stream, indent=2)
                stream.write("\n")
            print(json.dumps({"status": "prepared", "phases": list(PHASES), "qualifiesExecution": False}))
        else:
            plan = read_file(args.plan)
            validate_plan(plan)
            require(plan.get("clusterUID") == args.cluster_uid, "plan-cluster-mismatch")
            verify_bindings(kube, plan["agents"])
            reports = [read_file(path, report=True) for path in args.result]
            witnesses = {name: request_json(address, "/events/" + plan["run"])
                         for name, address in plan["targets"].items()}
            result = verify_observations(plan, reports, witnesses)
            verify_bindings(kube, plan["agents"])
            print(json.dumps(result))
        return 0
    except (CheckError, QUALIFICATION.QualificationError, OSError, KeyError, TypeError, ValueError) as error:
        reason = str(error) if isinstance(error, CheckError) else "invalid-input-or-unavailable-resource"
        print(json.dumps({"status": "failed", "reason": reason, "qualifiesExecution": False}))
        return 1


if __name__ == "__main__":
    sys.exit(main())
