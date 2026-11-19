// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { licenseUpdate, supplementPatch } from "./update-license-supplements.mjs";
import { productionLicenseNotices } from "./production-licenses.mjs";

const script = fileURLToPath(new URL("./update-license-supplements.mjs", import.meta.url));
const name = "@example/icons";
const oldCommit = "1".repeat(40);
const newCommit = "2".repeat(40);
const integrity = `sha512-${Buffer.alloc(64, 5).toString("base64")}`;
const license = "Copyright Example Authors\nPermission to use this fixture.\n";
const notice = "Additional fixture attribution.\n";

function fixture(t, version = "1.0.1") {
  const repo = mkdtempSync(join(tmpdir(), "cetirun-license-update-"));
  t.after(() => rmSync(repo, { recursive: true, force: true }));
  const root = join(repo, "app/cetirun-portal/web");
  mkdirSync(join(root, "licenses"), { recursive: true });
  const files = [["LICENSE", license], ["NOTICE", notice]].map(([file, text]) => {
    writeFileSync(join(root, "licenses", file), text);
    return { file, sha256: createHash("sha256").update(text).digest("hex"),
      source: `https://raw.githubusercontent.com/example/icons/${oldCommit}/${file}` };
  });
  const supplements = [{ packages: [`${name}@1.0.0`], files }];
  const lock = { lockfileVersion: 3, packages: {
    "": { name: "fixture", version: "1.0.0" },
    [`node_modules/${name}`]: { version, license: "MIT", integrity },
  } };
  const save = () => {
    writeFileSync(join(root, "licenses/supplements.json"), JSON.stringify(supplements, null, 2) + "\n");
    writeFileSync(join(root, "package-lock.json"), JSON.stringify(lock));
  };
  save();
  const metadata = { name, version, license: "MIT", gitHead: newCommit,
    repository: { url: "git+https://github.com/example/icons.git" }, dist: { integrity } };
  const responses = new Map([
    [`https://registry.npmjs.org/${encodeURIComponent(name)}/${version}`, metadata],
    [`https://registry.npmjs.org/${encodeURIComponent(name)}/1.0.0`, { name, version: "1.0.0", license: "MIT" }],
    [`https://api.github.com/repos/example/icons/contents?ref=${newCommit}`, files.map(({ file }) => ({ type: "file", name: file, path: file }))],
    [`https://raw.githubusercontent.com/example/icons/${newCommit}/LICENSE`, license],
    [`https://raw.githubusercontent.com/example/icons/${newCommit}/NOTICE`, notice.trimEnd()],
  ]);
  const requests = [];
  const fetcher = async (url, options) => {
    requests.push(url);
    assert.equal(options.redirect, "error");
    assert.ok(options.signal instanceof AbortSignal);
    assert.ok(responses.has(url), `Unexpected network request: ${url}`);
    const value = responses.get(url);
    if (value instanceof Response) return value;
    const body = typeof value === "string" ? value : JSON.stringify(value);
    return new Response(body);
  };
  return { repo, root, supplements, lock, save, metadata, responses, requests, fetcher };
}

test("proposes an applicable exact-version patch without changing source or running dependencies", async t => {
  const f = fixture(t);
  writeFileSync(join(f.root, "package.json"), JSON.stringify({ name: "fixture", version: "1.0.0", dependencies: { [name]: "1.0.1" } }));
  const packageDir = join(f.root, "node_modules", name);
  mkdirSync(packageDir, { recursive: true });
  writeFileSync(join(packageDir, "package.json"), JSON.stringify({ name, version: "1.0.1", license: "MIT" }));
  writeFileSync(join(packageDir, "index.js"), 'throw new Error("Do not execute dependency code");');
  const file = join(f.root, "licenses/supplements.json");
  const before = readFileSync(file, "utf8");
  assert.throws(() => productionLicenseNotices(join(f.root, "package.json")), /exact supplement/);
  const result = await licenseUpdate(f.root, f.fetcher);
  assert.deepEqual(result.updates, [`${name}@1.0.1`]);
  assert.equal(readFileSync(file, "utf8"), before);
  const check = spawnSync("git", ["apply", "--check", "-"], { cwd: f.repo, input: result.patch, encoding: "utf8" });
  assert.equal(check.status, 0, check.stderr);
  const apply = spawnSync("git", ["apply", "-"], { cwd: f.repo, input: result.patch, encoding: "utf8" });
  assert.equal(apply.status, 0, apply.stderr);
  const updated = JSON.parse(readFileSync(file, "utf8"));
  assert.deepEqual(updated[0].packages, [`${name}@1.0.1`]);
  assert.ok(updated[0].files.every(item => item.source.includes(newCommit)));
  assert.ok(productionLicenseNotices(join(f.root, "package.json")).includes(`${name}@1.0.1`));
  const again = await licenseUpdate(f.root, () => { throw new Error("No network expected"); });
  assert.equal(again.patch, "");
});

test("covered versions and unknown packages require no network or automatic approval", async t => {
  const f = fixture(t, "1.0.0");
  f.lock.packages["node_modules/unknown"] = { version: "2.0.0" };
  f.save();
  assert.deepEqual(await licenseUpdate(f.root, f.fetcher), { patch: "", updates: [] });
  assert.equal(f.requests.length, 0);
});

test("retains an older version that remains in the lockfile and checks platform-only entries", async t => {
  const f = fixture(t);
  f.lock.packages[`node_modules/consumer/node_modules/${name}`] = { version: "1.0.0", license: "MIT", integrity };
  Object.assign(f.lock.packages[`node_modules/${name}`], { optional: true, cpu: ["x64"], os: ["linux"] });
  f.save();
  const result = await licenseUpdate(f.root, f.fetcher);
  assert.equal(spawnSync("git", ["apply", "-"], { cwd: f.repo, input: result.patch }).status, 0);
  const updated = JSON.parse(readFileSync(join(f.root, "licenses/supplements.json"), "utf8"));
  assert.deepEqual(updated.flatMap(item => item.packages), [`${name}@1.0.0`, `${name}@1.0.1`]);
});

test("does not require production supplements for development-only packages", async t => {
  const f = fixture(t);
  f.lock.packages[`node_modules/${name}`].dev = true;
  f.save();
  assert.deepEqual(await licenseUpdate(f.root, f.fetcher), { patch: "", updates: [] });
  assert.equal(f.requests.length, 0);
});

test("validates all local policy and lock data before network access", async t => {
  const cases = [
    ["mutable source", f => { f.supplements[0].files[0].source = "https://raw.githubusercontent.com/example/icons/main/LICENSE"; }],
    ["unknown source", f => { f.supplements[0].files[0].source = "https://example.invalid/LICENSE"; }],
    ["path traversal", f => { f.supplements[0].files[0].file = "../LICENSE"; }],
    ["source traversal", f => { f.supplements[0].files[0].source = `https://raw.githubusercontent.com/example/icons/${oldCommit}/../LICENSE`; }],
    ["checksum mismatch", f => { f.supplements[0].files[0].sha256 = "0".repeat(64); }],
    ["duplicate approval", f => { f.supplements.push(structuredClone(f.supplements[0])); }],
    ["missing integrity", f => { delete f.lock.packages[`node_modules/${name}`].integrity; }],
    ["conflicting integrity", f => { f.lock.packages[`node_modules/consumer/node_modules/${name}`] = { version: "1.0.1", integrity: "sha512-other" }; }],
  ];
  for (const [label, change] of cases) {
    await t.test(label, async t => {
      const f = fixture(t);
      change(f);
      f.save();
      await assert.rejects(licenseUpdate(f.root, f.fetcher));
      assert.equal(f.requests.length, 0);
    });
  }
});

test("rejects symlinked license files outside the project", async t => {
  const f = fixture(t);
  const outside = join(f.repo, "outside.LICENSE");
  writeFileSync(outside, license);
  f.supplements[0].files[0].file = "linked.LICENSE";
  symlinkSync(outside, join(f.root, "licenses/linked.LICENSE"));
  f.save();
  await assert.rejects(licenseUpdate(f.root, f.fetcher), /escapes/);
  assert.equal(f.requests.length, 0);
});

test("requires matching package provenance and license declarations", async t => {
  for (const [field, value] of [
    ["name", "other"], ["version", "9.0.0"], ["repository", "https://github.com/other/repository"],
    ["gitHead", "main"], ["dist", { integrity: "sha512-other" }], ["license", "Apache-2.0"],
  ]) {
    await t.test(field, async t => {
      const f = fixture(t);
      f.metadata[field] = value;
      await assert.rejects(licenseUpdate(f.root, f.fetcher), /manual review/);
      assert.ok(f.requests.every(url => url.startsWith("https://registry.npmjs.org/")));
    });
  }
  const f = fixture(t);
  f.metadata.license = "Apache-2.0";
  f.lock.packages[`node_modules/${name}`].license = "Apache-2.0";
  f.save();
  await assert.rejects(licenseUpdate(f.root, f.fetcher), /manual review/);
});

test("requires review for changed attribution text or newly added upstream notices", async t => {
  const changed = fixture(t);
  changed.responses.set(`https://raw.githubusercontent.com/example/icons/${newCommit}/NOTICE`, "Different attribution.");
  await assert.rejects(licenseUpdate(changed.root, changed.fetcher), /NOTICE changed/);
  const added = fixture(t);
  added.responses.get(`https://api.github.com/repos/example/icons/contents?ref=${newCommit}`).push({ type: "file", name: "COPYING", path: "COPYING" });
  await assert.rejects(licenseUpdate(added.root, added.fetcher), /inventory changed/);
});

test("fails closed on upstream errors and oversized responses", async t => {
  for (const response of [new Response("missing", { status: 404 }), new Response("x".repeat(2 * 1024 * 1024 + 1))]) {
    const f = fixture(t);
    f.responses.set(`https://registry.npmjs.org/${encodeURIComponent(name)}/1.0.1`, response);
    await assert.rejects(licenseUpdate(f.root, f.fetcher), /Upstream/);
  }
});

test("patches JSON without a final newline", t => {
  const f = fixture(t, "1.0.0");
  const file = join(f.root, "licenses/supplements.json");
  const before = readFileSync(file, "utf8").trimEnd();
  writeFileSync(file, before);
  const patch = supplementPatch(before, before + "\n");
  const result = spawnSync("git", ["apply", "--check", "-"], { cwd: f.repo, input: patch, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
});

test("CLI validates input, preserves existing output, and stays offline for covered versions", t => {
  const f = fixture(t, "1.0.0");
  const run = (...args) => spawnSync(process.execPath, [script, ...args], { cwd: tmpdir(), encoding: "utf8" });
  assert.equal(run("--help").status, 0);
  assert.notEqual(run("--unknown").status, 0);
  assert.notEqual(run("--root", f.root).status, 0);
  assert.notEqual(run("--root", join(f.root, "missing"), "--output", join(f.repo, "patch")).status, 0);
  const existing = join(f.repo, "existing.patch");
  writeFileSync(existing, "preserve me");
  assert.notEqual(run("--root", f.root, "--output", existing).status, 0);
  assert.equal(readFileSync(existing, "utf8"), "preserve me");
  assert.notEqual(run("--root", f.root, "--output", join(f.root, "new.patch")).status, 0);
  symlinkSync(f.root, join(f.repo, "linked-web"), "dir");
  assert.notEqual(run("--root", f.root, "--output", join(f.repo, "linked-web/new/patch")).status, 0);
  const output = join(f.repo, "new/nested/patch");
  assert.equal(run("--root", f.root, "--output", output).status, 0);
  assert.throws(() => readFileSync(output), /ENOENT/);
});
