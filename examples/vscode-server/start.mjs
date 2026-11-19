// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { spawn } from "node:child_process";
import { lstat, mkdir, readFile, realpath, symlink, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { createProxy, validateConfig } from "./proxy.mjs";
import gateway from "./gateway-extension/gateway.cjs";

async function linkOnce(target, link) {
  try { await symlink(target, link); }
  catch (error) {
    if (error.code !== "EEXIST" || await realpath(link) !== await realpath(target)) {
      throw new Error(`Existing path conflicts with the VS Code bootstrap: ${link}`);
    }
  }
}

export async function prepareHome(home, install, commit, env = process.env) {
  const context = path.join(home, ".agents");
  if (!await readFile(path.join(context, "AGENTS.md"), "utf8") ||
      !await readFile(path.join(context, "skills/cetirun-memory/SKILL.md"), "utf8")) throw new Error("Platform context is missing");
  const cliData = path.join(home, ".cetirun-vscode", "cli");
  const data = path.join(home, ".vscode-server");
  const instructions = path.join(home, ".cetirun-vscode", "instructions");
  for (const directory of [path.join(cliData, "serve-web"), path.join(data, "data/Machine"),
    instructions, path.join(home, ".copilot"), path.join(home, "workspace")]) await mkdir(directory, { recursive: true, mode: 0o700 });
  await linkOnce(path.join(install, "server"), path.join(cliData, "serve-web", commit));
  await linkOnce(path.join(context, "AGENTS.md"), path.join(home, ".copilot/copilot-instructions.md"));
  const localInstructions = `---\nname: CetiRun\napplyTo: '**'\n---\n\nFollow the [platform instructions](${context}/AGENTS.md).\n`;
  try { await writeFile(path.join(instructions, "cetirun.instructions.md"), localInstructions, { flag: "wx", mode: 0o600 }); }
  catch (error) { if (error.code !== "EEXIST") throw error; }
  const settings = { "telemetry.telemetryLevel": "off", "extensions.autoUpdate": false,
    "update.mode": "none", "chat.disableAIFeatures": false, "remote.autoForwardPorts": false,
    "chat.instructionsFilesLocations": { ".github/instructions": true, [instructions]: true },
    "chat.includeReferencedInstructions": true };
  try { await writeFile(path.join(data, "data/Machine/settings.json"), JSON.stringify(settings, null, 2) + "\n", { flag: "wx", mode: 0o600 }); }
  catch (error) { if (error.code !== "EEXIST") throw error; }
  const workspace = path.join(home, "workspace");
  if (env.CETIRUN_MEMORY_ASSETS) {
    try { await configureMemoryTools(workspace, env); }
    catch { console.warn("Memory MCP configuration unavailable; existing workspace files were preserved."); }
  }
  return { cliData, data, settings, workspace };
}

// Keep VS Code's normal server/workspace trust prompt. No allow-all approval
// setting is written, and an existing user configuration is never overwritten.
export async function configureMemoryTools(workspace, env) {
  if (env.CETIRUN_MEMORY_ASSETS !== "/opt/cetirun-memory") throw new Error("Memory asset binding is invalid");
  const directory = path.join(workspace, ".vscode");
  try { const info = await lstat(directory); if (!info.isDirectory() || info.isSymbolicLink()) throw new Error("Memory MCP configuration conflicts with the workspace"); }
  catch (error) { if (error.code !== "ENOENT") throw error; await mkdir(directory, { mode: 0o700 }); }
  const configuration = { servers: { "cetirun-memory": { type: "stdio", command: "/opt/cetirun-memory/bin/cetirun-memory", args: ["mcp", "--stdio", "--owner-private"], cwd: "${workspaceFolder}" } } };
  const target = path.join(directory, "mcp.json");
  try { await writeFile(target, JSON.stringify(configuration, null, 2) + "\n", { flag: "wx", mode: 0o600 }); }
  catch (error) {
    if (error.code !== "EEXIST") throw error;
    const info = await lstat(target);
    if (info.isSymbolicLink() || !info.isFile() || info.size > 65536) throw new Error("Memory MCP configuration conflicts with the workspace");
    const existing = JSON.parse(await readFile(target, "utf8"));
    if (JSON.stringify(existing.servers?.["cetirun-memory"]) !== JSON.stringify(configuration.servers["cetirun-memory"])) {
      throw new Error("Existing MCP configuration is preserved. Add the platform cetirun-memory stdio server explicitly before enabling Memory in this workspace.");
    }
  }
}

export async function main() {
  if (process.argv.includes("--help")) {
    console.log("Start the official VS Code Web example. Requires HOME, PORT, CETIRUN_WEB_BASE_PATH and CETIRUN_WEB_INSTANCE. The image builder must accept the VS Code Server license.");
    return;
  }
  if (process.argv.length > 2) throw new Error("Unknown arguments; use --help");
  process.umask(0o077);
  const config = { port: Number(process.env.PORT), basePath: process.env.CETIRUN_WEB_BASE_PATH ?? "",
    instance: process.env.CETIRUN_WEB_INSTANCE ?? "" };
  validateConfig(config);
  if (process.env.VSCODE_SERVER_LICENSE_ACCEPTED !== "true") throw new Error("VS Code Server license acceptance is required");
  const home = process.env.HOME;
  if (!home || !path.isAbsolute(home) || home === "/") throw new Error("An absolute instance HOME is required");
  gateway.gatewayConfiguration(process.env);
  const install = "/opt/vscode";
  const commit = (await readFile(path.join(install, "commit"), "utf8")).trim();
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error("Invalid pinned VS Code commit");
  const dirs = await prepareHome(home, install, commit);
  const browserDefaults = JSON.parse(await readFile(path.join(install, "browser-defaults.json"), "utf8"));
  const upstreamPort = config.port === 18080 ? 18081 : 18080;
  const child = spawn(path.join(install, "code"), ["--cli-data-dir", dirs.cliData, "serve-web",
    "--host", "127.0.0.1", "--port", String(upstreamPort), "--server-base-path", config.basePath,
    "--server-data-dir", dirs.data, "--default-folder", dirs.workspace, "--commit-id", commit,
    "--disable-telemetry", "--accept-server-license-terms", "--without-connection-token"], {
    stdio: ["ignore", "inherit", "inherit"], detached: true,
  });
  const proxy = createProxy({ ...config, upstreamPort, defaults: { ...dirs.settings, ...browserDefaults }, healthy: () => child.exitCode === null });
  let stopping = false;
  let timer;
  function stop(code) {
    if (stopping) return;
    stopping = true;
    proxy.stop();
    process.exitCode = code;
    if (child.pid && child.exitCode === null) {
      try { process.kill(-child.pid, "SIGINT"); } catch { /* already exited */ }
      timer = setTimeout(() => { try { process.kill(-child.pid, "SIGKILL"); } catch { /* already exited */ } }, 5000);
      timer.unref();
    }
  }
  child.on("error", () => stop(1));
  child.on("exit", (code) => { clearTimeout(timer); if (!stopping) stop(code || 1); });
  proxy.on("error", () => stop(1));
  process.on("SIGINT", () => stop(0));
  process.on("SIGTERM", () => stop(0));
  proxy.listen(config.port, "0.0.0.0");
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => { console.error(error.message); process.exitCode = 1; });
}
