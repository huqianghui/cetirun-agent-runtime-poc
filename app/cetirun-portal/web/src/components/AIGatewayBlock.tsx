// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useState } from "react";
import {
  Badge,
  Button,
  Dialog,
  DialogActions,
  DialogBody,
  DialogContent,
  DialogSurface,
  DialogTitle,
  Select,
  Spinner,
  Text,
  tokens,
} from "@fluentui/react-components";
import { Checkmark20Regular, Copy20Regular, Open20Regular, Warning20Regular } from "@fluentui/react-icons";
import { useQueryClient } from "@tanstack/react-query";
import { useCopilotGatewayLogin } from "../hooks/useAIGateway";
import { useUpdateInstanceByName } from "../hooks/useInstance";
import type { CopilotProxyPhase, Instance } from "../types/instance";
import { RateLimitsSummary } from "./RateLimitsSummary";

const COPILOT_PHASES: Record<CopilotProxyPhase, { label: string; color: "warning" | "informative" | "success" | "danger" }> = {
  SignInRequired: { label: "Sign-in required", color: "warning" },
  Starting: { label: "Starting", color: "informative" },
  Ready: { label: "Ready", color: "success" },
  Degraded: { label: "Degraded", color: "danger" },
};

const REFRESH_MS = 10_000;
const REFRESH_WINDOW_MS = 5 * 60_000;

// The Copilot reasons for which the operator keeps the Agent's Copilot models
// off its key, and what they mean to the owner. Signing in again does not
// help with them.
const COPILOT_WITHHELD = new Map<string, string>([
  ["NotAllowedRouteRemovalFailed",
    "GitHub Copilot models are unavailable: the Agent Policy no longer allows one of them, and the AI Gateway has not removed it yet. They come back when it is removed. Shared models keep working."],
  ["PolicyCheckUnavailable",
    "GitHub Copilot models are unavailable until the operator can check them against the Agent Policy. Shared models keep working."],
]);

/**
 * The Agent's models in the platform AI Gateway, its default model, and the
 * GitHub Copilot sign-in the Portal runs for the Agent's Copilot proxy.
 */
export function AIGatewayBlock({ instance }: { instance: Instance }) {
  const gateway = instance.aiGateway;
  const custom = instance.agent.type === "custom";
  const name = instance.instanceName;
  const queryClient = useQueryClient();
  // Resume a sign-in still pending when the page was reloaded.
  const login = useCopilotGatewayLogin(name, instance.uid, !!gateway?.routed && !!gateway.copilot);
  const update = useUpdateInstanceByName(name);
  const [defaultModel, setDefaultModel] = useState(gateway?.defaultModel ?? "");
  const [copied, setCopied] = useState(false);
  const [signedInAt, setSignedInAt] = useState<number | null>(null);

  useEffect(() => {
    setDefaultModel(gateway?.defaultModel ?? "");
  }, [gateway?.defaultModel]);

  useEffect(() => {
    if (login.login?.state === "succeeded") setSignedInAt(Date.now());
  }, [login.login?.state]);

  // The refresh after a sign-in ends with its window even if nothing else
  // re-renders the page.
  useEffect(() => {
    if (signedInAt === null) return;
    const id = setTimeout(() => setSignedInAt(null), REFRESH_WINDOW_MS);
    return () => clearTimeout(id);
  }, [signedInAt]);

  const copilotPhase = gateway?.copilot?.phase;
  // Keep the page current while the proxy picks up a sign-in and the
  // operator registers the account's models.
  const refreshing = copilotPhase === "Starting" || (signedInAt !== null && copilotPhase !== "Ready");
  useEffect(() => {
    if (!refreshing) return;
    const id = setInterval(() => {
      void queryClient.invalidateQueries({ queryKey: ["instance", name] });
    }, REFRESH_MS);
    return () => clearInterval(id);
  }, [refreshing, queryClient, name]);

  if (!gateway?.routed) return null;

  const copilot = gateway.copilot;
  const pending = login.login?.state === "pending";
  const dialogOpen = login.login !== null && login.login.state !== "none";
  const phase = copilotPhase ? COPILOT_PHASES[copilotPhase] : undefined;

  const handleCopy = () => {
    const code = login.login?.userCode;
    if (!code) return;
    navigator.clipboard
      .writeText(code)
      .then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 2000);
      })
      .catch(() => {
        // Without clipboard access the code stays visible to copy by hand.
      });
  };

  const closeDialog = () => {
    if (pending) {
      void login.cancel();
    } else {
      login.reset();
    }
  };

  const emptyHint = copilot && copilotPhase !== "Ready"
    ? "Sign in to GitHub Copilot to add your Copilot models."
    : gateway.sharedModels
      ? "No shared models are available yet."
      : "No models are available yet.";

  return (
    <div
      style={{
        marginTop: 8,
        marginBottom: 8,
        padding: 12,
        borderRadius: tokens.borderRadiusMedium,
        border: `1px solid ${tokens.colorNeutralStroke1}`,
        display: "flex",
        flexDirection: "column",
        gap: 8,
      }}
    >
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <Text weight="semibold">Models</Text>
        <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
          through the AI Gateway
          {gateway.sharedModels ? " · shared models" : ""}
          {gateway.githubCopilot ? " · GitHub Copilot" : ""}
        </Text>
      </div>

      {gateway.phase === "Degraded" && (
        <div style={{ display: "flex", alignItems: "flex-start", gap: 8 }}>
          <Warning20Regular style={{ color: tokens.colorStatusDangerForeground1, flexShrink: 0, marginTop: 2 }} />
          <Text size={200} style={{ color: tokens.colorStatusDangerForeground1 }}>
            The agent's AI Gateway key is not ready{gateway.reason ? ` (${gateway.reason})` : ""}. The operator retries automatically.
          </Text>
        </div>
      )}

      {gateway.models.length > 0 ? (
        <div style={{ display: "flex", flexWrap: "wrap", gap: 6 }} aria-label="Agent models">
          {gateway.models.map((model) => (
            <Badge key={model} appearance={!custom && model === gateway.defaultModel ? "filled" : "outline"} color="brand">
              {model}
              {!custom && model === gateway.defaultModel ? " (default)" : ""}
            </Badge>
          ))}
        </div>
      ) : (
        <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>{emptyHint}</Text>
      )}

      {(gateway.modelRouters ?? []).some((router) => !router.granted) && (
        <div style={{ display: "flex", flexDirection: "column", gap: 2 }} aria-label="Unavailable smart-routing models">
          {(gateway.modelRouters ?? []).filter((router) => !router.granted).map((router) => (
            <Text key={router.name} size={200} style={{ color: tokens.colorNeutralForeground3 }}>
              Smart-routing model {router.name} is not available to this agent{router.reason ? `: ${router.reason}` : "."}
            </Text>
          ))}
        </div>
      )}

      {custom && (
        <Text size={200}>Choose a model in Chat from the models this Agent exposes.</Text>
      )}
      <RateLimitsSummary
        limits={gateway.rateLimits}
        intro="Set by the Team's Agent Policy on this Agent's own key. Requests over a limit get HTTP 429 and can be retried."
      />
      {!custom && gateway.models.length > 1 && (
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          <Text size={200}>Default model</Text>
          <Select
            aria-label="Default model"
            value={defaultModel}
            onChange={(event) => setDefaultModel(event.target.value)}
            disabled={update.isPending}
          >
            {gateway.models.map((model) => (
              <option key={model} value={model}>
                {model}
              </option>
            ))}
          </Select>
          <Button
            size="small"
            disabled={update.isPending || !defaultModel || defaultModel === gateway.defaultModel}
            onClick={() => update.mutate({ uid: instance.uid, resourceVersion: instance.resourceVersion, model: defaultModel })}
          >
            {update.isPending ? "Saving..." : "Save"}
          </Button>
          {update.error && (
            <Text size={200} style={{ color: tokens.colorStatusDangerForeground1 }}>
              {(update.error as Error).message}
            </Text>
          )}
        </div>
      )}

      {copilot && (
        <div style={{ display: "flex", flexDirection: "column", gap: 6, marginTop: 4 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <Text weight="semibold">GitHub Copilot</Text>
            {phase && <Badge appearance="filled" color={phase.color}>{phase.label}</Badge>}
          </div>
          {copilotPhase === "Starting" && (
            <Spinner size="tiny" label="The agent's Copilot proxy is starting with your sign-in..." labelPosition="after" />
          )}
          {copilotPhase === "Degraded" && copilot.reason && (
            <Text size={200} style={{ color: tokens.colorStatusDangerForeground1 }}>
              {COPILOT_WITHHELD.get(copilot.reason) ??
                `Copilot models could not be registered (${copilot.reason}). Signing in again may help.`}
            </Text>
          )}
          {copilot.shared && copilot.shared.length > 0 && (
            <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
              Also served by your Copilot subscription: {copilot.shared.join(", ")}
            </Text>
          )}
          {copilot.unpriced && copilot.unpriced.length > 0 && (
            <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
              Not offered because the Gateway has no price for them: {copilot.unpriced.join(", ")}
            </Text>
          )}
          {copilot.skipped && copilot.skipped.length > 0 && (
            <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
              Not offered because a shared model already has their name: {copilot.skipped.join(", ")}
            </Text>
          )}
          {copilot.notAllowed && copilot.notAllowed.length > 0 && (
            <Text size={200} style={{ color: tokens.colorNeutralForeground3 }}>
              Not offered because the Agent Policy does not allow these models: {copilot.notAllowed.join(", ")}
            </Text>
          )}
          <div>
            <Button
              appearance={copilotPhase === "SignInRequired" ? "primary" : "secondary"}
              onClick={() => void login.start()}
              disabled={login.busy || pending || instance.phase === "Deleting"}
            >
              {login.busy ? "Starting..." : copilotPhase === "SignInRequired" ? "Sign in with GitHub" : "Sign in again"}
            </Button>
          </div>
          {login.error && !dialogOpen && (
            <Text size={200} style={{ color: tokens.colorStatusDangerForeground1 }}>{login.error}</Text>
          )}
        </div>
      )}

      <Dialog open={dialogOpen} onOpenChange={(_, data) => { if (!data.open) closeDialog(); }}>
        <DialogSurface>
          <DialogBody>
            <DialogTitle>Sign in to GitHub Copilot</DialogTitle>
            <DialogContent>
              {pending && login.login?.userCode && (
                <div style={{ display: "flex", flexDirection: "column", gap: 12, paddingTop: 8 }}>
                  <Text>Open GitHub, enter this code and authorize the sign-in:</Text>
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <Text style={{ fontFamily: "monospace", fontSize: 18, letterSpacing: 2 }}>{login.login.userCode}</Text>
                    <Button
                      appearance="subtle"
                      size="small"
                      icon={copied ? <Checkmark20Regular /> : <Copy20Regular />}
                      onClick={handleCopy}
                      aria-label="Copy code"
                    />
                  </div>
                  <div>
                    <Button
                      appearance="primary"
                      icon={<Open20Regular />}
                      onClick={() => window.open(login.login!.verificationUri, "_blank", "noopener,noreferrer")}
                    >
                      Open GitHub
                    </Button>
                  </div>
                  <Spinner size="tiny" label="Waiting for authorization..." labelPosition="after" />
                  {login.error && (
                    <Text size={200} style={{ color: tokens.colorStatusWarningForeground1 }}>{login.error}</Text>
                  )}
                </div>
              )}
              {!pending && login.login && (
                <Text style={{ display: "block", paddingTop: 8 }}>{login.login.message}</Text>
              )}
            </DialogContent>
            <DialogActions>
              {!pending && login.login?.state !== "succeeded" && (
                <Button appearance="primary" onClick={() => void login.start()} disabled={login.busy}>
                  Try again
                </Button>
              )}
              <Button appearance="secondary" onClick={closeDialog}>
                {pending ? "Cancel" : "Close"}
              </Button>
            </DialogActions>
          </DialogBody>
        </DialogSurface>
      </Dialog>
    </div>
  );
}
