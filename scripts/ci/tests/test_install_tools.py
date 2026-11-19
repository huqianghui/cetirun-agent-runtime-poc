# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Offline installer regressions using synthetic archives and mocked downloads."""

from contextlib import redirect_stderr, redirect_stdout
import hashlib
import io
import json
import os
from pathlib import Path
import ssl
import stat
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import MagicMock, call, patch
from urllib.error import HTTPError, URLError

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import install_tools


MEMBERS = {
    name: f"controller-tools/envtest/{name}"
    for name in ("kube-apiserver", "etcd", "kubectl")
}
PAYLOADS = {member: name.encode() for name, member in MEMBERS.items()}


def archive_bytes(entries):
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
        for name, payload in entries:
            if isinstance(payload, tarfile.TarInfo):
                archive.addfile(payload)
            else:
                entry = tarfile.TarInfo(name)
                entry.size = len(payload)
                archive.addfile(entry, io.BytesIO(payload))
    return buffer.getvalue()


class Download(io.BytesIO):
    def __init__(self, data, length=None):
        super().__init__(data)
        self.headers = {} if length is None else {"Content-Length": str(length)}
        self.read_sizes = []

    def read(self, size=-1):
        self.read_sizes.append(size)
        return super().read(size)


class InstallerTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="cetirun-installer-test-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.destination = self.root / "bin"
        self.manifest = self.root / "tools.json"
        self.data = archive_bytes(PAYLOADS.items())

    def spec(self, data=None, **overrides):
        checksum = hashlib.sha256(self.data if data is None else data).hexdigest()
        spec = {
            "url": "https://example.com/envtest-linux-{arch}.tar.gz",
            "sha256": {"amd64": checksum, "arm64": checksum},
            "members": dict(MEMBERS),
        }
        spec.update(overrides)
        return spec

    def install(self, manifest=None, names=("envtest",), fetch=None,
                *, arch=None, machine="x86_64"):
        self.manifest.write_text(json.dumps(manifest or {"envtest": self.spec()}))
        stderr = io.StringIO()
        with (
            patch.object(install_tools, "MANIFEST", self.manifest),
            patch.object(install_tools.platform, "system", return_value="Linux"),
            patch.object(install_tools.platform, "machine", return_value=machine),
            patch.object(
                install_tools,
                "urlopen",
                side_effect=fetch or (lambda *a, **kw: Download(self.data)),
            ) as download,
            redirect_stdout(io.StringIO()),
            redirect_stderr(stderr),
        ):
            options = [] if arch is None else ["--arch", arch]
            result = install_tools.main(["--bin-dir", str(self.destination), *options, *names])
        return result, download, stderr.getvalue()

    def assert_no_outputs(self):
        self.assertFalse(self.destination.exists())

    def test_transient_http_errors_retry_before_installing_verified_payloads(self):
        for status in (408, 429, 500, 502, 503, 504):
            with self.subTest(status=status):
                self.destination = self.root / str(status)
                body = Download(b"synthetic HTTP error")
                failure = HTTPError("https://example.com/tool", status, "temporary", {}, body)
                with patch.object(install_tools.time, "sleep") as sleep:
                    result, download, error = self.install(fetch=[failure, Download(self.data)])
                self.assertEqual(result, 0, error)
                self.assertEqual(download.call_count, 2)
                sleep.assert_called_once_with(5)
                self.assertTrue(body.closed)
                self.assertIn(f"HTTP {status}", error)
                for name, member in MEMBERS.items():
                    self.assertEqual((self.destination / name).read_bytes(), PAYLOADS[member])

    def test_connection_errors_and_timeouts_retry(self):
        for index, failure in enumerate((
            TimeoutError("synthetic timeout"), ConnectionResetError("synthetic reset"),
            URLError(TimeoutError("synthetic timeout")),
            URLError(ConnectionResetError("synthetic reset")),
        )):
            with self.subTest(error=failure):
                self.destination = self.root / str(index)
                with patch.object(install_tools.time, "sleep") as sleep:
                    result, download, error = self.install(fetch=[failure, Download(self.data)])
                self.assertEqual(result, 0, error)
                self.assertEqual(download.call_count, 2)
                sleep.assert_called_once_with(5)

    def test_read_timeout_closes_response_and_restarts_the_download(self):
        class InterruptedDownload(Download):
            def read(self, size=-1):
                super().read(1)
                raise TimeoutError("synthetic interrupted read")

        interrupted = InterruptedDownload(self.data)
        with patch.object(install_tools.time, "sleep") as sleep:
            result, download, error = self.install(fetch=[interrupted, Download(self.data)])
        self.assertEqual(result, 0, error)
        self.assertEqual(download.call_count, 2)
        self.assertTrue(interrupted.closed)
        sleep.assert_called_once_with(5)
        for name, member in MEMBERS.items():
            self.assertEqual((self.destination / name).read_bytes(), PAYLOADS[member])

    def test_retry_exhaustion_fails_without_installing_earlier_tools(self):
        raw_spec = self.spec(url="https://example.com/raw")
        del raw_spec["members"]

        def fetch(url, **kwargs):
            if url.endswith("/raw"):
                raise HTTPError(url, 504, "Gateway Time-out", {}, None)
            return Download(self.data)

        with patch.object(install_tools.time, "sleep") as sleep:
            result, download, error = self.install(
                {"envtest": self.spec(), "raw": raw_spec}, ("envtest", "raw"), fetch
            )
        self.assertEqual(result, 1)
        self.assertEqual(download.call_count, 5)
        self.assertEqual(sleep.call_args_list, [call(5), call(15), call(30)])
        self.assertIn("HTTP Error 504", error)
        self.assert_no_outputs()

    def test_permanent_http_and_tls_errors_are_not_retried(self):
        failures = [
            HTTPError("https://example.com/tool", status, "permanent", {}, None)
            for status in (400, 401, 403, 404, 410, 501)
        ] + [
            URLError(ssl.SSLCertVerificationError("synthetic certificate failure")),
            URLError("synthetic unsupported error"),
        ]
        for failure in failures:
            with self.subTest(error=failure):
                with patch.object(install_tools.time, "sleep") as sleep:
                    result, download, _ = self.install(fetch=[failure])
                self.assertEqual(result, 1)
                download.assert_called_once()
                sleep.assert_not_called()
                self.assert_no_outputs()

    def test_explicit_arch_selects_locked_helm_pin_and_member(self):
        manifest = json.loads(install_tools.MANIFEST.read_text())
        spec = manifest["helm"]
        for arch, machine in (("amd64", "aarch64"), ("arm64", "x86_64")):
            with self.subTest(arch=arch, machine=machine):
                self.assertEqual(
                    install_tools.plan(
                        manifest, ["helm"], self.destination, "Linux", machine,
                        arch=arch,
                    ),
                    [(self.destination / "helm", spec["url"].format(arch=arch),
                      spec["sha256"][arch], spec["member"].format(arch=arch))],
                )

    def test_cross_arch_cli_installs_only_target_helm(self):
        archives = {
            arch: archive_bytes([(f"linux-{arch}/helm", f"synthetic Helm {arch}".encode())])
            for arch in ("amd64", "arm64")
        }
        manifest = {"helm": {
            "url": "https://example.com/helm-linux-{arch}.tar.gz",
            "member": "linux-{arch}/helm",
            "sha256": {arch: hashlib.sha256(data).hexdigest() for arch, data in archives.items()},
        }}
        for arch, machine in (("amd64", "aarch64"), ("arm64", "x86_64")):
            with self.subTest(arch=arch, machine=machine):
                self.destination = self.root / arch
                result, download, error = self.install(
                    manifest, ("helm",), lambda *a, **kw: Download(archives[arch]),
                    arch=arch, machine=machine,
                )
                self.assertEqual(result, 0, error)
                download.assert_called_once_with(
                    f"https://example.com/helm-linux-{arch}.tar.gz", timeout=45
                )
                self.assertEqual(list(self.destination.iterdir()), [self.destination / "helm"])
                self.assertEqual((self.destination / "helm").read_bytes(), f"synthetic Helm {arch}".encode())
                self.assertEqual(stat.S_IMODE((self.destination / "helm").stat().st_mode), 0o755)

    def test_explicit_arch_rejects_unsupported_values_and_non_linux(self):
        manifest = {"envtest": self.spec()}
        for arch, system in (("ppc64le", "Linux"), ("", "Linux"), ("arm64", "Darwin")):
            with self.subTest(arch=arch, system=system):
                with self.assertRaisesRegex(ValueError, "Linux amd64/arm64 only"):
                    install_tools.plan(manifest, ["envtest"], self.destination, system,
                                       "x86_64", arch=arch)
        with patch.object(install_tools, "urlopen") as download, redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as error:
                install_tools.main(["--bin-dir", str(self.destination), "--arch", "ppc64le", "helm"])
        self.assertEqual(error.exception.code, 2)
        download.assert_not_called()
        self.assert_no_outputs()

    def test_operator_packaging_passes_target_arch_to_existing_installer(self):
        root = Path(__file__).resolve().parents[3]
        dockerfile = (root / "app/cetirun-operator/Dockerfile").read_text()
        self.assertIn("ARG TARGETARCH", dockerfile)
        self.assertIn("COPY scripts/ci/install_tools.py scripts/ci/tools.json /tools/", dockerfile)
        self.assertIn('python3 /tools/install_tools.py --arch "${TARGETARCH}" --bin-dir /out/bin helm', dockerfile)
        self.assertIn(
            "COPY --from=cetiadmin-tools --chmod=0755 /out/bin/helm /usr/local/share/cetirun/cetiadmin/bin/helm",
            dockerfile,
        )
        self.assertNotIn("BUILDARCH", dockerfile)
        self.assertNotIn("get.helm.sh", dockerfile)
        self.assertNotIn("HELM_VERSION", dockerfile)
        runtime = dockerfile[dockerfile.rfind("\nFROM "):]
        self.assertIn("azurelinux/distroless/base:", runtime)
        self.assertIn("USER 65532:65532", runtime)
        self.assertIn('ENTRYPOINT ["/manager"]', runtime)
        self.assertNotIn("RUN ", runtime)

    def test_envtest_pins_and_archive_members_match_both_releases(self):
        manifest = json.loads(install_tools.MANIFEST.read_text())
        spec = manifest["envtest"]
        self.assertEqual(spec["version"], "1.37.0")
        source = "https://github.com/kubernetes-sigs/controller-tools/releases"
        self.assertEqual(spec["source"], f"{source}/tag/envtest-v1.37.0")
        self.assertEqual(spec["members"], MEMBERS)
        self.assertEqual(
            spec["sha256"],
            {
                "amd64": "f03faedfc68d2a78cee5cb02618bb44e7f67044f4c5fc957f72d3307fda61a9c",
                "arm64": "ff2a0cdcadd0a6b3181faa4758e28fc63b774d2e40f29f62645dd7fa0b881a2c",
            },
        )
        for machine, arch in (
            ("x86_64", "amd64"), ("aarch64", "arm64"), ("arm64", "arm64")
        ):
            with self.subTest(machine=machine):
                entries = install_tools.plan(
                    manifest, ["envtest"], self.destination, "Linux", machine
                )
                self.assertEqual(
                    entries,
                    [
                        (
                            self.destination / name,
                            f"{source}/download/envtest-v1.37.0/"
                            f"envtest-v1.37.0-linux-{arch}.tar.gz",
                            spec["sha256"][arch],
                            member,
                        )
                        for name, member in MEMBERS.items()
                    ],
                )

    def test_bundle_downloads_once_and_installs_only_named_executables(self):
        self.destination.mkdir()
        sentinel = self.destination / "unrelated"
        sentinel.write_bytes(b"keep")
        result, download, error = self.install()
        self.assertEqual(result, 0, error)
        download.assert_called_once_with(
            "https://example.com/envtest-linux-amd64.tar.gz", timeout=45
        )
        self.assertEqual(
            {path.name for path in self.destination.iterdir()}, {*MEMBERS, "unrelated"}
        )
        for name, member in MEMBERS.items():
            binary = self.destination / name
            self.assertEqual(binary.read_bytes(), PAYLOADS[member])
            self.assertEqual(stat.S_IMODE(binary.stat().st_mode), 0o755)
        self.assertEqual(sentinel.read_bytes(), b"keep")

    def test_transient_http_errors_retry_raw_and_bundled_downloads(self):
        for code in (408, 429, 500, 502, 503, 504):
            for bundled in (True, False):
                with self.subTest(code=code, bundled=bundled):
                    self.destination = self.root / f"bin-{code}-{bundled}"
                    data = self.data if bundled else b"synthetic raw tool"
                    spec = self.spec(data=data)
                    if not bundled:
                        del spec["members"]
                    failure = HTTPError(spec["url"], code, "temporary failure", {}, io.BytesIO())
                    with patch("time.sleep") as sleep:
                        result, download, error = self.install(
                            {"envtest": spec}, fetch=[failure, Download(data)]
                        )
                    self.assertEqual(result, 0, error)
                    self.assertEqual(download.call_count, 2)
                    self.assertTrue(failure.fp.closed)
                    sleep.assert_called_once_with(5)
                    if bundled:
                        for name, member in MEMBERS.items():
                            self.assertEqual((self.destination / name).read_bytes(), PAYLOADS[member])
                    else:
                        self.assertEqual((self.destination / "envtest").read_bytes(), data)

    def test_transient_http_retries_stop_without_installing_partial_tools(self):
        raw_spec = self.spec(url="https://example.com/raw")
        del raw_spec["members"]

        def fetch(url, **kwargs):
            if url.endswith("/raw"):
                raise HTTPError(url, 504, "Gateway Time-out", {}, io.BytesIO())
            return Download(self.data)

        with patch("time.sleep") as sleep:
            result, download, error = self.install(
                {"envtest": self.spec(), "raw": raw_spec}, ("envtest", "raw"), fetch
            )
        self.assertEqual(result, 1)
        self.assertEqual(download.call_count, 5)
        self.assertEqual(sleep.call_args_list, [call(5), call(15), call(30)])
        self.assertIn("HTTP Error 504", error)
        self.assert_no_outputs()

    def test_permanent_http_errors_do_not_retry(self):
        for code in (401, 403, 404):
            with self.subTest(code=code), patch("time.sleep") as sleep:
                failure = HTTPError("https://example.com/tool", code, "permanent failure", {}, io.BytesIO())
                result, download, error = self.install(fetch=[failure])
                self.assertEqual(result, 1)
                self.assertIn(f"HTTP Error {code}", error)
                download.assert_called_once()
                sleep.assert_not_called()
                self.assert_no_outputs()

    def test_recovered_download_still_requires_matching_checksum(self):
        failure = HTTPError("https://example.com/tool", 504, "Gateway Time-out", {}, io.BytesIO())
        with patch("time.sleep") as sleep, patch.object(install_tools.tarfile, "open") as parse:
            result, download, error = self.install(fetch=[failure, Download(b"tampered")])
        self.assertEqual(result, 1)
        self.assertIn("SHA256 mismatch", error)
        self.assertEqual(download.call_count, 2)
        sleep.assert_called_once_with(5)
        parse.assert_not_called()
        self.assert_no_outputs()

    def test_oversized_content_length_fails_without_reading_body(self):
        limit = len(self.data)
        response = Download(self.data, length=limit + 1)
        with (
            patch.object(install_tools, "MAX_DOWNLOAD_BYTES", limit),
            patch.object(install_tools, "binaries_bytes") as parse,
        ):
            result, _, error = self.install(fetch=lambda *a, **kw: response)
        self.assertEqual(result, 1)
        self.assertIn("Tool download exceeds", error)
        self.assertEqual(response.read_sizes, [])
        parse.assert_not_called()
        self.assert_no_outputs()

    def test_oversized_body_is_bounded_despite_missing_or_understated_length(self):
        limit = len(self.data) - 1
        for length in (None, 1, limit):
            with self.subTest(length=length):
                response = Download(self.data, length=length)
                with (
                    patch.object(install_tools, "MAX_DOWNLOAD_BYTES", limit),
                    patch.object(install_tools, "binaries_bytes") as parse,
                ):
                    result, _, error = self.install(fetch=lambda *a, **kw: response)
                self.assertEqual(result, 1)
                self.assertIn("Tool download exceeds", error)
                self.assertEqual(response.read_sizes, [limit + 1])
                parse.assert_not_called()
                self.assert_no_outputs()

    def test_invalid_content_length_fails_without_reading_body(self):
        for length in ("invalid", -1):
            with self.subTest(length=length):
                response = Download(self.data, length=length)
                result, _, error = self.install(fetch=lambda *a, **kw: response)
                self.assertEqual(result, 1)
                self.assertIn("Invalid", error)
                self.assertEqual(response.read_sizes, [])
                self.assert_no_outputs()

    def test_download_member_and_total_limits_allow_exact_boundaries(self):
        response = Download(self.data, length=len(self.data))
        with (
            patch.object(install_tools, "MAX_DOWNLOAD_BYTES", len(self.data)),
            patch.object(install_tools, "MAX_MEMBER_BYTES", max(map(len, PAYLOADS.values()))),
            patch.object(install_tools, "MAX_TOTAL_BYTES", sum(map(len, PAYLOADS.values()))),
        ):
            result, _, error = self.install(fetch=lambda *a, **kw: response)
        self.assertEqual(result, 0, error)
        self.assertEqual(response.read_sizes, [len(self.data) + 1])
        for name, member in MEMBERS.items():
            self.assertEqual((self.destination / name).read_bytes(), PAYLOADS[member])

    def test_member_declared_sizes_fail_before_any_member_payload_is_read(self):
        limit = max(map(len, PAYLOADS.values()))
        entry = tarfile.TarInfo(MEMBERS["kubectl"])
        entry.size = limit + 1
        self.data = archive_bytes({**PAYLOADS, entry.name: entry}.items())
        with (
            patch.object(install_tools, "MAX_MEMBER_BYTES", limit),
            patch.object(tarfile.TarFile, "extractfile") as extract,
        ):
            result, _, error = self.install()
        self.assertEqual(result, 1)
        self.assertIn("Tool archive member", error)
        self.assertIn("size limit", error)
        extract.assert_not_called()
        self.assert_no_outputs()

    def test_total_declared_size_fails_before_any_member_payload_is_read(self):
        limit = sum(map(len, PAYLOADS.values())) - 1
        with (
            patch.object(install_tools, "MAX_TOTAL_BYTES", limit),
            patch.object(tarfile.TarFile, "extractfile") as extract,
        ):
            result, _, error = self.install()
        self.assertEqual(result, 1)
        self.assertIn("Selected tool payload total exceeds", error)
        extract.assert_not_called()
        self.assert_no_outputs()

    def test_later_download_uses_remaining_total_budget_before_reading_members(self):
        later = self.spec(
            url="https://example.com/legacy.tar.gz", member=MEMBERS["kubectl"]
        )
        del later["members"]
        limit = sum(map(len, PAYLOADS.values())) + len(b"kubectl") - 1
        original_extract = tarfile.TarFile.extractfile
        with (
            patch.object(install_tools, "MAX_TOTAL_BYTES", limit),
            patch.object(
                tarfile.TarFile, "extractfile", autospec=True, side_effect=original_extract
            ) as extract,
        ):
            result, download, error = self.install(
                {"envtest": self.spec(), "legacy": later}, ("envtest", "legacy")
            )
        self.assertEqual(result, 1)
        self.assertIn("Selected tool payload total exceeds", error)
        self.assertEqual(download.call_count, 2)
        self.assertEqual(extract.call_count, len(MEMBERS))
        self.assert_no_outputs()

    def test_raw_and_legacy_apis_enforce_size_limits(self):
        raw = b"standalone binary"
        checksum = hashlib.sha256(raw).hexdigest()
        for constant in ("MAX_DOWNLOAD_BYTES", "MAX_MEMBER_BYTES", "MAX_TOTAL_BYTES"):
            with self.subTest(limit=constant):
                with patch.object(install_tools, constant, len(raw) - 1):
                    with self.assertRaisesRegex(ValueError, "size limit"):
                        install_tools.binary_bytes(raw, checksum)
        with patch.object(install_tools, "MAX_MEMBER_BYTES", len(b"etcd") - 1):
            with self.assertRaisesRegex(ValueError, "Tool archive member.*size limit"):
                install_tools.binary_bytes(
                    self.data, hashlib.sha256(self.data).hexdigest(), MEMBERS["etcd"]
                )

    def test_legacy_member_and_raw_tools_keep_plan_and_binary_api(self):
        raw = b"standalone binary"
        raw_spec = self.spec(data=raw)
        del raw_spec["members"]
        raw_spec["url"] = "https://example.com/raw-{arch}"
        member_spec = self.spec(member=MEMBERS["etcd"])
        del member_spec["members"]
        manifest = {"raw": raw_spec, "legacy": member_spec}
        plan = install_tools.plan(
            manifest, ["raw", "legacy"], self.destination, "Linux", "x86_64"
        )
        self.assertEqual(
            plan,
            [
                (
                    self.destination / "raw",
                    "https://example.com/raw-amd64",
                    hashlib.sha256(raw).hexdigest(),
                    None,
                ),
                (
                    self.destination / "legacy",
                    "https://example.com/envtest-linux-amd64.tar.gz",
                    hashlib.sha256(self.data).hexdigest(),
                    MEMBERS["etcd"],
                ),
            ],
        )
        self.assertEqual(install_tools.binary_bytes(raw, plan[0][2]), raw)
        self.assertEqual(
            install_tools.binary_bytes(self.data, plan[1][2], plan[1][3]), b"etcd"
        )
        result, download, error = self.install(
            manifest, ("raw", "legacy"),
            lambda url, **kw: Download(raw if "/raw-" in url else self.data),
        )
        self.assertEqual(result, 0, error)
        self.assertEqual(download.call_count, 2)
        self.assertEqual((self.destination / "raw").read_bytes(), raw)
        self.assertEqual((self.destination / "legacy").read_bytes(), b"etcd")

    def test_bad_pin_on_later_tool_fails_before_any_download(self):
        for pin in ("0" * 63, "g" * 64, None):
            with self.subTest(pin=pin):
                later = self.spec(sha256={"amd64": pin})
                later["members"] = {"later": MEMBERS["etcd"]}
                result, download, error = self.install(
                    {"envtest": self.spec(), "later": later}, ("envtest", "later")
                )
                self.assertEqual(result, 1)
                self.assertIn("Invalid download pin", error)
                download.assert_not_called()
                self.assert_no_outputs()

    def test_non_https_url_fails_before_download(self):
        result, download, error = self.install(
            {"envtest": self.spec(url="http://example.com/unpinned.tar.gz")}
        )
        self.assertEqual(result, 1)
        self.assertIn("Invalid download pin", error)
        download.assert_not_called()
        self.assert_no_outputs()

    def test_checksum_is_verified_before_parsing_archive(self):
        with (
            patch.object(install_tools.tarfile, "open") as parse,
            patch.object(install_tools.time, "sleep") as sleep,
        ):
            result, download, error = self.install(
                {"envtest": self.spec(sha256={"amd64": "0" * 64})}
            )
        self.assertEqual(result, 1)
        self.assertIn("SHA256 mismatch", error)
        download.assert_called_once()
        sleep.assert_not_called()
        parse.assert_not_called()
        self.assert_no_outputs()

    def test_all_output_names_are_validated_before_download(self):
        for name in ("", ".", "..", "../escape", "nested/tool", "/tool", r"..\tool",
                     "tool\x00name", "-tool", "C:tool"):
            with self.subTest(name=name):
                members = {**MEMBERS, name: MEMBERS["etcd"]}
                result, download, error = self.install(
                    {"envtest": self.spec(members=members)}
                )
                self.assertEqual(result, 1)
                self.assertIn("Invalid tool output name", error)
                download.assert_not_called()
                self.assert_no_outputs()

    def test_legacy_tool_names_cannot_be_paths(self):
        for name in ("../escape", "/tool", r"dir\tool"):
            with self.subTest(name=name):
                spec = self.spec()
                del spec["members"]
                result, download, error = self.install({name: spec}, (name,))
                self.assertEqual(result, 1)
                self.assertIn("Invalid tool output name", error)
                download.assert_not_called()
                self.assert_no_outputs()

    def test_member_paths_are_validated_before_download(self):
        for member in (
            "", "/absolute", "../escape", "dir/../escape", "./tool", "dir//tool",
            "dir/./tool", "dir/tool/", r"dir\tool", "dir/\x00tool", None, 1
        ):
            with self.subTest(member=member):
                result, download, error = self.install(
                    {"envtest": self.spec(members={**MEMBERS, "extra": member})}
                )
                self.assertEqual(result, 1)
                self.assertIn("Invalid tool archive member", error)
                download.assert_not_called()
                self.assert_no_outputs()

    def test_formatted_archive_member_paths_are_validated(self):
        spec = self.spec(
            architectures={"amd64": "../escape"},
            members={"tool": "linux/{arch}/tool"},
        )
        result, download, error = self.install({"envtest": spec})
        self.assertEqual(result, 1)
        self.assertIn("Invalid tool archive member", error)
        download.assert_not_called()
        self.assert_no_outputs()

    def test_malformed_or_ambiguous_bundles_fail_before_download(self):
        for overrides in (
            {"members": {}}, {"members": []}, {"members": None},
            {"member": "tool"},
        ):
            with self.subTest(overrides=overrides):
                result, download, error = self.install(
                    {"envtest": self.spec(**overrides)}
                )
                self.assertEqual(result, 1)
                self.assertIn("Invalid tool bundle members", error)
                download.assert_not_called()
                self.assert_no_outputs()

    def test_cross_tool_output_collisions_fail_before_download(self):
        raw_spec = self.spec()
        del raw_spec["members"]
        for names in (("envtest", "kubectl"), ("kubectl", "envtest")):
            with self.subTest(names=names):
                result, download, error = self.install(
                    {"envtest": self.spec(), "kubectl": raw_spec}, names
                )
                self.assertEqual(result, 1)
                self.assertIn("Duplicate tool output", error)
                download.assert_not_called()
                self.assert_no_outputs()

    def test_existing_last_output_of_every_kind_blocks_all_downloads(self):
        for kind in ("file", "directory", "symlink", "dangling-symlink"):
            with self.subTest(kind=kind):
                self.destination = self.root / kind
                self.destination.mkdir()
                existing = self.destination / "kubectl"
                if kind == "file":
                    existing.write_bytes(b"user binary")
                elif kind == "directory":
                    existing.mkdir()
                else:
                    target = self.root / f"{kind}-target"
                    if kind == "symlink":
                        target.write_bytes(b"user binary")
                    existing.symlink_to(target)
                result, download, error = self.install()
                self.assertEqual(result, 1)
                self.assertIn("Refusing to overwrite", error)
                download.assert_not_called()
                self.assertEqual(list(self.destination.iterdir()), [existing])
                if kind in ("file", "symlink"):
                    self.assertEqual(existing.read_bytes(), b"user binary")
                if "symlink" in kind:
                    self.assertTrue(existing.is_symlink())

    def test_symlinked_destination_and_ancestors_block_downloads(self):
        target = self.root / "target"
        target.mkdir()
        link = self.root / "link"
        link.symlink_to(target, target_is_directory=True)
        dangling = self.root / "dangling"
        dangling.symlink_to(self.root / "missing", target_is_directory=True)
        for destination in (link, link / "bin", dangling, dangling / "bin"):
            with self.subTest(destination=destination):
                self.destination = destination
                result, download, error = self.install()
                self.assertEqual(result, 1)
                self.assertIn("must not contain symlinks", error)
                download.assert_not_called()
        self.assertEqual(list(target.iterdir()), [])
        self.assertFalse((self.root / "missing").exists())

    def test_non_directory_destination_ancestor_blocks_downloads(self):
        file = self.root / "file"
        file.write_bytes(b"keep")
        for destination in (file, file / "bin"):
            with self.subTest(destination=destination):
                self.destination = destination
                result, download, error = self.install()
                self.assertEqual(result, 1)
                self.assertIn("must be a directory", error)
                download.assert_not_called()
        self.assertEqual(file.read_bytes(), b"keep")

    def test_missing_late_member_leaves_no_binaries(self):
        self.data = archive_bytes(
            (member, payload) for member, payload in PAYLOADS.items()
            if member != MEMBERS["kubectl"]
        )
        result, download, error = self.install()
        self.assertEqual(result, 1)
        self.assertIn("Missing tool archive member", error)
        download.assert_called_once()
        self.assert_no_outputs()

    def test_every_selected_member_must_be_regular(self):
        for kind in (
            tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.DIRTYPE,
            tarfile.FIFOTYPE, tarfile.CHRTYPE, tarfile.BLKTYPE,
        ):
            with self.subTest(kind=kind):
                entry = tarfile.TarInfo(MEMBERS["kubectl"])
                entry.type = kind
                entry.linkname = MEMBERS["etcd"]
                entries = {**PAYLOADS, MEMBERS["kubectl"]: entry}
                self.data = archive_bytes(entries.items())
                result, download, error = self.install()
                self.assertEqual(result, 1)
                self.assertIn("not a regular file", error)
                download.assert_called_once()
                self.assert_no_outputs()

    def test_unselected_traversal_and_link_members_are_never_extracted(self):
        link = tarfile.TarInfo("link")
        link.type = tarfile.SYMTYPE
        link.linkname = "../escape"
        self.data = archive_bytes(
            [*PAYLOADS.items(), ("../escape", b"not installed"), ("link", link)]
        )
        result, _, error = self.install()
        self.assertEqual(result, 0, error)
        self.assertEqual({p.name for p in self.destination.iterdir()}, set(MEMBERS))
        self.assertFalse((self.root / "escape").exists())

    def test_direct_binary_api_rejects_requested_traversal_member(self):
        data = archive_bytes([("../escape", b"not installed")])
        with self.assertRaisesRegex(ValueError, "Invalid tool archive member"):
            install_tools.binary_bytes(data, hashlib.sha256(data).hexdigest(), "../escape")

    def test_duplicate_selected_archive_names_fail_without_outputs(self):
        self.data = archive_bytes([*PAYLOADS.items(), (MEMBERS["kubectl"], b"duplicate")])
        result, _, error = self.install()
        self.assertEqual(result, 1)
        self.assertIn("Duplicate tool archive member", error)
        self.assert_no_outputs()

    def test_corrupt_but_checksum_matching_archive_fails_without_outputs(self):
        self.data = b"not a tar archive"
        result, _, _ = self.install()
        self.assertEqual(result, 1)
        self.assert_no_outputs()

    def test_later_download_failure_leaves_no_earlier_outputs(self):
        raw_spec = self.spec(url="https://example.com/raw")
        del raw_spec["members"]

        def fetch(url, **kwargs):
            if url.endswith("/raw"):
                raise URLError("synthetic download failure")
            return Download(self.data)

        result, download, error = self.install(
            {"envtest": self.spec(), "raw": raw_spec}, ("envtest", "raw"), fetch
        )
        self.assertEqual(result, 1)
        self.assertEqual(download.call_count, 2)
        self.assertIn("synthetic download failure", error)
        self.assert_no_outputs()

    def test_output_created_during_download_is_preserved(self):
        def fetch(*args, **kwargs):
            self.destination.mkdir()
            (self.destination / "kubectl").write_bytes(b"concurrent binary")
            return Download(self.data)

        result, _, error = self.install(fetch=fetch)
        self.assertEqual(result, 1)
        self.assertIn("Refusing to overwrite", error)
        self.assertEqual({p.name for p in self.destination.iterdir()}, {"kubectl"})
        self.assertEqual((self.destination / "kubectl").read_bytes(), b"concurrent binary")

    def test_destination_symlink_created_during_download_is_rejected(self):
        target = self.root / "target"
        target.mkdir()

        def fetch(*args, **kwargs):
            self.destination.symlink_to(target, target_is_directory=True)
            return Download(self.data)

        result, _, error = self.install(fetch=fetch)
        self.assertEqual(result, 1)
        self.assertIn("must not contain symlinks", error)
        self.assertEqual(list(target.iterdir()), [])

    def test_exclusive_creation_catches_late_collision_and_rolls_back_owned_files(self):
        original_open = Path.open

        def racing_open(path, mode="r", *args, **kwargs):
            if path == self.destination / "kubectl" and mode == "xb":
                with original_open(path, "wb") as target:
                    target.write(b"concurrent binary")
            return original_open(path, mode, *args, **kwargs)

        with patch.object(Path, "open", racing_open):
            result, _, _ = self.install()
        self.assertEqual(result, 1)
        self.assertEqual({p.name for p in self.destination.iterdir()}, {"kubectl"})
        self.assertEqual((self.destination / "kubectl").read_bytes(), b"concurrent binary")

    def test_write_failure_removes_only_outputs_created_by_this_install(self):
        self.destination.mkdir()
        sentinel = self.destination / "unrelated"
        sentinel.write_bytes(b"keep")
        original_open = Path.open

        def failing_open(path, mode="r", *args, **kwargs):
            target = original_open(path, mode, *args, **kwargs)
            if path == self.destination / "kubectl" and mode == "xb":
                wrapper = MagicMock(wraps=target)
                wrapper.__enter__.return_value = wrapper
                wrapper.__exit__.side_effect = target.__exit__
                wrapper.write.side_effect = OSError("synthetic write failure")
                return wrapper
            return target

        with patch.object(Path, "open", failing_open):
            result, _, error = self.install()
        self.assertEqual(result, 1)
        self.assertIn("synthetic write failure", error)
        self.assertEqual(list(self.destination.iterdir()), [sentinel])
        self.assertEqual(sentinel.read_bytes(), b"keep")

    def test_chmod_failure_removes_all_new_outputs(self):
        chmod = os.fchmod
        calls = 0

        def failing_chmod(fd, mode):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise OSError("synthetic chmod failure")
            chmod(fd, mode)

        with patch.object(install_tools.os, "fchmod", side_effect=failing_chmod):
            result, _, error = self.install()
        self.assertEqual(result, 1)
        self.assertIn("synthetic chmod failure", error)
        self.assertEqual(list(self.destination.iterdir()), [])

    def test_rollback_does_not_delete_concurrently_replaced_output(self):
        def replace_then_fail(fd, mode):
            replaced = self.destination / "kube-apiserver"
            replaced.unlink()
            replaced.write_bytes(b"concurrent binary")
            raise OSError("synthetic replacement")

        with patch.object(install_tools.os, "fchmod", side_effect=replace_then_fail):
            result, _, error = self.install()
        self.assertEqual(result, 1)
        self.assertIn("synthetic replacement", error)
        self.assertEqual({p.name for p in self.destination.iterdir()}, {"kube-apiserver"})
        self.assertEqual(
            (self.destination / "kube-apiserver").read_bytes(), b"concurrent binary"
        )


if __name__ == "__main__":
    unittest.main()
