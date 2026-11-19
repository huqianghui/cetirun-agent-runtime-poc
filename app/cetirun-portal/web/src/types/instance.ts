// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { AgentPolicyRateLimits } from "./rateLimits";

export interface TeamSummary {
  name: string;
  role: string;
}

export type AgentType = "openclaw" | "hermes" | "custom";

export interface AgentConfig {
  type: AgentType;
  image?: string;
  username?: string;
  port?: number;
  webUI?: boolean;
  registrySecretRef?: { name: string };
}

export interface InstanceVersion {
  uid: string;
  resourceVersion: string;
}

export interface RestartInstanceRequest extends InstanceVersion {
  requestId: string;
}

export interface SetInstanceSuspendedRequest extends InstanceVersion {
  /** The displayed configuration generation; status-only changes do not advance it. */
  generation?: number;
  suspended: boolean;
}

export interface RegistryAuth {
  username: string;
  password: string;
}

export interface Instance extends InstanceVersion {
  instanceName: string;
  agent: AgentConfig;
  registryAuthConfigured?: boolean;
  generation?: number;
  observedGeneration?: number;
  suspended?: boolean;
  suspendAvailable?: boolean;
  suspensionUnavailableReason?: string;
  resumeAvailable?: boolean;
  restartAvailable?: boolean;
  restartRequestId?: string;
  observedRestartRequestId?: string;
  alias: string;
  displayName: string;
  team: string;
  /** Per-instance GitHub Copilot model override; empty means deployment default. */
  model?: string;
  phase: "Pending" | "Provisioning" | "Ready" | "Failed" | "Deleting" | "Suspended";
  accessUrl?: string;              // Gateway access URL; may have token appended when available
  webUIUrl?: string;
  gatewayToken?: string;           // Token for gateway access, returned separately when available
  apiServerKey?: string;           // Hermes API server key, returned separately when available
  tailscaleAuthKey?: string;       // Tailscale auth key, returned when Tailscale is enabled
  conditions?: ConditionStatus[];
  governance?: GovernanceStatus;
  createdAt: string;
  lastReconciled?: string;
  upgradeAvailable: boolean;
  agentIdentity?: {
    enabled: boolean;
    phase?: "Disabled" | "Provisioning" | "Ready" | "Degraded" | "Failed" | "Deleting";
    reason?: string;
  };
  /** The Agent's models in the platform AI Gateway; absent when it has none. */
  aiGateway?: AIGatewayInfo;
  /** The Skill Marketplace skills the Agent's Team selects; absent when there are none. */
  teamSkills?: TeamSkill[];
  /** The Agent runs an earlier release of its Team's skills; a restart copies the current one. */
  teamSkillsRestartNeeded?: boolean;
  /** The Skill Marketplace could not publish the Team's skills; administrators see the error on the Skills page. */
  teamSkillsPublicationFailed?: boolean;
}

/**
 * published: the release of the Team's current selection has the skill, at
 * the repository's version. updating: that release has another version than
 * the repository, which is not published for the Team yet. pending: the
 * release for the Team's current selection is not published yet. missing:
 * the skill was never published. retained: the repository's copy is no
 * longer valid, and the release keeps the last published version.
 */
export type TeamSkillState = "published" | "updating" | "pending" | "missing" | "retained";

/** One Skill Marketplace skill of the Agent's Team. */
export interface TeamSkill {
  name: string;
  description?: string;
  /** The version in the Team's release, which the Agent copies at its start. */
  version?: string;
  /** The repository's version, when the Team's release does not have it. */
  repositoryVersion?: string;
  state: TeamSkillState;
}

export type CopilotProxyPhase = "SignInRequired" | "Starting" | "Ready" | "Degraded";

export interface AIGatewayInfo {
  /** Whether the Agent's model traffic goes through the Gateway. */
  routed: boolean;
  phase?: string;
  reason?: string;
  sharedModels: boolean;
  githubCopilot: boolean;
  /** Models the Agent is configured with, as it names them. */
  models: string[];
  defaultModel?: string;
  copilot?: {
    phase?: CopilotProxyPhase;
    reason?: string;
    models: string[];
    /** Account models left out because the Gateway has no price for them. */
    unpriced?: string[];
    /** Shared models the account serves too; requests for them may go to either. */
    shared?: string[];
    /** Account models left out because a shared model already has their name. */
    skipped?: string[];
    /** Account models left out because the Agent Policy does not allow the shared models that are the same models. */
    notAllowed?: string[];
  };
  /** The Team's smart-routing models for this Agent: usable or why not. */
  modelRouters?: { name: string; granted: boolean; reason?: string }[];
  /** Limits the Agent's policy sets on its key; absent when there are none. */
  rateLimits?: AgentPolicyRateLimits;
}

export interface ModelAccess {
  sharedModels?: boolean;
  githubCopilot?: boolean;
}

export interface ConditionStatus {
  type: string;
  status: "True" | "False" | "Unknown";
  reason: string;
  message: string;
  observedGeneration?: number;
}

/** A current governance observation selected by the Portal API. */
export interface GovernanceStatus {
  category: "policy" | "identity" | "platform" | "dependency" | "pending";
  reason: string;
  message: string;
  condition: string;
  observedGeneration: number;
}

export interface CreateInstanceRequest {
  instanceName?: string;
  agent: Omit<AgentConfig, "registrySecretRef" | "type"> & { type?: AgentType };
  registryAuth?: RegistryAuth;
  requestId?: string;
  displayName?: string;
  team?: string;
  gatewayToken?: string;
  tailscaleAuthKey?: string;
  model?: string;
  agentIdentityEnabled?: boolean;
  /** Gateway model sources for every Agent harness. */
  modelAccess?: ModelAccess;
}

export interface UpdateInstanceRequest extends InstanceVersion {
  agent?: {
    type?: AgentType;
    image?: string | null;
    username?: string;
    port?: number | null;
    webUI?: boolean | null;
  };
  displayName?: string;
  team?: string;
  gatewayToken?: string;
  tailscaleAuthKey?: string; // "" = disable tailscale
  model?: string; // "" = use deployment default
}

export interface StatusResponse {
  conditions: ConditionStatus[];
  events: EventItem[];
  nsEvents: EventItem[];
  pods: PodStatus[];
}

export interface EventItem {
  type: string;
  reason: string;
  message: string;
  source: string;
  firstSeen: string;
  lastSeen: string;
  count: number;
}

export interface PodStatus {
  name: string;
  phase: string;
  ready: boolean;
  restartCount: number;
}
