// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { createHash } from "node:crypto";
import { productionLicenseNotices, productionLicenses } from "./production-licenses.mjs";

const script = fileURLToPath(new URL("./production-licenses.mjs", import.meta.url));
const license = "Copyright (c) Example Authors\r\nPermission is hereby granted.\r\n";

function fixture(t, metadata = {}) {
  const root = mkdtempSync(join(tmpdir(), "cetirun-notice-test-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  writeFileSync(join(root, "package.json"), JSON.stringify({
    name: "fixture", version: "1.0.0", ...metadata,
  }));
  return root;
}

function packageAt(root, name, metadata = {}, text = license) {
  const directory = join(root, "node_modules", name);
  mkdirSync(directory, { recursive: true });
  writeFileSync(join(directory, "package.json"), JSON.stringify({
    name, version: "1.0.0", license: "MIT", ...metadata,
  }));
  if (text !== null) writeFileSync(join(directory, "LICENSE"), text);
  return directory;
}

test("includes sorted transitive licenses and notices without running package code", t => {
  const root = fixture(t, {
    dependencies: { zebra: "1.0.0", alpha: "1.0.0" },
    devDependencies: { "not-installed-dev-tool": "1.0.0" },
  });
  const zebra = packageAt(root, "zebra", {
    dependencies: { nested: "1.0.0" },
    exports: { ".": "./index.js" },
  });
  writeFileSync(join(zebra, "index.js"), 'throw new Error("Must not execute dependencies");');
  const nested = packageAt(zebra, "nested");
  writeFileSync(join(nested, "NOTICE"), "Additional upstream attribution.\n");
  packageAt(root, "alpha");
  const output = productionLicenseNotices(join(root, "package.json"));
  assert.deepEqual([...output.matchAll(/^Package: (.+)$/gm)].map(match => match[1]), [
    "alpha@1.0.0", "nested@1.0.0", "zebra@1.0.0",
  ]);
  assert.ok(output.includes(license.replace(/\r\n/g, "\n").trimEnd()));
  assert.ok(output.includes("Additional upstream attribution."));
  assert.ok(!output.includes(root));
  assert.ok(!output.includes("not-installed-dev-tool"));
  assert.equal(productionLicenseNotices(join(root, "package.json")), output);
});

test("covers peers and installed optional dependencies while omitting absent optional ones", t => {
  const root = fixture(t, { dependencies: { alpha: "1.0.0" } });
  packageAt(root, "alpha", {
    dependencies: { absent: "1.0.0" },
    optionalDependencies: { present: "1.0.0", absent: "1.0.0" },
    peerDependencies: { peer: "1.0.0", "optional-peer": "1.0.0" },
    peerDependenciesMeta: { "optional-peer": { optional: true } },
  });
  packageAt(root, "present");
  packageAt(root, "peer");
  const output = productionLicenseNotices(join(root, "package.json"));
  for (const name of ["alpha", "present", "peer"]) assert.ok(output.includes(`Package: ${name}@1.0.0`));
  assert.ok(!output.includes("Package: absent"));
  assert.ok(!output.includes("Package: optional-peer"));
});

test("deduplicates identical copies but rejects conflicting same-version notices", t => {
  const root = fixture(t, { dependencies: { alpha: "1.0.0", beta: "1.0.0" } });
  const alpha = packageAt(root, "alpha", { dependencies: { shared: "1.0.0" } });
  const beta = packageAt(root, "beta", { dependencies: { shared: "1.0.0" } });
  packageAt(alpha, "shared");
  const duplicate = packageAt(beta, "shared");
  assert.equal(productionLicenseNotices(join(root, "package.json")).match(/Package: shared@/g).length, 1);
  writeFileSync(join(duplicate, "LICENSE"), "Different copyright and terms.\n");
  assert.throws(() => productionLicenseNotices(join(root, "package.json")), /Conflicting upstream/);
});

test("rejects missing packages, missing or empty license text, and malformed metadata", t => {
  const root = fixture(t, { dependencies: { alpha: "1.0.0" } });
  const manifest = join(root, "package.json");
  assert.throws(() => productionLicenseNotices(manifest), /Cannot locate dependency/);
  const directory = packageAt(root, "alpha", {}, null);
  assert.throws(() => productionLicenseNotices(manifest), /Missing upstream license/);
  writeFileSync(join(directory, "LICENSE"), " \n");
  assert.throws(() => productionLicenseNotices(manifest), /Empty notice/);
  writeFileSync(join(directory, "package.json"), '{"name": "alpha"}');
  assert.throws(() => productionLicenseNotices(manifest), /lacks a name or version/);
});

test("does not source package notices from outside the selected project", t => {
  const root = fixture(t, { dependencies: { alpha: "1.0.0" } });
  const outside = fixture(t);
  const directory = packageAt(outside, "alpha");
  mkdirSync(join(root, "node_modules"));
  symlinkSync(directory, join(root, "node_modules/alpha"), "dir");
  assert.throws(() => productionLicenseNotices(join(root, "package.json")), /escapes the project/);
});

test("requires exact versions and verified text for upstream supplements", t => {
  const root = fixture(t, { dependencies: { alpha: "1.0.0" } });
  const directory = packageAt(root, "alpha", {}, null);
  const licenses = join(root, "licenses");
  mkdirSync(licenses);
  const files = ["LICENSE", "NOTICE"].map(file => {
    const content = `${file}: upstream copyright and terms.\n`;
    writeFileSync(join(licenses, file), content);
    return {
      file, source: `https://example.invalid/commit/${file}`,
      sha256: createHash("sha256").update(content).digest("hex"),
    };
  });
  writeFileSync(join(licenses, "supplements.json"), JSON.stringify([
    { packages: ["alpha@1.0.0"], files },
  ]));
  const generate = () => productionLicenseNotices(join(root, "package.json"));
  assert.ok(generate().includes("NOTICE: upstream copyright and terms."));
  writeFileSync(join(licenses, "LICENSE"), "Changed terms\n");
  assert.throws(generate, /checksum mismatch/);
  writeFileSync(join(directory, "package.json"), JSON.stringify({ name: "alpha", version: "2.0.0" }));
  assert.throws(generate, /Missing upstream license file or exact supplement/);
  writeFileSync(join(licenses, "supplements.json"), JSON.stringify([
    { packages: "alpha@2.0.00", files },
  ]));
  assert.throws(generate, /Missing upstream license file or exact supplement/);
});

test("build plugin emits the same notice as the standalone generator", t => {
  const root = fixture(t, { dependencies: { alpha: "1.0.0" } });
  packageAt(root, "alpha");
  const manifest = join(root, "package.json");
  let emitted;
  const plugin = productionLicenses(manifest);
  assert.equal(plugin.apply, "build");
  plugin.generateBundle.call({ emitFile: asset => { emitted = asset; } });
  assert.deepEqual(emitted, {
    type: "asset", fileName: "THIRD_PARTY_LICENSES.txt",
    source: productionLicenseNotices(manifest),
  });
});

test("platform-only optional packages require their pinned license when installed", t => {
  const root = fixture(t, { dependencies: { tabster: "8.7.0" } });
  const nativeName = "@rollup/rollup-linux-x64-gnu";
  packageAt(root, "tabster", {
    version: "8.7.0", optionalDependencies: { [nativeName]: "4.53.3" },
  });
  const generate = () => productionLicenseNotices(join(root, "package.json"));
  assert.ok(!generate().includes(nativeName));

  packageAt(root, nativeName, { version: "4.53.3", os: ["linux"], cpu: ["x64"] }, null);
  assert.throws(generate, /Missing upstream license file or exact supplement/);
  const committed = fileURLToPath(new URL("../licenses/", import.meta.url));
  const entry = JSON.parse(readFileSync(join(committed, "supplements.json"), "utf8"))
    .find(item => item.packages.includes(`${nativeName}@4.53.3`));
  assert.ok(entry, "Keep the supported x64 package supplement");
  mkdirSync(join(root, "licenses"));
  writeFileSync(join(root, "licenses/supplements.json"), JSON.stringify([entry]));
  for (const file of entry.files) {
    writeFileSync(join(root, "licenses", file.file), readFileSync(join(committed, file.file)));
  }
  const output = generate();
  assert.ok(output.includes(`Package: ${nativeName}@4.53.3`));
  assert.ok(output.includes("# Licenses of bundled dependencies"));
});

test("CLI supports help and nested output and never overwrites output after a failed scan", t => {
  const root = fixture(t, { dependencies: { alpha: "1.0.0" } });
  const output = join(root, "output/notices/THIRD_PARTY_LICENSES.txt");
  const run = (...args) => spawnSync(process.execPath, [script, ...args], {
    cwd: tmpdir(), encoding: "utf8",
  });
  assert.equal(run("--help").status, 0);
  assert.notEqual(run("--unknown").status, 0);
  assert.notEqual(run("--root", join(root, "missing")).status, 0);
  mkdirSync(dirname(output), { recursive: true });
  writeFileSync(output, "Existing notice\n");
  assert.notEqual(run("--root", root, "--output", output).status, 0);
  assert.equal(readFileSync(output, "utf8"), "Existing notice\n");
  packageAt(root, "alpha");
  assert.equal(run("--root", root, "--output", output).status, 0);
  assert.equal(readFileSync(output, "utf8"), productionLicenseNotices(join(root, "package.json")));
  const another = join(root, "new/nested/NOTICE");
  assert.equal(run("--root", root, "--output", another).status, 0);
  assert.equal(readFileSync(another, "utf8"), readFileSync(output, "utf8"));
});
