# Pre-proxy native upgrade fixture

The controller and runtime renderer at commit
`6414494154151b2daf74d39d1e989ebeb7ea5020` produced
[pre-proxy.json](pre-proxy.json). That commit comes before the change that
added native ingress proxy projection. It is not a fixture from the new
compiler with a revision that was rewritten to look old.

This baseline uses the current single-policy API and the `v1` authorization,
expectation, and revision formats. It does not assert a rolling migration from
the removed dual-policy prototype. The chart compatibility tests validate the
pre-proxy assets from this same source revision.

The exporter runs the `nativeSandboxFixture` that already exists in that
revision until the fixture is Ready. Then the exporter captures the fixture's
API objects, rendered OpenClaw configuration, workload, source digests,
preparation receipt, consumed single-start marker, and Gateway test-double
state. The Kubernetes controller observations and the Gateway responses are
synthetic. This fixture does not claim an actual application run or cluster
run.

Random bytes are fixed to `0x42`. In the file, named markers replace only the
dummy Secret data that results and the checksum of an empty template render.
The test reconstructs those values. Then it verifies the original object-set
SHA-256 before it loads the object set. No live credentials or deployment data
are used.

## Regeneration

Use Go 1.27.3 and the separately prepared dependencies that
[DEVELOPMENT.md](../../../../../../DEVELOPMENT.md) describes. From the current
checkout, select a new temporary directory:

```bash
source_root="$(git rev-parse --show-toplevel)"
fixture_dir="$source_root/app/cetirun-operator/internal/controller/testdata/native-upgrade"
old_root="$(mktemp -d)"
git clone --no-hardlinks --no-checkout "$source_root" "$old_root/repo"
git -C "$old_root/repo" checkout --detach 6414494154151b2daf74d39d1e989ebeb7ea5020
cp "$fixture_dir/export_test.go" \
  "$old_root/repo/app/cetirun-operator/internal/controller/export_native_upgrade_test.go"
CETIRUN_UPGRADE_FIXTURE_OUT="$fixture_dir/pre-proxy.json" \
  GOTOOLCHAIN=go1.27.3 GOPROXY=off GOFLAGS=-mod=readonly \
  go -C "$old_root/repo/app/cetirun-operator" test ./internal/controller \
    -run '^TestExportNativeUpgradeFixture$' -count=1
```

The exporter makes sure that the checkout revision is correct before it
collects any fixture data. Review the regenerated file before you commit it.
Observation timestamps can differ between runs. But the controller and
compiler provenance and the complete captured state must stay verifiable.
Afterward, remove only the temporary checkout that you created.
