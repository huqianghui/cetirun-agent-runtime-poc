// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

const { createHash, randomBytes, scrypt } = require("node:crypto");
const { promisify } = require("node:util");
const { GROUP, gatewayConfiguration, discoverModels, providerGroup, parseGroups, mergeGatewayGroup } = require("./gateway.cjs");

const STATE = "gatewayConfiguration";
const digest = value => createHash("sha256").update(JSON.stringify(value)).digest("hex");
const deriveSignature = promisify(scrypt);
const isGateway = group => group.name === GROUP && group.vendor === "customendpoint";
const isSecretReference = value => typeof value === "string" && /^\$\{input:chat\.lm\.secret\.[^}]+\}$/.test(value);
const setupMessages = new Set([
  "Invalid Gateway binding", "Gateway model discovery failed", "Gateway catalog is too large",
  "Invalid Gateway catalog", "No Gateway models are available", "Invalid language model configuration",
  "Save the active language model configuration before setup", "Existing Gateway configuration was edited; it is preserved",
  "Native Gateway model setup failed", "Native Gateway secret storage was not confirmed",
  "Model configuration changed during setup", "Could not save model configuration",
]);

async function openedModelsDocument(vscode) {
  for (let attempt = 0; attempt < 50; attempt++) {
    const document = vscode.window.activeTextEditor?.document;
    if (document?.uri.path.endsWith("/chatLanguageModels.json")) return document;
    await new Promise(resolve => setTimeout(resolve, 100));
  }
}

async function replaceDocument(vscode, document, expected, next) {
  if (document.isDirty || document.getText() !== expected) throw new Error("Model configuration changed during setup");
  const edit = new vscode.WorkspaceEdit();
  edit.replace(document.uri, new vscode.Range(document.positionAt(0), document.positionAt(expected.length)), next);
  if (!await vscode.workspace.applyEdit(edit) || !await document.save()) throw new Error("Could not save model configuration");
}

async function configure(vscode, context, env = process.env, fetcher = fetch) {
  const config = gatewayConfiguration(env);
  if (!config) return;
  const chat = vscode.workspace.getConfiguration("chat");
  if (chat.get("aiDisabled") || chat.get("disableAIFeatures")) return;
  const ids = await discoverModels(config, fetcher);
  if (!ids.length) throw new Error("No Gateway models are available");
  const group = providerGroup(config, ids);
  const previous = context.globalState.get(STATE);
  let state = previous;
  let document;
  let opened = false;
  if (previous?.uri) {
    document = await vscode.workspace.openTextDocument(vscode.Uri.parse(previous.uri));
  } else {
    // The browser owns this profile, not the remote server's data/Machine directory.
    await vscode.commands.executeCommand("workbench.action.chat.manage");
    await vscode.commands.executeCommand("workbench.action.openLanguageModelsJson");
    document = await openedModelsDocument(vscode);
    opened = true;
  }
  if (!document || !document.uri.path.endsWith("/chatLanguageModels.json") || document.isDirty) {
    throw new Error("Save the active language model configuration before setup");
  }
  const original = document.getText();
  const groups = parseGroups(original);
  const owned = groups.filter(isGateway);
  if (owned.length > 1 || (owned.length &&
      (!isSecretReference(owned[0].apiKey) || digest(owned[0]) !== previous?.groupDigest))) {
    throw new Error("Existing Gateway configuration was edited; it is preserved");
  }
  const signatureSalt = typeof previous?.signatureSalt === "string" && /^[a-f0-9]{32}$/.test(previous.signatureSalt)
    ? previous.signatureSalt : randomBytes(16).toString("hex");
  const signature = (await deriveSignature(JSON.stringify(group), signatureSalt, 32)).toString("hex");
  if (!owned.length || previous?.signatureSalt !== signatureSalt || previous?.signature !== signature) {
    const withoutGateway = JSON.stringify(groups.filter(group => !isGateway(group)), null, 2) + "\n";
    if (owned.length) await replaceDocument(vscode, document, original, withoutGateway);
    try {
      // Activate the native provider before storing apiKey as a secret-backed input reference.
      await vscode.commands.executeCommand("lm.migrateLanguageModelsProviderGroup", group);
    } catch {
      if (owned.length && !document.isDirty && document.getText() === withoutGateway) {
        await replaceDocument(vscode, document, withoutGateway, original);
      }
      throw new Error("Native Gateway model setup failed");
    }
    const nativeText = document.getText();
    const savedGroups = parseGroups(nativeText);
    const saved = savedGroups.filter(isGateway);
    if (saved.length !== 1 || !isSecretReference(saved[0].apiKey)) {
      throw new Error("Native Gateway secret storage was not confirmed");
    }
    if (JSON.stringify(savedGroups.filter(group => !isGateway(group))) !== JSON.stringify(groups.filter(group => !isGateway(group)))) {
      throw new Error("Model configuration changed during setup");
    }
    // Native addition normalizes the whole JSON file; restore unrelated formatting and comments.
    await replaceDocument(vscode, document, nativeText, mergeGatewayGroup(original, saved[0]));
    state = { uri: document.uri.toString(), signature, signatureSalt, groupDigest: digest(saved[0]), defaults: previous?.defaults };
    await context.globalState.update(STATE, state);
  }
  const settings = vscode.workspace.getConfiguration("chat");
  const defaults = {};
  for (const [key, value] of Object.entries({ defaultModel: ids[0], byokUtilityModelDefault: "mainAgent",
    utilityModel: `customendpoint/${ids[0]}`, utilitySmallModel: `customendpoint/${ids[0]}` })) {
    const existing = settings.inspect(key);
    if (existing?.workspaceValue === undefined && existing?.workspaceFolderValue === undefined &&
        (existing?.globalValue === undefined || existing.globalValue === previous?.defaults?.[key])) {
      await settings.update(key, value, vscode.ConfigurationTarget.Global);
      defaults[key] = value;
    }
  }
  await context.globalState.update(STATE, { ...state, defaults });
  if (opened && vscode.window.activeTextEditor?.document === document) {
    await vscode.commands.executeCommand("workbench.action.closeActiveEditor");
  }
}

function activate(context) {
  const vscode = require("vscode");
  let running;
  const run = () => running ??= configure(vscode, context).catch(error => {
    // Native errors can contain provider responses or credentials. Never echo them.
    const detail = setupMessages.has(error?.message) ? ` ${error.message}.` : "";
    void vscode.window.showWarningMessage(`CetiRun AI Gateway setup could not finish.${detail} Check Gateway access and saved Language Models configuration, then run CetiRun: Configure AI Gateway Models. No GitHub sign-in fallback was enabled.`);
  }).finally(() => { running = undefined; });
  context.subscriptions.push(vscode.commands.registerCommand("cetirun.configureGateway", run));
  return run();
}

module.exports = { activate, configure };
