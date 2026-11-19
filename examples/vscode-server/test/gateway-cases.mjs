// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import assert from "node:assert/strict";
import http from "node:http";
import { readFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import * as crypto from "node:crypto";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import vm from "node:vm";
import test from "node:test";
import gateway from "../gateway-extension/gateway.cjs";
import extension from "../gateway-extension/extension.cjs";

const { GROUP, gatewayConfiguration, discoverModels, providerGroup, parseGroups, mergeGatewayGroup } = gateway;
const env = { OPENAI_BASE_URL: "https://gateway.example.com/v1", OPENAI_API_KEY: "sk-dummy" };
const catalog = (...ids) => async () => Response.json({ data: ids.map(id => ({ id })) });

function editorFixture(text = "[]", initialSettings = {}) {
  const state = new Map(), settings = new Map(Object.entries(initialSettings)), commands = [], secrets = new Map();
  const uri = { path: "/User/profiles/example/chatLanguageModels.json", toString: () => "vscode-userdata:/User/profiles/example/chatLanguageModels.json" };
  const document = { uri, isDirty: false, getText: () => text, positionAt: offset => offset, save: async () => true };
  const fixture = { state, settings, commands, document, secrets, failNative: false, concurrentText: undefined, delayEditor: false,
    get text() { return text; }, set text(value) { text = value; } };
  fixture.context = { globalState: { get: key => state.get(key), update: async (key, value) => state.set(key, value) } };
  fixture.vscode = {
    Uri: { parse: value => { assert.equal(value, uri.toString()); return uri; } },
    ConfigurationTarget: { Global: 1 },
    Range: class { constructor(start, end) { Object.assign(this, { start, end }); } },
    WorkspaceEdit: class { replace(_uri, _range, replacement) { this.text = replacement; } },
    window: {},
    workspace: {
      openTextDocument: async () => document,
      applyEdit: async edit => { text = edit.text; return true; },
      getConfiguration: section => {
        assert.equal(section, "chat");
        return { get: key => {
          const value = settings.get(key);
          return value?.workspaceFolderValue ?? value?.workspaceValue ?? value?.globalValue;
        }, inspect: key => settings.get(key), update: async (key, value, target) => {
          assert.equal(target, 1);
          settings.set(key, { globalValue: value });
        } };
      },
    },
    commands: { executeCommand: async (command, group) => {
      commands.push(command);
      if (command === "workbench.action.chat.manage") fixture.providerEnabled = true;
      else if (command === "workbench.action.openLanguageModelsJson") {
        if (fixture.delayEditor) setTimeout(() => { fixture.vscode.window.activeTextEditor = { document }; }, 20);
        else fixture.vscode.window.activeTextEditor = { document };
      }
      else if (command === "workbench.action.closeActiveEditor") fixture.vscode.window.activeTextEditor = undefined;
      else if (command === "lm.migrateLanguageModelsProviderGroup") {
        assert.equal(fixture.providerEnabled, true, "Native Manage Models must enable the bundled provider first");
        if (fixture.failNative) {
          if (fixture.concurrentText) text = fixture.concurrentText;
          throw new Error("synthetic native failure sk-dummy");
        }
        const groups = parseGroups(text);
        assert.ok(!groups.some(existing => existing.name === group.name && existing.vendor === group.vendor));
        const key = `chat.lm.secret.fixture${secrets.size}`;
        secrets.set(key, group.apiKey);
        text = JSON.stringify([...groups, { ...group, apiKey: "${input:" + key + "}" }], null, 2);
      } else assert.fail(`Unexpected native command: ${command}`);
    } },
  };
  fixture.run = (models = ["example-model"], binding = env) => extension.configure(fixture.vscode, fixture.context, binding, catalog(...models));
  return fixture;
}

test("Gateway bindings are optional but partial or unsafe inputs fail without network", async () => {
  assert.equal(gatewayConfiguration({}), undefined);
  assert.equal(gatewayConfiguration({ ...env, OPENAI_BASE_URL: env.OPENAI_BASE_URL + "/" }).baseURL, env.OPENAI_BASE_URL);
  for (const binding of [{ OPENAI_BASE_URL: env.OPENAI_BASE_URL }, { OPENAI_API_KEY: "sk-dummy" },
    { ...env, OPENAI_API_KEY: "sk-dummy\n" }, ...["invalid", "file:///v1", "https://user:pass@gateway.example/v1",
      "https://gateway.example/v1?redirect=other", "https://gateway.example/v1#fragment", "https://gateway.example/other"].map(OPENAI_BASE_URL => ({ ...env, OPENAI_BASE_URL }))]) {
    let called = false;
    await assert.rejects(extension.configure({}, {}, binding, () => { called = true; }), /Invalid Gateway binding/);
    assert.equal(called, false);
  }
  const f = editorFixture();
  await f.run([], {});
  assert.deepEqual(f.commands, []);
});

test("explicitly disabled AI features do not enable Copilot or write model configuration", async () => {
  for (const setting of ["aiDisabled", "disableAIFeatures"]) {
    const f = editorFixture("[]", { [setting]: { globalValue: true } });
    await f.run();
    assert.deepEqual(f.commands, []);
    assert.equal(f.secrets.size, 0);
    assert.equal(f.state.size, 0);
  }
});

test("model discovery is bounded, uses the instance key and never follows redirects", async t => {
  const requests = [];
  const server = http.createServer((request, response) => {
    requests.push({ path: request.url, authorization: request.headers.authorization });
    response.writeHead(302, { Location: "/not-approved" });
    response.end();
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(() => { server.closeAllConnections(); server.close(); });
  const config = { baseURL: `http://127.0.0.1:${server.address().port}/v1`, apiKey: "sk-dummy" };
  await assert.rejects(discoverModels(config));
  assert.deepEqual(requests, [{ path: "/v1/models", authorization: "Bearer sk-dummy" }]);
  const normal = async (url, options) => {
    assert.equal(url, config.baseURL + "/models");
    assert.equal(options.redirect, "error");
    assert.ok(options.signal instanceof AbortSignal);
    return catalog("model-b", "cetirun-shared", "model-a", "cetirun-copilot-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")();
  };
  assert.deepEqual(await discoverModels(config, normal), ["model-a", "model-b"]);
  for (const response of [Response.json({}), Response.json({ data: [{ id: "same" }, { id: "same" }] }),
    Response.json({ data: [{ id: "invalid\nmodel" }] }), Response.json({ data: [{ id: null }] }),
    Response.json({ data: Array.from({ length: 257 }, (_, id) => ({ id: String(id) })) }),
    new Response("x".repeat(256 * 1024 + 1)), new Response("sk-dummy", { status: 401 })]) {
    await assert.rejects(discoverModels(config, async () => response));
  }
});

test("native models use explicit Chat Completions and omit default sampling parameters", () => {
  const group = providerGroup(gatewayConfiguration(env), ["example-model"]);
  assert.equal(group.vendor, "customendpoint");
  assert.equal(group.name, GROUP);
  assert.equal(group.apiType, "chat-completions");
  assert.deepEqual(group.models[0], { id: "example-model", name: "example-model",
    url: env.OPENAI_BASE_URL + "/chat/completions", toolCalling: true, vision: false,
    maxInputTokens: 114688, maxOutputTokens: 16384, modelOptions: { temperature: null, top_p: null } });
});

test("JSONC parsing keeps URL, escaped quotes and comment-like string values", () => {
  const group = { name: 'Owner "models"', vendor: "example", url: "https://example.com/v1", note: "// /* ,] ,} */" };
  assert.deepEqual(parseGroups(`// header\n[/* comment */${JSON.stringify(group)},\n]`), [group]);
  for (const text of ["{}", "[null]", "[{}]", "[/* unfinished", '[{"name":"owner" "vendor":"x"}]']) assert.throws(() => parseGroups(text));
});

test("model updates retain unrelated JSONC text through native normalization", async () => {
  const ownerText = '{"name":"Owner", "vendor":"other", "models":[{"id":"owner", "note":"[{ // \\\""}]}';
  const original = `// owner header\n[\n${ownerText}, /* trailing comment */\n]\n// footer`;
  const f = editorFixture(original);
  await f.run();
  assert.ok(f.text.startsWith("// owner header\n[\n" + ownerText));
  assert.ok(f.text.endsWith(", /* trailing comment */\n]\n// footer"));
  await f.run(["updated-model"]);
  assert.ok(f.text.startsWith("// owner header\n[\n" + ownerText));
  assert.ok(f.text.endsWith(", /* trailing comment */\n]\n// footer"));
  for (const text of ["[]", "[// empty\n]", "[/* empty */]", original]) {
    const merged = mergeGatewayGroup(text, { name: GROUP, vendor: "customendpoint", models: [] });
    assert.equal(parseGroups(merged).filter(group => group.name === GROUP).length, 1);
  }
});

test("bootstrap configures native secret storage, current profile and utility models without GitHub login", async () => {
  const owner = { vendor: "other", name: "Owner", models: [{ id: "owner-model" }] };
  const f = editorFixture(JSON.stringify([owner]));
  await f.run(["model-b", "model-a"]);
  assert.deepEqual(parseGroups(f.text)[0], owner);
  assert.equal(parseGroups(f.text)[1].models[0].id, "model-a");
  assert.equal(f.secrets.get("chat.lm.secret.fixture0"), "sk-dummy");
  assert.ok(!f.text.includes("sk-dummy"));
  assert.ok(!JSON.stringify([...f.state]).includes("sk-dummy"));
  assert.equal(f.settings.get("defaultModel").globalValue, "model-a");
  assert.equal(f.settings.get("utilityModel").globalValue, "customendpoint/model-a");
  assert.equal(f.settings.get("utilitySmallModel").globalValue, "customendpoint/model-a");
  assert.equal(f.settings.get("byokUtilityModelDefault").globalValue, "mainAgent");
  assert.ok(!f.commands.some(command => /signIn|login|authentication/i.test(command)));
  const original = f.text;
  await f.run(["model-a", "model-b"]);
  assert.equal(f.text, original);
  assert.equal(f.secrets.size, 1);
});

test("restart refreshes owned models and rotated key, preserving explicit settings", async () => {
  const f = editorFixture("[]", { utilityModel: { globalValue: "owner/utility" }, defaultModel: { workspaceValue: "owner-model" } });
  await f.run(["old-model"]);
  await f.run(["new-model"], { ...env, OPENAI_API_KEY: "sk-dummy-rotated" });
  assert.equal(parseGroups(f.text).length, 1);
  assert.equal(parseGroups(f.text)[0].models[0].id, "new-model");
  assert.equal(f.secrets.get("chat.lm.secret.fixture1"), "sk-dummy-rotated");
  assert.deepEqual(f.settings.get("defaultModel"), { workspaceValue: "owner-model" });
  assert.equal(f.settings.get("utilityModel").globalValue, "owner/utility");
  assert.equal(f.settings.get("utilitySmallModel").globalValue, "customendpoint/new-model");
});

test("credential signatures use independent salts and remain stable across restarts", async () => {
  const a = editorFixture(), b = editorFixture();
  await a.run();
  await b.run();
  const first = a.state.get("gatewayConfiguration");
  const second = b.state.get("gatewayConfiguration");
  assert.match(first.signatureSalt, /^[a-f0-9]{32}$/);
  assert.notEqual(first.signatureSalt, second.signatureSalt);
  assert.notEqual(first.signature, second.signature);
  const expected = await promisify(crypto.scrypt)(JSON.stringify(providerGroup(gatewayConfiguration(env), ["example-model"])), first.signatureSalt, 32);
  assert.equal(first.signature, expected.toString("hex"));
  await a.run();
  assert.deepEqual(a.state.get("gatewayConfiguration"), first);
  assert.equal(a.secrets.size, 1);
});

test("key-only rotation refreshes native secrets without changing the signature salt", async () => {
  const f = editorFixture();
  await f.run();
  const before = f.state.get("gatewayConfiguration");
  const rotated = { ...env, OPENAI_API_KEY: "sk-dummy-rotated" };
  await f.run(["example-model"], rotated);
  const after = f.state.get("gatewayConfiguration");
  assert.equal(after.signatureSalt, before.signatureSalt);
  assert.notEqual(after.signature, before.signature);
  assert.equal(f.secrets.get("chat.lm.secret.fixture1"), rotated.OPENAI_API_KEY);
  assert.ok(!JSON.stringify([...f.state]).includes(rotated.OPENAI_API_KEY));
  await f.run(["example-model"], rotated);
  assert.equal(f.secrets.size, 2);
});

test("legacy signatures migrate once and failed migration preserves state and user edits", async () => {
  for (const binding of [env, { ...env, OPENAI_API_KEY: "sk-dummy-rotated" }]) {
    const f = editorFixture("[]", { utilityModel: { globalValue: "owner/utility" } });
    await f.run();
    const { signatureSalt, ...legacy } = f.state.get("gatewayConfiguration");
    legacy.signature = "0".repeat(64);
    f.state.set("gatewayConfiguration", legacy);
    const original = f.text;
    f.failNative = true;
    await assert.rejects(f.run(["example-model"], binding), /Native Gateway model setup failed/);
    assert.equal(f.text, original);
    assert.deepEqual(f.state.get("gatewayConfiguration"), legacy);
    f.failNative = false;
    await f.run(["example-model"], binding);
    assert.match(f.state.get("gatewayConfiguration").signatureSalt, /^[a-f0-9]{32}$/);
    assert.equal(f.secrets.size, 2);
    assert.equal(f.secrets.get("chat.lm.secret.fixture1"), binding.OPENAI_API_KEY);
    assert.equal(f.settings.get("utilityModel").globalValue, "owner/utility");
    await f.run(["example-model"], binding);
    assert.equal(f.secrets.size, 2);
  }
});

test("plaintext keys never reach the fast hash, including user-edited model groups", async () => {
  const source = await readFile(new URL("../gateway-extension/extension.cjs", import.meta.url), "utf8");
  const sandbox = { module: { exports: {} }, require: name => {
    if (name === "./gateway.cjs") return gateway;
    if (name === "node:util") return { promisify };
    assert.equal(name, "node:crypto");
    return { ...crypto, createHash: algorithm => {
      const hash = crypto.createHash(algorithm);
      return { update: value => {
        assert.ok(!value.includes("sk-dummy"), "Plaintext keys must not enter SHA-256");
        return hash.update(value);
      } };
    } };
  } };
  vm.runInNewContext(source, sandbox);
  const f = editorFixture('[{"name":"Owner","vendor":"other","apiKey":"sk-dummy-owner"}]');
  const run = () => sandbox.module.exports.configure(f.vscode, f.context, env, catalog("example-model"));
  await run();
  const original = f.text;
  const current = f.state.get("gatewayConfiguration");
  for (const key of ["sk-dummy-edited", "${input:chat.lm.secret.user-edited}"]) {
    for (const legacy of [false, true]) {
      const state = { ...current };
      if (legacy) delete state.signatureSalt;
      f.state.set("gatewayConfiguration", state);
      const groups = parseGroups(original);
      groups.find(group => group.name === GROUP).apiKey = key;
      f.text = JSON.stringify(groups);
      const edited = f.text;
      await assert.rejects(run(), /Existing Gateway configuration was edited/);
      assert.equal(f.text, edited);
      assert.deepEqual(f.state.get("gatewayConfiguration"), state);
      assert.equal(f.secrets.size, 1);
    }
  }
});

test("bootstrap waits for the browser model editor to reach the remote extension host", async () => {
  const f = editorFixture();
  f.delayEditor = true;
  await f.run();
  assert.equal(parseGroups(f.text)[0].name, GROUP);
  assert.ok(f.commands.includes("lm.migrateLanguageModelsProviderGroup"));
});

test("user changes to bootstrap defaults remain user-owned on subsequent reloads", async () => {
  const f = editorFixture();
  await f.run();
  f.settings.set("utilitySmallModel", { globalValue: "owner/small" });
  await f.run(["updated-model"]);
  assert.equal(f.settings.get("utilitySmallModel").globalValue, "owner/small");
  assert.equal(f.settings.get("defaultModel").globalValue, "updated-model");
  await f.run(["another-model"]);
  assert.equal(f.settings.get("utilitySmallModel").globalValue, "owner/small");
});

test("missing models, malformed, dirty, conflicting and manually edited configurations are preserved", async () => {
  const f = editorFixture();
  await assert.rejects(f.run([]), /No Gateway models/);
  assert.deepEqual(f.commands, []);
  for (const text of ["not json", JSON.stringify([{ vendor: "customendpoint", name: GROUP }])]) {
    const f = editorFixture(text);
    await assert.rejects(f.run());
    assert.equal(f.text, text);
    assert.equal(f.secrets.size, 0);
  }
  f.document.isDirty = true;
  await assert.rejects(f.run(), /Save the active/);
  f.document.isDirty = false;
  await f.run();
  const edited = parseGroups(f.text);
  edited[0].models[0].vision = true;
  f.text = JSON.stringify(edited);
  await assert.rejects(f.run(), /edited/);
  assert.deepEqual(parseGroups(f.text), edited);
});

test("failed native refresh restores the prior group but does not overwrite concurrent edits", async () => {
  const f = editorFixture();
  await f.run();
  const original = f.text;
  f.failNative = true;
  await assert.rejects(f.run(["changed-model"]), /Native Gateway model setup failed/);
  assert.equal(f.text, original);
  f.concurrentText = '[{"vendor":"owner","name":"Concurrent"}]';
  await assert.rejects(f.run(["changed-model"]), /Native Gateway model setup failed/);
  assert.equal(f.text, f.concurrentText);
});

test("the demo bundles the workspace configuration extension without replacing Copilot", async () => {
  const pkg = JSON.parse(await readFile(new URL("../gateway-extension/package.json", import.meta.url), "utf8"));
  assert.deepEqual(pkg.extensionKind, ["workspace"]);
  assert.equal(pkg.extensionDependencies, undefined);
  assert.equal(pkg.capabilities.untrustedWorkspaces.supported, false);
  assert.deepEqual(pkg.activationEvents, ["onStartupFinished"]);
  assert.equal(pkg.contributes.languageModelChatProviders, undefined);
  const docker = await readFile(new URL("../Dockerfile", import.meta.url), "utf8");
  assert.match(docker, /\/opt\/vscode\/server\/extensions\/cetirun-gateway-config/);
  const agent = await readFile(new URL("../agent.yaml", import.meta.url), "utf8");
  assert.match(agent, /sharedModels: true\s+githubCopilot: false/);
});

test("the entrypoint rejects a partial Gateway binding before loading VS Code", () => {
  const result = spawnSync(process.execPath, [fileURLToPath(new URL("../start.mjs", import.meta.url))], {
    env: { PORT: "8080", CETIRUN_WEB_BASE_PATH: "/ui/example/", CETIRUN_WEB_INSTANCE: "uid-example",
      HOME: "/tmp/cetirun-synthetic-home", VSCODE_SERVER_LICENSE_ACCEPTED: "true", OPENAI_BASE_URL: env.OPENAI_BASE_URL },
    encoding: "utf8", timeout: 5000,
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Invalid Gateway binding/);
  assert.doesNotMatch(result.stderr, /ENOENT/);
});

test("activation reports a sanitized warning without leaking a failed provider response", async () => {
  const source = await readFile(new URL("../gateway-extension/extension.cjs", import.meta.url), "utf8");
  const warnings = [];
  const sandbox = { module: { exports: {} }, process: { env }, fetch: async () => { throw new Error("sk-dummy private response"); },
    require: name => {
      if (name === "./gateway.cjs") return gateway;
      if (name === "node:crypto") return { ...crypto, randomBytes: () => assert.fail("No configuration should be written") };
      if (name === "node:util") return { promisify };
      assert.equal(name, "vscode");
      return { commands: { registerCommand: () => ({ dispose() {} }) },
        workspace: { getConfiguration: () => ({ get: () => undefined }) },
        window: { showWarningMessage: value => warnings.push(value) } };
    } };
  vm.runInNewContext(source, sandbox);
  await sandbox.module.exports.activate({ subscriptions: [] });
  assert.equal(warnings.length, 1);
  assert.ok(!warnings[0].includes("sk-dummy"));
  assert.ok(!warnings[0].includes("private response"));
});
