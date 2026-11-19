// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { createHash } from "node:crypto";

function readPackage(path) {
  const metadata = JSON.parse(readFileSync(path, "utf8"));
  if (!metadata.name || typeof metadata.name !== "string" ||
      !metadata.version || typeof metadata.version !== "string") {
    throw new Error(`Package metadata lacks a name or version: ${path}`);
  }
  return metadata;
}

function isWithin(root, path) {
  const child = relative(root, path);
  return child !== ".." && !child.startsWith(`..${sep}`) && !isAbsolute(child);
}

function resolvePackage(name, parentManifest, root, optional = false) {
  if (!/^(?:@[^/.\s]+\/)?[^/.\s][^/\s]*$/.test(name)) {
    throw new Error(`Invalid dependency name: ${name}`);
  }
  const require = createRequire(parentManifest);
  // Resolve metadata without importing or executing dependency code. Search
  // paths also handle packages whose exports hide their package.json.
  for (const directory of require.resolve.paths(name) || []) {
    const manifest = join(directory, name, "package.json");
    if (!isWithin(root, manifest) || !existsSync(manifest)) continue;
    const resolved = realpathSync(manifest);
    if (!isWithin(root, resolved)) throw new Error(`Dependency escapes the project: ${name}`);
    return resolved;
  }
  if (optional) return null;
  throw new Error(`Cannot locate dependency ${name} from ${parentManifest}`);
}

function dependencies(metadata) {
  const optional = new Set(Object.keys(metadata.optionalDependencies || {}));
  const required = new Set(Object.keys(metadata.dependencies || {}).filter(name => !optional.has(name)));
  for (const name of Object.keys(metadata.peerDependencies || {})) {
    if (metadata.peerDependenciesMeta?.[name]?.optional) optional.add(name);
    else required.add(name);
  }
  return [...new Set([...required, ...optional])].sort()
    .map(name => [name, optional.has(name) && !required.has(name)]);
}

export function productionLicenseNotices(rootManifest) {
  rootManifest = realpathSync(rootManifest);
  const root = dirname(rootManifest);
  const supplementFile = join(root, "licenses/supplements.json");
  const supplements = existsSync(supplementFile)
    ? JSON.parse(readFileSync(supplementFile, "utf8")) : [];
  if (!Array.isArray(supplements)) throw new Error("License supplements must be an array");
  const packages = new Map();
  const visited = new Set();

  function supplement(identity) {
    const matches = supplements.filter(entry =>
      Array.isArray(entry?.packages) && entry.packages.includes(identity));
    if (matches.length !== 1) throw new Error(`Missing upstream license file or exact supplement for ${identity}`);
    if (!Array.isArray(matches[0].files) || !matches[0].files.length) {
      throw new Error(`Invalid license supplement for ${identity}`);
    }
    return matches[0].files.map(entry => {
      if (typeof entry.file !== "string" || !/^[\w.-]+$/.test(entry.file) ||
          !/^[a-f0-9]{64}$/.test(entry.sha256) ||
          typeof entry.source !== "string" || !entry.source.startsWith("https://")) {
        throw new Error(`Invalid license supplement for ${identity}`);
      }
      const directory = join(root, "licenses");
      const file = realpathSync(join(directory, entry.file));
      if (!isWithin(directory, file)) throw new Error(`License supplement escapes the project: ${identity}`);
      const data = readFileSync(file);
      if (createHash("sha256").update(data).digest("hex") !== entry.sha256) {
        throw new Error(`License supplement checksum mismatch for ${identity}`);
      }
      const text = data.toString("utf8").replace(/\r\n/g, "\n").trimEnd();
      if (!text) throw new Error(`Empty license supplement for ${identity}`);
      return `Source: ${entry.source}\n\n--- ${entry.file} (version-pinned supplement) ---\n${text}`;
    }).join("\n\n");
  }

  function visit(manifest) {
    if (visited.has(manifest)) return;
    visited.add(manifest);
    const metadata = readPackage(manifest);
    const directory = dirname(manifest);
    const files = readdirSync(directory, { withFileTypes: true })
      .filter(entry => entry.isFile() && /^(licen[cs]e|notice|copying)(?:[.-]|$)/i.test(entry.name))
      .map(entry => entry.name).sort();
    const identity = `${metadata.name}@${metadata.version}`;
    const hasLicense = files.some(name => /^(licen[cs]e|copying)(?:[.-]|$)/i.test(name));
    const notices = [
      `Package: ${identity}`,
      `License: ${metadata.license || "See upstream license below"}`,
      ...files.map(name => {
        const text = readFileSync(join(directory, name), "utf8");
        if (!text.trim()) throw new Error(`Empty notice file for ${identity}: ${name}`);
        return `\n--- ${name} ---\n${text.replace(/\r\n/g, "\n").trimEnd()}`;
      }),
      ...(!hasLicense ? [supplement(identity)] : []),
    ].join("\n");
    if (packages.has(identity) && packages.get(identity) !== notices) {
      throw new Error(`Conflicting upstream license notices for ${identity}`);
    }
    packages.set(identity, notices);
    visitDependencies(metadata, manifest);
  }

  function visitDependencies(metadata, manifest) {
    for (const [name, optional] of dependencies(metadata)) {
      const dependency = resolvePackage(name, manifest, root, optional);
      if (dependency) visit(dependency);
    }
  }

  visitDependencies(readPackage(rootManifest), rootManifest);
  return [
    "CetiRun Portal - third-party JavaScript dependency notices",
    "Generated from installed production dependencies, including transitive, peer, and installed optional dependencies.",
    "This conservative inventory can include packages removed by tree shaking. It is not a whole-image notice or SBOM.",
    "Upstream copyright and license text follows; line endings are normalized.",
    ...[...packages.keys()].sort().map(name => packages.get(name)),
    "",
  ].join("\n\n").trimEnd() + "\n";
}

export function productionLicenses(rootManifest) {
  return {
    name: "cetirun-production-license-notices",
    apply: "build",
    generateBundle() {
      this.emitFile({
        type: "asset",
        fileName: "THIRD_PARTY_LICENSES.txt",
        source: productionLicenseNotices(rootManifest),
      });
    },
  };
}

function main() {
  const { values } = parseArgs({
    options: {
      root: { type: "string", default: fileURLToPath(new URL("..", import.meta.url)) },
      output: { type: "string" },
      help: { type: "boolean" },
    },
  });
  if (values.help) {
    console.log("Usage: node scripts/production-licenses.mjs [--root WEB_DIR] [--output FILE]");
    console.log("Read installed production dependencies without network access or executing their code.");
    console.log("Default root: the web directory containing this script. Default output: stdout.");
    return;
  }
  const notice = productionLicenseNotices(join(resolve(values.root), "package.json"));
  if (values.output) {
    const output = resolve(values.output);
    mkdirSync(dirname(output), { recursive: true });
    writeFileSync(output, notice);
  } else {
    process.stdout.write(notice);
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main();
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
