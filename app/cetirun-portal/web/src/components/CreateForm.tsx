// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useMemo, useState } from "react";
import { Button, Input, Field, Switch, MessageBar, Select } from "@fluentui/react-components";
import { useCreateInstance } from "../hooks/useInstance";
import { useFeatures } from "../hooks/useFeatures";
import { useCreateOptions } from "../hooks/useCreateOptions";
import type { AgentType } from "../types/instance";
import { TeamPicker } from "./TeamPicker";
import { RateLimitsSummary } from "./RateLimitsSummary";
import { CreateInstanceError } from "../api/instance";

const NO_AGENT_TYPES: AgentType[] = [];
const AGENT_TYPE_LABELS: Record<AgentType, string> = {
  openclaw: "OpenClaw",
  hermes: "Hermes",
  custom: "Custom",
};

export function CreateForm({ onCreated }: { onCreated?: () => void }) {
  const [instanceName, setInstanceName] = useState("");
  const [agentType, setAgentType] = useState<AgentType>("hermes");
  const [image, setImage] = useState("");
  const [username, setUsername] = useState("vibe");
  const [port, setPort] = useState("");
  const [webUI, setWebUI] = useState(false);
  const [validationError, setValidationError] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [team, setTeam] = useState("");
  const [gatewayToken, setGatewayToken] = useState("");
  const [tailscaleAuthKey, setTailscaleAuthKey] = useState("");
  const [agentIdentityEnabled, setAgentIdentityEnabled] = useState(false);
  const [privateRegistry, setPrivateRegistry] = useState(false);
  const [registryUsername, setRegistryUsername] = useState("");
  const [registryPassword, setRegistryPassword] = useState("");
  const [requestId] = useState(() => crypto.randomUUID());
  const clearRegistryCredentials = () => {
    setRegistryUsername("");
    setRegistryPassword("");
  };
  const [useSharedModels, setUseSharedModels] = useState(true);
  const [useCopilot, setUseCopilot] = useState(true);
  const [gatewayModel, setGatewayModel] = useState("");
  const createMutation = useCreateInstance({ onSuccess: onCreated, onSettled: clearRegistryCredentials });
  const { data: features, isLoading: featuresLoading, error: featuresError, refetch: refreshFeatures } = useFeatures();
  const optionsEnabled = !!features;
  const optionsQuery = useCreateOptions(team, optionsEnabled);
  const options = optionsEnabled && optionsQuery.data?.team === team && !optionsQuery.error ? optionsQuery.data : undefined;
  const { refetch: refreshOptions } = optionsQuery;
  useEffect(() => {
    if (createMutation.error) void refreshOptions();
  }, [createMutation.error, refreshOptions]);
  const custom = agentType === "custom";
  const gatewayOn = options?.aiGatewayEnabled ?? !!features?.aiGatewayEnabled;
  // Every platform-managed harness takes its models from the AI Gateway.
  const gatewayModels = gatewayOn;
  const catalog = options?.gateway;
  const catalogKnown = !!catalog?.known;
  const routedAgentTypes = catalog?.agentTypes ?? null;
  const agentTypesUnknown = gatewayModels && catalogKnown && routedAgentTypes === null;
  const chartUnsupported =
    gatewayModels && catalogKnown && routedAgentTypes !== null && !routedAgentTypes.includes(agentType);
  const sharedModels = useMemo(
    () => (catalog?.known ? catalog.sharedModels : []),
    [catalog],
  );
  const sharedAvailable = sharedModels.length > 0;
  // The Team's smart-routing models the Team's Policy allows, by name only.
  const modelRouters = useMemo(() => (catalog?.known ? options?.modelRouters ?? [] : []), [catalog, options?.modelRouters]);
  const copilotOffered = catalogKnown && !!catalog?.githubCopilot;
  // Every Agent needs a model source: shared models while any are
  // available, or GitHub Copilot while the deployment offers it. Without
  // GitHub Copilot, shared models stay on; with no shared model on,
  // GitHub Copilot does.
  const sharedForced = !copilotOffered && sharedAvailable;
  const effectiveShared = sharedAvailable && (sharedForced || useSharedModels);
  const copilotForced = copilotOffered && (!effectiveShared || !sharedAvailable);
  const effectiveCopilot = copilotOffered && (copilotForced || useCopilot);
  const noModelSource = catalogKnown && !sharedAvailable && !copilotOffered;

  const allowedAgentTypes = useMemo(
    () => options?.allowedAgentTypes ?? NO_AGENT_TYPES,
    [options?.allowedAgentTypes],
  );

  useEffect(() => {
    if (allowedAgentTypes.length > 0 && !allowedAgentTypes.includes(agentType)) {
      setAgentType(allowedAgentTypes.includes("hermes") ? "hermes" : allowedAgentTypes[0]);
      setImage("");
      setGatewayToken("");
      setTailscaleAuthKey("");
      setAgentIdentityEnabled(false);
      setWebUI(false);
      setPort("");
      setValidationError("");
    }
  }, [agentType, allowedAgentTypes]);

  useEffect(() => {
    if (options && !modelRouters.includes(gatewayModel) && (!effectiveShared || !sharedModels.some((m) => m.name === gatewayModel))) setGatewayModel("");
  }, [options, effectiveShared, sharedModels, modelRouters, gatewayModel]);

  useEffect(() => {
    if (options && (!options.agentIdentity.allowed || agentType === "custom" && !options.agentIdentity.customAllowed)) setAgentIdentityEnabled(false);
  }, [agentType, options]);

  const identitySupported = agentType !== "custom" || !!options?.agentIdentity.customAllowed;
  const effectiveIdentity = identitySupported && !!options?.agentIdentity.allowed && (options.agentIdentity.required || agentIdentityEnabled);
  const creationBlocked = createMutation.isPending || featuresLoading || !!featuresError || !options ||
    optionsQuery.isFetching || !!options.blocked || !allowedAgentTypes.includes(agentType) ||
    (options.agentIdentity.required && (!options.agentIdentity.allowed || !identitySupported)) ||
    (gatewayModels && (!catalogKnown || noModelSource || chartUnsupported || agentTypesUnknown));

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (creationBlocked) return;
    const httpPort = port.trim() === "" ? undefined : Number(port);
    if (custom && (!image.trim() || !/^[a-z_][a-z0-9_-]{0,31}$/.test(username) || username === "root")) {
      setValidationError("Custom requires an image and an existing non-root image username.");
      return;
    }
    if (custom && httpPort !== undefined && (!Number.isInteger(httpPort) || httpPort < 1 || httpPort > 65535)) {
      setValidationError("HTTP port must be an integer between 1 and 65535, or left blank for a headless Agent.");
      return;
    }
    if (custom && webUI && httpPort === undefined) {
      setValidationError("A port is required when Web UI is enabled.");
      return;
    }
    setValidationError("");
    createMutation.mutate({
      instanceName: instanceName || undefined,
      agent: custom
        ? { type: agentType, image: image.trim(), username, port: httpPort, webUI: webUI || undefined }
        : { type: agentType, image: image.trim() || undefined },
      registryAuth: privateRegistry ? { username: registryUsername, password: registryPassword } : undefined,
      requestId: privateRegistry ? requestId : undefined,
      displayName: displayName || undefined,
      team: team || undefined,
      gatewayToken: !custom ? gatewayToken || undefined : undefined,
      tailscaleAuthKey: !custom ? tailscaleAuthKey || undefined : undefined,
      model: gatewayModels && !custom ? gatewayModel || undefined : undefined,
      agentIdentityEnabled: effectiveIdentity || undefined,
      modelAccess: gatewayModels ? { sharedModels: effectiveShared, githubCopilot: effectiveCopilot } : undefined,
    });
  };

  return (
    <form
      onSubmit={handleSubmit}
      style={{ display: "flex", flexDirection: "column", gap: 16, marginTop: 16 }}
    >
      <Field
        label="Agent Name (optional)"
        hint={
          instanceName
            ? `CR name will be: <your-alias>-${instanceName} (e.g. alice-${instanceName})`
            : "Leave blank to use your alias as the agent name. Lowercase letters, digits, hyphens only."
        }
      >
        <Input
          value={instanceName}
          onChange={(_, data) => setInstanceName(data.value)}
          placeholder="e.g. my-project (leave blank for default)"
          pattern={"[a-z](?:[a-z0-9\\-]*[a-z0-9])?"}
        />
      </Field>

      <TeamPicker value={team} onChange={setTeam} />
      {options?.policyName && <p style={{ margin: 0 }}>Agent Policy: {options.policyName}</p>}
      {options?.policyName && (
        <RateLimitsSummary limits={options.rateLimits} intro="The policy limits each Agent's own AI Gateway key:" />
      )}
      {optionsEnabled && optionsQuery.isFetching && <MessageBar intent="info">Loading allowed Agent options...</MessageBar>}
      {(featuresError || optionsQuery.error) && (
        <MessageBar intent="error">
          {(featuresError || optionsQuery.error)?.message}
          <Button appearance="subtle" onClick={() => { if (featuresError) void refreshFeatures(); else void refreshOptions(); }}>Retry options</Button>
        </MessageBar>
      )}
      {options?.blocked && <MessageBar intent="error">{options.blocked.message}</MessageBar>}
      <Field label="Agent Type">
        <Select
          value={allowedAgentTypes.includes(agentType) ? agentType : ""}
          disabled={!options || optionsQuery.isFetching || allowedAgentTypes.length <= 1}
          onChange={(event) => {
            setAgentType(event.target.value as AgentType);
            setImage("");
            setPort("");
            setGatewayToken("");
            setTailscaleAuthKey("");
            setAgentIdentityEnabled(false);
            setWebUI(false);
            setValidationError("");
          }}
        >
          {allowedAgentTypes.map((type) => (
            <option key={type} value={type}>
              {AGENT_TYPE_LABELS[type]}
            </option>
          ))}
        </Select>
      </Field>

      {custom && (
        <MessageBar intent="info">
          Image updates retain HOME and require a compatible image. Platform-provided model access uses the AI Gateway.
        </MessageBar>
      )}
      <Field
        label={custom || privateRegistry ? "Agent Image" : "Agent Image (optional)"}
        hint={custom
          ? "Use an image supported by the configured runtime. The image keeps its own entrypoint."
          : "Leave empty for the platform default. Overrides must be qualified for the selected agent type."}
        required={custom || privateRegistry}
      >
        <Input value={image} onChange={(_, data) => setImage(data.value)} maxLength={2048} placeholder="registry.example.com/team/agent:version" required={custom || privateRegistry} />
      </Field>
      {custom && (
        <>
          <Field label="Agent Username" required hint="An existing non-root user in the image; HOME is /home/<username>. This cannot change after creation.">
            <Input value={username} onChange={(_, data) => setUsername(data.value)} required maxLength={32} pattern={"[a-z_][a-z0-9_\\-]{0,31}"} />
          </Field>
          <Switch checked={webUI} label="Serve Web UI" onChange={(_, data) => setWebUI(data.checked)} />
          <Field label={webUI ? "Web UI Port" : "HTTP Port (optional)"} required={webUI}
            hint={webUI ? "Serve a browser UI through platform authentication instead of the Chat API. The image must support /ui/<instance>/ and /healthz." : "Leave blank for headless operation. Platform Terminal and Desktop do not need this port."}>
            <Input type="number" min={1} max={65535} step={1} value={port} required={webUI} onChange={(_, data) => setPort(data.value)} placeholder="e.g. 8080" />
          </Field>
        </>
      )}

      <Field label="Display Name (optional)">
        <Input
          value={displayName}
          onChange={(_, data) => setDisplayName(data.value)}
          placeholder="Defaults to your profile name"
        />
      </Field>

      <Switch checked={privateRegistry} label="Use private registry credentials" onChange={(_, data) => {
        setPrivateRegistry(data.checked);
        clearRegistryCredentials();
      }} />
      {privateRegistry && (
        <>
          <Field label="Registry username" required>
            <Input autoComplete="off" value={registryUsername} onChange={(_, data) => setRegistryUsername(data.value)} required />
          </Field>
          <Field label="Registry password or token" required hint="Write-only. The registry address is taken from the image; no Secret setup is needed.">
            <Input type="password" autoComplete="new-password" value={registryPassword} onChange={(_, data) => setRegistryPassword(data.value)} required />
          </Field>
        </>
      )}

      {chartUnsupported && (
        <MessageBar intent="error">
          {`This deployment's agent chart does not route ${AGENT_TYPE_LABELS[agentType]} agents through the AI Gateway. Ask an administrator to upgrade the agent chart.`}
        </MessageBar>
      )}

      {agentTypesUnknown && (
        <MessageBar intent="warning">
          The operator has not read its agent chart yet. Try again in a minute.
        </MessageBar>
      )}

      {options && gatewayModels && !chartUnsupported && !agentTypesUnknown && (
        <>
          {sharedAvailable && <Field
            hint={`Models your administrators share: ${sharedModels.map((m) => m.name).join(", ")}`}
          >
            <Switch
              checked={effectiveShared}
              disabled={sharedForced}
              onChange={(_, data) => setUseSharedModels(!!data.checked)}
              label="Use shared models"
            />
          </Field>}
          {copilotOffered && <Field
            hint={copilotForced
              ? "Required when shared models are off or none are available. Sign in with GitHub once the agent is created."
              : "Uses your own GitHub Copilot subscription. Sign in with GitHub once the agent is created."}
          >
            <Switch
              checked={effectiveCopilot}
              disabled={!copilotOffered || copilotForced}
              onChange={(_, data) => setUseCopilot(!!data.checked)}
              label="Use GitHub Copilot"
            />
          </Field>}
          {!custom && ((effectiveShared && sharedAvailable) || modelRouters.length > 0) && (
            <Field label="Default model (optional)" hint={[
              effectiveCopilot ? "GitHub Copilot models can be chosen after sign-in." : "",
              modelRouters.length > 0 ? "Smart-routing models pick a model for each request; one works once the agent may use every model it routes to." : "",
            ].filter(Boolean).join(" ") || undefined}>
              <Select value={gatewayModel} onChange={(event) => setGatewayModel(event.target.value)}>
                <option value="">(automatic)</option>
                {effectiveShared && sharedModels.map((m) => (
                  <option key={m.name} value={m.name}>
                    {m.name}
                  </option>
                ))}
                {modelRouters.map((name) => (
                  <option key={`router-${name}`} value={name}>
                    {name} (smart routing)
                  </option>
                ))}
              </Select>
            </Field>
          )}
          {!catalogKnown && !optionsQuery.isFetching && (
            <MessageBar intent="warning">
              The AI Gateway has not published its models yet. Try again in a minute.
            </MessageBar>
          )}
          {noModelSource && (
            <MessageBar intent="error">
              No models are available for new agents under the selected Team's Policy and deployment settings. Ask an administrator to check model access.
            </MessageBar>
          )}
        </>
      )}

      {!custom && <Field label="Gateway Token (optional)">
        <Input
          type="password"
          autoComplete="off"
          value={gatewayToken}
          onChange={(_, data) => setGatewayToken(data.value)}
          placeholder="Leave empty to auto-generate"
        />
      </Field>}

      {!custom && features?.tailscaleEnabled && (
        <Field label="Tailscale Auth Key (optional)">
          <Input
            type="password"
            autoComplete="off"
            value={tailscaleAuthKey}
            onChange={(_, data) => setTailscaleAuthKey(data.value)}
            placeholder="Leave empty to skip Tailscale"
          />
        </Field>
      )}

      {identitySupported && options?.agentIdentity.allowed && (
        <Switch
          checked={effectiveIdentity}
          disabled={options.agentIdentity.required}
          onChange={(_, data) => setAgentIdentityEnabled(!!data.checked)}
          label="Enable Microsoft Entra Agent Identity"
        />
      )}

      <p style={{ margin: 0, fontSize: 12 }}>
        When Memory assets are installed, memory and bounded learning are enabled automatically.
        Qualified entrypoints may use up to four extra model requests per day for synthetic method
        checks. Conversation-level memory restrictions still apply; Team Skills and tool permissions
        are unchanged.
      </p>

      <Button
        type="submit"
        appearance="primary"
        disabled={creationBlocked}
      >
        {createMutation.isPending ? "Creating..." : "Create Agent"}
      </Button>

      {validationError && <MessageBar intent="error">{validationError}</MessageBar>}
      {createMutation.error && (
        <MessageBar intent="error">
          {(createMutation.error as Error).message}
          {createMutation.error instanceof CreateInstanceError && createMutation.error.instanceName && (
            <a href={`/instances/${encodeURIComponent(createMutation.error.instanceName)}/overview`}>Manage the existing agent</a>
          )}
        </MessageBar>
      )}
    </form>
  );
}
