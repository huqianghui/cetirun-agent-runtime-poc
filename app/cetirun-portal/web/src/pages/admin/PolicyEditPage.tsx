// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useLayoutEffect, useRef, useState, type FormEvent } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button, Combobox, Divider, Field, Input, MessageBar, Option, Select, Spinner, Switch, Text, Textarea, Title2 } from "@fluentui/react-components";
import {
  createPolicy, getPolicy, updatePolicy, PolicyConflictError, type AgentPolicyDTO,
  type AgentPolicyPort, type AgentPolicyProtocol, type AgentPolicyUpsertRequest, type PolicyHarness,
} from "../../api/adminPolicy";
import {
  emptyRateLimitForm,
  MAX_MODEL_LIMITS,
  rateLimitFormFrom,
  rateLimitsFromForm,
  TPM_HINT,
  type RateLimitForm,
  type RateLimitRow,
} from "../../utils/rateLimits";
import { InternalNetworkRules, formatInternalNetworkRules, parseInternalNetworkRules, validCIDR, type InternalNetworkRule } from "./InternalNetworkRules";
import { useFeatures } from "../../hooks/useFeatures";
import { useSkillMarketplace } from "../../hooks/useSkillMarketplace";
import { SkillChecklist } from "./SkillChecklist";
import { listTeams } from "../../api/adminTeam";
import { useAIGatewayCatalog } from "../../hooks/useAIGateway";
import { AdminConfigurationErrorMessage } from "../../components/AdminConfigurationErrorMessage";

export interface PolicyFormState {
  name: string; uid: string; resourceVersion: string; openclaw: boolean; hermes: boolean; custom: boolean; skills: string;
  copilot: boolean; sharedModels: string; budgetEnabled: boolean; defaultMonthlyUSD: string; maxMonthlyUSD: string;
  allowMultiple: boolean; readOnlyRootFilesystem: boolean; blockedCIDRs: string; blockedPorts: AgentPolicyPort[];
  allowedInternalCIDRs: InternalNetworkRule[]; ingressMode: "authenticated" | "disabled"; limits: RateLimitForm;
  agentIdentityMode: "disabled" | "optional" | "required"; entraIDEnabled: boolean;
}

const emptyForm: PolicyFormState = {
  name: "", uid: "", resourceVersion: "", openclaw: true, hermes: false, custom: false, skills: "", copilot: false,
  sharedModels: "", budgetEnabled: false, defaultMonthlyUSD: "", maxMonthlyUSD: "", allowMultiple: false,
  readOnlyRootFilesystem: true, blockedCIDRs: "", blockedPorts: [], allowedInternalCIDRs: [], ingressMode: "authenticated", limits: emptyRateLimitForm,
  agentIdentityMode: "disabled", entraIDEnabled: false,
};

function lines(value: string): string[] { return value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean); }

/** The Allowed skills text with one skill name added at the end or removed; other lines stay as written. */
export function toggleAllowedSkill(text: string, name: string, checked: boolean): string {
  const kept = text.split(/\r?\n/).filter((line) => line.trim() !== name);
  if (!checked) return kept.join("\n");
  while (kept.length > 0 && kept[kept.length - 1].trim() === "") kept.pop();
  return [...kept, name].join("\n");
}

function SharedModelPicker({ policyName, value, onChange }: { policyName?: string; value: string; onChange: (models: string[]) => void }) {
  const catalog = useAIGatewayCatalog(true);
  const teams = useQuery({ queryKey: ["admin", "teams"], queryFn: listTeams, enabled: !!policyName });
  const [search, setSearch] = useState("");
  const known = catalog.data?.known === true;
  const routerDetails = new Map<string, string[]>();
  for (const team of teams.data ?? []) {
    if (!policyName || team.deleting || team.agentPolicyRef !== policyName) continue;
    for (const router of team.modelRouters ?? []) {
      const registered = team.modelRouterStatus?.find((status) => status.name === router.name)?.ready === true;
      routerDetails.set(router.name, [...(routerDetails.get(router.name) ?? []),
        `Team ${team.name}: ${registered ? "registered" : "registration pending"}`]);
    }
  }
  const models = [...new Set([
    ...(known ? catalog.data!.sharedModels.map((model) => model.name) : []), ...routerDetails.keys(),
  ])].sort((a, b) => a.localeCompare(b));
  const selected = lines(value);
  const filtered = models.filter((model) => model.toLowerCase().includes(search.toLowerCase()));

  return <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
    <Field label="Allowed shared models" hint="Search shared models and routers from Teams linked to this Policy. Router tiers still require their own grants. These models also decide the owners' GitHub Copilot models that are the same models: a Copilot model is offered only when this list allows a shared model that is that model.">
      <Combobox multiselect selectedOptions={selected} value={search} placeholder="Search models"
        disabled={models.length === 0} style={{ width: "100%" }}
        onChange={(event) => setSearch(event.target.value)}
        onOptionSelect={(_, data) => { onChange(data.selectedOptions); setSearch(""); }}>
        {filtered.map((model) => <Option key={model} value={model} text={model}>
          {model}{routerDetails.has(model) && <Text block size={200}>{routerDetails.get(model)!.join("; ")}</Text>}
        </Option>)}
        {filtered.length === 0 && <Option disabled>No models match your search</Option>}
      </Combobox>
    </Field>
    {selected.length > 0 && <ul aria-label="Selected shared models" style={{ listStyle: "none", margin: 0, padding: 0 }}>
      {selected.map((model) => <li key={model} style={{ display: "flex", alignItems: "center", gap: 8, paddingBlock: 4 }}>
        <div style={{ flex: 1, minWidth: 0, overflowWrap: "anywhere" }}>
          <Text>{model}</Text>
          {routerDetails.has(model) && <Text block size={200}>{routerDetails.get(model)!.join("; ")}</Text>}
          {known && !models.includes(model) && <Text block size={200}>Not listed in the current catalog</Text>}
        </div>
        <Button type="button" appearance="subtle" aria-label={`Remove ${model}`}
          onClick={() => onChange(selected.filter((name) => name !== model))}>Remove</Button>
      </li>)}
    </ul>}
    {catalog.isPending && <Spinner size="tiny" label="Loading models..." />}
    {catalog.isError && <MessageBar intent="warning">Failed to load the model catalog. Your selections have been kept. Try refreshing.</MessageBar>}
    {!catalog.isPending && !catalog.isError && !known && <Text>The Gateway model catalog is not available yet.</Text>}
    {known && models.length === 0 && <Text>No shared models are listed in the Gateway catalog.</Text>}
    {policyName && teams.isPending && <Spinner size="tiny" label="Loading Team routers..." />}
    {policyName && teams.isError && <MessageBar intent="warning">Failed to load Team routers. Your selections have been kept. Try refreshing.</MessageBar>}
    <Button type="button" style={{ alignSelf: "flex-start" }} disabled={catalog.isFetching || (!!policyName && teams.isFetching)}
      onClick={() => { void catalog.refetch(); if (policyName) void teams.refetch(); }}>Refresh models</Button>
  </div>;
}

export function fromPolicyDTO(policy: AgentPolicyDTO): PolicyFormState {
  return {
    name: policy.name, uid: policy.uid, resourceVersion: policy.resourceVersion,
    openclaw: policy.harnesses.allowed.includes("openclaw"), hermes: policy.harnesses.allowed.includes("hermes"),
    custom: policy.harnesses.allowed.includes("custom"),
    skills: (policy.skills?.allowed || []).join("\n"), copilot: !!policy.modelAccess?.copilot?.allowed,
    sharedModels: (policy.modelAccess?.sharedModels?.allowed || []).join("\n"), budgetEnabled: !!policy.modelAccess?.gatewayBudget,
    defaultMonthlyUSD: policy.modelAccess?.gatewayBudget?.defaultMonthlyUSD || "", maxMonthlyUSD: policy.modelAccess?.gatewayBudget?.maxMonthlyUSD || "",
    allowMultiple: !!policy.sandboxSelection?.allowMultiple, readOnlyRootFilesystem: policy.security?.readOnlyRootFilesystem ?? true,
    blockedCIDRs: (policy.network?.blockedCIDRs || []).join("\n"), blockedPorts: policy.network?.blockedPorts || [],
    allowedInternalCIDRs: formatInternalNetworkRules(policy.network?.allowedInternalCIDRs || []), ingressMode: policy.network?.ingress?.mode || "authenticated",
    limits: rateLimitFormFrom(policy.modelAccess?.rateLimits),
    agentIdentityMode: policy.agentIdentity?.required ? "required" : policy.agentIdentity?.allowed ? "optional" : "disabled",
    entraIDEnabled: !!policy.entraID?.enabled,
  };
}

export function toPolicyRequest(form: PolicyFormState, baseline?: PolicyFormState): AgentPolicyUpsertRequest {
  const allowed: PolicyHarness[] = [];
  if (form.openclaw) allowed.push("openclaw");
  if (form.hermes) allowed.push("hermes");
  if (form.custom) allowed.push("custom");
  const request: AgentPolicyUpsertRequest = {
    name: form.name.trim(), uid: form.uid || undefined, resourceVersion: form.resourceVersion || undefined,
    harnesses: { allowed }, skills: { allowed: lines(form.skills) },
    modelAccess: { copilot: { allowed: form.copilot }, sharedModels: { allowed: lines(form.sharedModels) } },
    sandboxSelection: { allowMultiple: form.allowMultiple }, security: { readOnlyRootFilesystem: form.readOnlyRootFilesystem },
    agentIdentity: {
      allowed: form.agentIdentityMode !== "disabled", required: form.agentIdentityMode === "required",
      credentialServices: form.agentIdentityMode === "disabled" ? [] : ["azure-resource-manager"],
    },
    entraID: { enabled: form.entraIDEnabled },
  };
  // Omit untouched network fields before parsing. Stored legacy lists may
  // contain duplicates that must not block unrelated edits or be rewritten.
  const network: NonNullable<AgentPolicyUpsertRequest["network"]> = {};
  if (!baseline || form.blockedCIDRs !== baseline.blockedCIDRs) network.blockedCIDRs = lines(form.blockedCIDRs);
  if (!baseline || JSON.stringify(form.blockedPorts) !== JSON.stringify(baseline.blockedPorts)) network.blockedPorts = form.blockedPorts;
  if (!baseline || JSON.stringify(form.allowedInternalCIDRs) !== JSON.stringify(baseline.allowedInternalCIDRs)) {
    network.allowedInternalCIDRs = parseInternalNetworkRules(form.allowedInternalCIDRs);
  }
  if (!baseline || form.ingressMode !== baseline.ingressMode) network.ingress = { mode: form.ingressMode };
  if (Object.keys(network).length > 0) request.network = network;
  if (form.budgetEnabled) request.modelAccess!.gatewayBudget = { defaultMonthlyUSD: form.defaultMonthlyUSD.trim(), maxMonthlyUSD: form.maxMonthlyUSD.trim() };
  return request;
}

function validPort(port: AgentPolicyPort): boolean { return Number.isInteger(port.port) && port.port >= 1 && port.port <= 65535 && ["TCP", "UDP", "SCTP"].includes(port.protocol); }
function updateAt<T>(items: T[], index: number, value: T): T[] { return items.map((item, itemIndex) => itemIndex === index ? value : item); }

function validateRequest(request: AgentPolicyUpsertRequest, isCreate: boolean): string | null {
  if (request.harnesses.allowed.length === 0) return "Choose at least one harness.";
  const access = request.modelAccess;
  const hasModelEntitlement = !!access?.copilot?.allowed || (access?.sharedModels?.allowed?.length || 0) > 0;
  if (hasModelEntitlement && !access?.gatewayBudget) return "Model access requires a monthly Gateway budget.";
  if (!hasModelEntitlement && access?.gatewayBudget) return "A Gateway budget requires Copilot or a shared model.";
  if (isCreate && !/^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/.test(request.name || "")) return "Name must be a DNS-1123 label: lowercase, start with a letter, max 63 chars.";
  const network = request.network;
  if (network?.blockedCIDRs?.some((cidr) => !validCIDR(cidr))) return "Blocked CIDRs must be valid IPv4 or IPv6 CIDR values.";
  const allPorts = [...(network?.blockedPorts || []), ...(network?.allowedInternalCIDRs || []).flatMap((rule) => rule.ports)];
  if (allPorts.some((port) => !validPort(port))) return "Ports must be 1–65535 and use TCP, UDP, or SCTP.";
  if (new Set((network?.blockedPorts || []).map((port) => `${port.port}/${port.protocol}`)).size !== (network?.blockedPorts || []).length) return "Blocked ports must be unique.";
  if (network?.allowedInternalCIDRs?.some((rule) => !validCIDR(rule.cidr) || rule.ports.length === 0)) return "Each internal CIDR rule needs a valid CIDR and at least one port.";
  return null;
}

export function PolicyEditPage() {
  const { name } = useParams<{ name: string }>();
  const isCreate = !name;
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [form, setForm] = useState<PolicyFormState>(emptyForm);
  const [submitError, setSubmitError] = useState<Error | null>(null);
  const baseline = useRef<PolicyFormState | null>(null);
  const activePolicyName = useRef(name);
  const [networkEditing, setNetworkEditing] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [reloading, setReloading] = useState(false);
  const { data: existing, isLoading, error, refetch } = useQuery<AgentPolicyDTO, Error>({
    queryKey: ["admin", "policy", name], queryFn: () => getPolicy(name!), enabled: !isCreate,
    refetchInterval: 30_000, refetchOnMount: "always",
  });
  const { data: features } = useFeatures();
  const marketplaceEnabled = !!features?.skillMarketplaceEnabled;
  const marketplace = useSkillMarketplace(marketplaceEnabled);
  useLayoutEffect(() => {
    activePolicyName.current = name;
    baseline.current = null;
    setForm(emptyForm);
    setSubmitError(null);
    setConflict(false);
    setReloading(false);
    setNetworkEditing(false);
  }, [name]);
  useLayoutEffect(() => {
    if (!existing || existing.name !== name || networkEditing) return;
    const previous = baseline.current;
    const pristine = !previous || JSON.stringify(form) === JSON.stringify(previous);
    if (pristine && (previous?.uid !== existing.uid || previous.resourceVersion !== existing.resourceVersion)) {
      const next = fromPolicyDTO(existing);
      baseline.current = next;
      setForm(next);
      setConflict(false);
      setSubmitError(null);
    }
  }, [existing, name, form, networkEditing]);
  const needsReload = !isCreate && (conflict || !!(existing && form.uid && (
    existing.uid !== form.uid || existing.resourceVersion !== form.resourceVersion
  )));
  const reload = async () => {
    setReloading(true);
    const result = await refetch();
    if (activePolicyName.current !== name) return;
    setReloading(false);
    if (result.isSuccess && result.data) {
      const next = fromPolicyDTO(result.data);
      baseline.current = next;
      setForm(next);
      setConflict(false);
      setSubmitError(null);
    } else {
      setSubmitError(new Error(`Failed to reload policy: ${result.error?.message || "Try again."}`));
    }
  };
  const success = () => { queryClient.invalidateQueries({ queryKey: ["admin", "policies"] }); if (name) queryClient.invalidateQueries({ queryKey: ["admin", "policy", name] }); navigate("/admin/policies"); };
  const createMutation = useMutation({ mutationFn: createPolicy, onSuccess: success, onError: (mutationError: Error) => setSubmitError(mutationError) });
  const updateMutation = useMutation({
    mutationFn: (request: AgentPolicyUpsertRequest) => updatePolicy(name!, request), onSuccess: success,
    onError: (mutationError: Error) => {
      setSubmitError(mutationError);
      if (mutationError instanceof PolicyConflictError) setConflict(true);
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (needsReload || reloading) return;
    setSubmitError(null);
    let request: AgentPolicyUpsertRequest;
    try {
      request = toPolicyRequest(form, isCreate ? undefined : baseline.current ?? undefined);
    } catch (error) {
      setSubmitError(error instanceof Error ? error : new Error("Check the internal network rules."));
      return;
    }
    const validationError = validateRequest(request, isCreate);
    if (validationError) { setSubmitError(new Error(validationError)); return; }
    const limits = rateLimitsFromForm(form.limits);
    if (limits.error) { setSubmitError(new Error(limits.error)); return; }
    if (limits.limits) request.modelAccess!.rateLimits = limits.limits;
    if (isCreate) createMutation.mutate(request); else updateMutation.mutate(request);
  };
  const setLimits = (limits: Partial<RateLimitForm>) => setForm({ ...form, limits: { ...form.limits, ...limits } });
  const setRow = (index: number, row: Partial<RateLimitRow>) =>
    setLimits({ perModel: form.limits.perModel.map((current, i) => (i === index ? { ...current, ...row } : current)) });
  if (!isCreate && isLoading) return <Spinner label="Loading policy..." />;
  if (reloading) return <Spinner label="Reloading policy..." />;
  if (!isCreate && error && !existing) return <MessageBar intent="error">Failed to load: {error.message}</MessageBar>;
  const pending = createMutation.isPending || updateMutation.isPending;
  const readonlyNetwork = existing?.network;
  const allowedSkills = lines(form.skills);
  return <form onSubmit={submit} style={{ maxWidth: 820, display: "flex", flexDirection: "column", gap: 16 }}>
    <Title2>{isCreate ? "New agent policy" : `Agent policy: ${name}`}</Title2>
    <Text>Configure the harnesses, skills, model access, and Sandbox controls that Agents may use.</Text>
    <Field label="Name" required><Input value={form.name} disabled={!isCreate} onChange={(_, data) => setForm({ ...form, name: data.value })} placeholder="e.g. standard" /></Field>
    <Divider /><Text weight="semibold">Allowed harnesses</Text>
    <Switch checked={form.openclaw} label="OpenClaw" onChange={(_, data) => setForm({ ...form, openclaw: data.checked })} />
    <Switch checked={form.hermes} label="Hermes" onChange={(_, data) => setForm({ ...form, hermes: data.checked })} />
    <Switch checked={form.custom} label="Custom" onChange={(_, data) => setForm({ ...form, custom: data.checked })} />
    <Field label="Allowed skills" hint="Enter one skill identifier per line; commas are not separators. Leave empty to allow no Team or Agent skills."><Textarea rows={4} value={form.skills} onChange={(_, data) => setForm({ ...form, skills: data.value })} placeholder={"skill-a\nnpm:@scope/skill-b\nskill-c@1.2.0"} /></Field>
    {marketplaceEnabled && <section aria-label="Skill Marketplace skills" style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <Text size={300} weight="semibold">Skill Marketplace skills</Text>
      <Text size={200}>Checking a skill adds its name to Allowed skills; unchecking removes it. Other entries, such as npm packages, stay in the list above. A skill that a Team using this policy selects cannot be removed until the Team drops it.</Text>
      {marketplace.error && <MessageBar intent="error">Failed to load the skill catalog: {marketplace.error.message}</MessageBar>}
      {marketplace.isLoading && <Spinner size="tiny" label="Loading the skill catalog..." />}
      {marketplace.data && !marketplace.data.catalog && <Text size={200}>The skill catalog is not available yet.</Text>}
      {marketplace.data?.catalog && <SkillChecklist label="Allowed marketplace skills"
        choices={(marketplace.data.catalog.skills ?? []).map((skill) => ({
          name: skill.name, description: skill.description, version: skill.version, updatedAt: skill.updatedAt, checked: allowedSkills.includes(skill.name),
        }))}
        emptyText="The skills repository has no valid skills yet."
        onToggle={(skill, checked) => setForm({ ...form, skills: toggleAllowedSkill(form.skills, skill, checked) })} />}
    </section>}
    <Divider /><Text weight="semibold">AI Gateway</Text>
    <Switch checked={form.copilot} label="Allow GitHub Copilot" onChange={(_, data) => {
      const copilot = data.checked;
      setForm({ ...form, copilot, budgetEnabled: copilot || form.sharedModels.trim() !== "" ? form.budgetEnabled : false });
    }} />
    <SharedModelPicker key={`models-${name || "__new__"}`} policyName={name} value={form.sharedModels} onChange={(models) => {
      const sharedModels = models.join("\n");
      setForm({ ...form, sharedModels, budgetEnabled: form.copilot || sharedModels.trim() !== "" ? form.budgetEnabled : false });
    }} />
    <Switch checked={form.budgetEnabled} label="Set monthly Gateway budget" onChange={(_, data) => setForm({ ...form, budgetEnabled: data.checked })} />
    {form.budgetEnabled && <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}><Field label="Default monthly USD" required><Input value={form.defaultMonthlyUSD} onChange={(_, data) => setForm({ ...form, defaultMonthlyUSD: data.value })} placeholder="e.g. 20" /></Field><Field label="Maximum monthly USD" required><Input value={form.maxMonthlyUSD} onChange={(_, data) => setForm({ ...form, maxMonthlyUSD: data.value })} placeholder="e.g. 50" /></Field></div>}
    <Divider /><Text weight="semibold">AI Gateway limits per Agent</Text>
    <Text size={200}>Every Agent of a Team that uses this policy gets these limits on its own Gateway key; Agents do not share them. Leave a field empty for no limit. Changes to limits alone update each Agent's existing key without restarting running Agents. Budget, model access, and Sandbox control changes retain their normal enforcement checks.</Text>
    <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 12 }}>
      <Field label="Max parallel requests" hint="At least 1."><Input inputMode="numeric" value={form.limits.maxParallelRequests} onChange={(_, data) => setLimits({ maxParallelRequests: data.value })} placeholder="No limit" /></Field>
      <Field label="Requests per minute" hint="At least 1."><Input inputMode="numeric" value={form.limits.rpm} onChange={(_, data) => setLimits({ rpm: data.value })} placeholder="No limit" /></Field>
      <Field label="Tokens per minute" hint="At least 4,096."><Input inputMode="numeric" value={form.limits.tpm} onChange={(_, data) => setLimits({ tpm: data.value })} placeholder="No limit" /></Field>
    </div>
    <Text size={200}>{TPM_HINT}</Text>
    <Text size={200}>Total limits count every request of the key, including GitHub Copilot models. A per-model limit applies to the exact model name the Agent requests; for an auto router that is the router's name, not the model it picks.</Text>
    {form.limits.perModel.map((row, index) => (
      <div key={index} style={{ display: "grid", gridTemplateColumns: "2fr 1fr 1fr auto", gap: 8, alignItems: "center" }}>
        <Input aria-label={`Model name ${index + 1}`} value={row.model} onChange={(_, data) => setRow(index, { model: data.value })} placeholder="Gateway model name" />
        <Input aria-label={`Requests per minute for model ${index + 1}`} inputMode="numeric" value={row.rpm} onChange={(_, data) => setRow(index, { rpm: data.value })} placeholder="RPM" />
        <Input aria-label={`Tokens per minute for model ${index + 1}`} inputMode="numeric" value={row.tpm} onChange={(_, data) => setRow(index, { tpm: data.value })} placeholder="TPM" />
        <Button type="button" appearance="subtle" onClick={() => setLimits({ perModel: form.limits.perModel.filter((_, i) => i !== index) })}>Remove</Button>
      </div>
    ))}
    <div>
      <Button type="button" disabled={form.limits.perModel.length >= MAX_MODEL_LIMITS} onClick={() => setLimits({ perModel: [...form.limits.perModel, { model: "", rpm: "", tpm: "" }] })}>Add per-model limit</Button>
    </div>
    <Divider />
    <section aria-label="Identity" style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <Text weight="semibold">Identity</Text>
      <Field label="Agent Identity" hint="An independent Microsoft Entra directory identity for access through Connections. Optional lets users choose; Required enables it for every new Agent.">
        <Select value={form.agentIdentityMode} onChange={(event) => setForm({ ...form, agentIdentityMode: event.target.value as PolicyFormState["agentIdentityMode"] })}>
          <option value="disabled">Disabled</option><option value="optional">Optional</option><option value="required">Required</option>
        </Select>
      </Field>
      {form.agentIdentityMode !== "disabled" && <Text size={200}>Credential service: Azure Resource Manager. The platform must have Agent Identity and Connections configured.</Text>}
      <Switch checked={form.entraIDEnabled} label="Entra workload identity" onChange={(_, data) => setForm({ ...form, entraIDEnabled: data.checked })} />
      <Text size={200}>Provides a token and Azure SDK configuration to the main application container. Requires a platform-approved identity and federation. This is independent of Agent Identity.</Text>
    </section>
    <Divider /><Text weight="semibold">Sandbox selection and security</Text>
    <Switch checked={form.allowMultiple} label="Allow multiple Sandboxes per Team" onChange={(_, data) => setForm({ ...form, allowMultiple: data.checked })} />
    <Switch checked={form.readOnlyRootFilesystem} label="Use a read-only root filesystem" onChange={(_, data) => setForm({ ...form, readOnlyRootFilesystem: data.checked })} />
    <Divider /><Text weight="semibold">Network</Text>
    <Field label="Blocked CIDRs" hint="One IPv4 or IPv6 CIDR per line."><Textarea rows={3} value={form.blockedCIDRs} onChange={(_, data) => setForm({ ...form, blockedCIDRs: data.value })} placeholder={"10.0.0.0/8\n2001:db8::/32"} /></Field>
    <Text size={300} weight="semibold">Blocked ports</Text>
    {form.blockedPorts.map((port, index) => <div key={index} style={{ display: "flex", gap: 8 }}><Input aria-label={`Blocked port ${index + 1}`} type="number" value={String(port.port)} onChange={(_, data) => setForm({ ...form, blockedPorts: updateAt(form.blockedPorts, index, { ...port, port: Number(data.value) }) })} /><Select aria-label={`Blocked port protocol ${index + 1}`} value={port.protocol} onChange={(event) => setForm({ ...form, blockedPorts: updateAt(form.blockedPorts, index, { ...port, protocol: event.target.value as AgentPolicyProtocol }) })}><option>TCP</option><option>UDP</option><option>SCTP</option></Select><Button type="button" onClick={() => setForm({ ...form, blockedPorts: form.blockedPorts.filter((_, itemIndex) => itemIndex !== index) })}>Remove</Button></div>)}
    <Button type="button" onClick={() => setForm({ ...form, blockedPorts: [...form.blockedPorts, { port: 443, protocol: "TCP" }] })}>Add blocked port</Button>
    <InternalNetworkRules key={name || "__new__"} rules={form.allowedInternalCIDRs}
      onEditingChange={setNetworkEditing}
      onChange={(allowedInternalCIDRs) => setForm({ ...form, allowedInternalCIDRs })} />
    <Field label="Ingress mode"><Select value={form.ingressMode} onChange={(event) => setForm({ ...form, ingressMode: event.target.value as PolicyFormState["ingressMode"] })}><option value="authenticated">Authenticated</option><option value="disabled">Disabled</option></Select></Field>
    <Divider />
    <section aria-label="Read-only policy capabilities" style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <Text weight="semibold">Read-only policy capabilities</Text>
      <Text size={200}>These values cannot be edited in the Portal. Saving other settings preserves them.</Text>
      <section aria-label="Kubernetes API access" style={{ display: "flex", flexDirection: "column", gap: 8, overflowWrap: "anywhere" }}>
        <Text weight="semibold">Kubernetes API access</Text>
        <Text size={200}>Managed by your platform administrator. This policy records requested access; authorization is configured separately.</Text>
        {existing?.kubernetesAccess ? <>
          <Text>Service account: {existing.kubernetesAccess.serviceAccountName}</Text>
          {!!existing.kubernetesAccess.roleBindings?.length && <>
            <Text weight="semibold" size={300}>Requested namespace permissions</Text>
            <ul style={{ margin: 0, paddingLeft: 24 }}>{existing.kubernetesAccess.roleBindings.map((binding, index) =>
              <li key={index}>ClusterRole {binding.clusterRoleName} in namespace {binding.namespace}</li>)}</ul>
          </>}
          {!!existing.kubernetesAccess.clusterRoleBindings?.length && <>
            <Text weight="semibold" size={300}>Requested cluster-wide permissions</Text>
            <ul style={{ margin: 0, paddingLeft: 24 }}>{existing.kubernetesAccess.clusterRoleBindings.map((binding, index) =>
              <li key={index}>ClusterRole {binding.clusterRoleName}</li>)}</ul>
          </>}
        </> : <Text>No Kubernetes API access is configured in this policy.</Text>}
      </section>
      <Text>Blocked FQDNs: {(readonlyNetwork?.blockedFQDNs || []).join(", ") || "none"}.</Text><Text>Ingress mTLS required: {String(!!readonlyNetwork?.ingress?.requireMTLS)}.</Text>
    </section>
    {submitError && <AdminConfigurationErrorMessage error={submitError} />}
    {needsReload && <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <MessageBar intent="warning">
        This policy changed since you opened it. Your unsaved edits have been kept. Reloading will discard them and load the latest policy.
      </MessageBar>
      <Button type="button" style={{ alignSelf: "flex-start" }} disabled={pending} onClick={reload}>Discard edits and reload</Button>
    </div>}
    <div style={{ display: "flex", gap: 8 }}><Button appearance="primary" type="submit" disabled={pending || needsReload}>{pending ? "Saving..." : isCreate ? "Create" : "Save"}</Button><Button appearance="secondary" type="button" onClick={() => navigate("/admin/policies")}>Cancel</Button></div>
  </form>;
}
