// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

const supplementPath = "app/cetirun-portal/web/licenses/supplements.json";
const identityPattern = /^((?:@[a-z0-9._-]+\/)?[a-z0-9._-]+)@(\d+\.\d+\.\d+(?:-[\w.-]+)?(?:\+[\w.-]+)?)$/;
const sourcePattern = /^https:\/\/raw\.githubusercontent\.com\/([a-zA-Z0-9][\w.-]*\/[a-zA-Z0-9][\w.-]*)\/([a-f0-9]{40})\/([\w./-]+)$/;
const attributionPattern = /^(licen[cs]e|notice|copying|authors|contributors)(?:[.-]|$)/i;
const normalize = text => text.replace(/\r\n/g, "\n").trimEnd();
const digest = data => createHash("sha256").update(data).digest("hex");

function isWithin(root, file) {
  const child = relative(root, file);
  return child !== ".." && !child.startsWith(`..${sep}`) && !isAbsolute(child);
}

function policySignature(files) {
  return JSON.stringify(files.map(({ file, sha256, repository, path }) => ({ file, sha256, repository, path })));
}

function plan(root) {
  const before = readFileSync(join(root, "licenses/supplements.json"), "utf8");
  const supplements = JSON.parse(before);
  const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8"));
  if (!Array.isArray(supplements) || lock.lockfileVersion !== 3 || !lock.packages) {
    throw new Error("Expected license supplements and an npm v3 lockfile");
  }
  const policies = new Map();
  const covered = new Set();
  for (const entry of supplements) {
    if (!Array.isArray(entry.packages) || !entry.packages.length || !Array.isArray(entry.files) || !entry.files.length) {
      throw new Error("Invalid license supplement");
    }
    const files = entry.files.map(file => {
      const source = typeof file.source === "string" && file.source.match(sourcePattern);
      if (!source || source[3].split("/").some(part => !part || part === "." || part === "..") ||
          typeof file.file !== "string" || !/^[\w-][\w.-]*$/.test(file.file) || !/^[a-f0-9]{64}$/.test(file.sha256)) {
        throw new Error("Supplements need safe filenames and immutable GitHub source URLs");
      }
      const directory = join(root, "licenses");
      const local = realpathSync(join(directory, file.file));
      if (!isWithin(directory, local)) throw new Error("License file escapes the project");
      const data = readFileSync(local);
      if (digest(data) !== file.sha256 || !data.toString("utf8").trim()) {
        throw new Error(`License supplement checksum or content mismatch: ${file.file}`);
      }
      return { ...file, repository: source[1], path: source[3], text: data.toString("utf8") };
    });
    if (new Set(files.map(file => file.repository)).size !== 1) {
      throw new Error("A package supplement must have one upstream repository");
    }
    for (const identity of entry.packages) {
      const match = typeof identity === "string" && identity.match(identityPattern);
      if (!match || covered.has(identity)) throw new Error(`Invalid or duplicate supplement: ${identity}`);
      covered.add(identity);
      const previous = policies.get(match[1]);
      if (previous && policySignature(previous) !== policySignature(files)) {
        throw new Error(`Multiple source policies need manual review: ${match[1]}`);
      }
      policies.set(match[1], files);
    }
  }
  const versions = new Map();
  for (const [path, pkg] of Object.entries(lock.packages)) {
    if (!path.includes("node_modules/") || pkg.link || pkg.dev) continue;
    const name = pkg.name || path.split("node_modules/").at(-1);
    if (!policies.has(name)) continue;
    const identity = `${name}@${pkg.version}`;
    if (!identityPattern.test(identity) || typeof pkg.integrity !== "string" || !pkg.integrity.startsWith("sha512-")) {
      throw new Error(`Missing exact version or integrity: ${identity}`);
    }
    if (versions.has(identity) && versions.get(identity).integrity !== pkg.integrity) {
      throw new Error(`Conflicting lockfile integrity: ${identity}`);
    }
    versions.set(identity, { ...pkg, name });
  }
  const pending = [...versions].filter(([identity]) => !covered.has(identity)).sort(([a], [b]) => a.localeCompare(b));
  return { before, supplements, policies, versions, pending };
}

async function readURL(url, fetcher) {
  let response;
  try {
    response = await fetcher(url, { redirect: "error", signal: AbortSignal.timeout(15000) });
  } catch (error) {
    throw new Error(`Upstream request failed: ${url}: ${error.message}`);
  }
  if (!response.ok) throw new Error(`Upstream request failed (${response.status}): ${url}`);
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.length;
      if (size > 2 * 1024 * 1024) throw new Error(`Upstream response is too large: ${url}`);
      chunks.push(value);
    }
  } finally {
    await reader.cancel();
  }
  return new TextDecoder("utf8", { fatal: true }).decode(Buffer.concat(chunks));
}

export function supplementPatch(before, after) {
  const lines = (text, prefix) => {
    const parts = text.split("\n");
    if (text.endsWith("\n")) parts.pop();
    return { count: parts.length, text: parts.map(line => prefix + line).join("\n") +
      (text.endsWith("\n") ? "\n" : "\n\\ No newline at end of file\n") };
  };
  const oldLines = lines(before, "-");
  const newLines = lines(after, "+");
  return `diff --git a/${supplementPath} b/${supplementPath}\n` +
    `--- a/${supplementPath}\n+++ b/${supplementPath}\n` +
    `@@ -1,${oldLines.count} +1,${newLines.count} @@\n${oldLines.text}${newLines.text}`;
}

export async function licenseUpdate(root, fetcher = fetch) {
  root = realpathSync(root);
  const { before, supplements, policies, versions, pending } = plan(root);
  if (!pending.length) return { patch: "", updates: [] };
  const cache = new Map();
  const read = url => {
    if (!cache.has(url)) cache.set(url, readURL(url, fetcher));
    return cache.get(url);
  };
  const additions = [];
  for (const [identity, pkg] of pending) {
    const files = policies.get(pkg.name);
    const repository = files[0].repository;
    const metadata = JSON.parse(await read(`https://registry.npmjs.org/${encodeURIComponent(pkg.name)}/${encodeURIComponent(pkg.version)}`));
    const previousIdentity = supplements.flatMap(entry => entry.packages).find(value => value.match(identityPattern)[1] === pkg.name);
    const previousVersion = previousIdentity.match(identityPattern)[2];
    const previous = JSON.parse(await read(`https://registry.npmjs.org/${encodeURIComponent(pkg.name)}/${encodeURIComponent(previousVersion)}`));
    const repositoryURL = typeof metadata.repository === "string" ? metadata.repository : metadata.repository?.url;
    const expected = `https://github.com/${repository}`.toLowerCase();
    if (typeof repositoryURL !== "string" || repositoryURL.replace(/^git\+/, "").replace(/\.git$/, "").toLowerCase() !== expected ||
        metadata.name !== pkg.name || metadata.version !== pkg.version || !/^[a-f0-9]{40}$/.test(metadata.gitHead || "") ||
        metadata.dist?.integrity !== pkg.integrity || typeof pkg.license !== "string" || metadata.license !== pkg.license ||
        previous.name !== pkg.name || previous.version !== previousVersion || previous.license !== metadata.license) {
      throw new Error(`Upstream identity, repository, gitHead, license or integrity needs manual review: ${identity}`);
    }
    const byDirectory = Map.groupBy(files, file => dirname(file.path));
    for (const [directory, expectedFiles] of byDirectory) {
      const suffix = directory === "." ? "" : "/" + directory;
      const inventory = JSON.parse(await read(`https://api.github.com/repos/${repository}/contents${suffix}?ref=${metadata.gitHead}`));
      if (!Array.isArray(inventory)) throw new Error(`Cannot inspect upstream attribution files: ${identity}`);
      const found = inventory.filter(file => file.type === "file" && attributionPattern.test(file.name)).map(file => file.path).sort();
      if (JSON.stringify(found) !== JSON.stringify(expectedFiles.map(file => file.path).sort())) {
        throw new Error(`Upstream attribution file inventory changed; manual review required: ${identity}`);
      }
    }
    const proposed = [];
    for (const { file, sha256, path, text } of files) {
      const source = `https://raw.githubusercontent.com/${repository}/${metadata.gitHead}/${path}`;
      const upstream = await read(source);
      if (!upstream.trim() || normalize(upstream) !== normalize(text)) {
        throw new Error(`Upstream ${path} changed; manual review required: ${identity}`);
      }
      proposed.push({ file, sha256, source });
    }
    additions.push({ packages: [identity], files: proposed });
  }
  const changedNames = new Set(pending.map(([, pkg]) => pkg.name));
  const retained = supplements.map(entry => ({
    ...entry,
    packages: entry.packages.filter(identity => !changedNames.has(identity.match(identityPattern)[1]) || versions.has(identity)),
  })).filter(entry => entry.packages.length);
  return {
    patch: supplementPatch(before, JSON.stringify([...retained, ...additions], null, 2) + "\n"),
    updates: pending.map(([identity]) => identity),
  };
}

async function main() {
  const { values } = parseArgs({ options: {
    root: { type: "string", default: fileURLToPath(new URL("..", import.meta.url)) },
    output: { type: "string" },
    help: { type: "boolean" },
  } });
  if (values.help) {
    console.log("Usage: node scripts/update-license-supplements.mjs --output PATCH [--root WEB_DIR]");
    console.log("Read public upstream metadata for uncovered versions of known supplements. Never change source files.");
    console.log("Exit 0: covered; exit 1: review needed or error. A patch is written only for unchanged attribution text.");
    return;
  }
  const root = realpathSync(values.root);
  if (!values.output) throw new Error("--output is required");
  const output = resolve(values.output);
  let ancestor = dirname(output);
  while (!existsSync(ancestor)) ancestor = dirname(ancestor);
  const physicalOutput = resolve(realpathSync(ancestor), relative(ancestor, output));
  if (isWithin(root, physicalOutput) || existsSync(output)) throw new Error("Use a new output path outside the web directory");
  const result = await licenseUpdate(root);
  if (!result.patch) {
    console.log("Known license supplements cover the locked versions; no upstream requests needed.");
    return;
  }
  mkdirSync(dirname(output), { recursive: true });
  writeFileSync(output, result.patch, { flag: "wx" });
  console.error(`Review version-pinned supplements for ${result.updates.join(", ")}. Patch: ${output}`);
  process.exitCode = 1;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(error => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
