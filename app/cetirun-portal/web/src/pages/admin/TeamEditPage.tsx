// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Title2,
  Button,
  Spinner,
  MessageBar,
  MessageBarActions,
  MessageBarBody,
  Field,
  Input,
  Label,
  Textarea,
  Text,
  Divider,
  tokens,
  Select,
  Checkbox,
} from "@fluentui/react-components";
import {
  createTeam,
  getTeam,
  updateTeam,
  type TeamDTO,
  type TeamModelRouter,
  type TeamSkillStorage,
  type TeamUpsertRequest,
} from "../../api/adminTeam";

import { listPolicies } from "../../api/adminPolicy";
import { listSandboxes } from "../../api/adminSandbox";
import { getAIGatewayCatalog } from "../../api/aiGateway";
import { useFeatures } from "../../hooks/useFeatures";
import { useSkillMarketplace } from "../../hooks/useSkillMarketplace";
import { modelRouterProblem, TeamModelRoutersSection, toModelRouterRequests } from "./TeamModelRoutersSection";
import { mergeSkillSelection, npmSkillsProblem, splitTeamSkills, TeamSkillsSection } from "./TeamSkillsSection";
import { AdminConfigurationErrorMessage } from "../../components/AdminConfigurationErrorMessage";

interface FormState {
  uid: string;
  resourceVersion: string;
  agentPolicyRef: string;
  sandboxRefs: string[];
  defaultSandboxRef: string;
  name: string;
  role: string;
  description: string; // newline-separated
  skills: string; // newline-separated; edited without the Skill Marketplace
  marketplaceSkills: string[]; // Skill Marketplace names; edited with the marketplace
  npmSkills: string; // newline-separated npm: entries; edited with the marketplace
  storage: TeamSkillStorage;
  storageEnabled: boolean;
  modelRouters: TeamModelRouter[];
}

const emptyStorage: TeamSkillStorage = {
  resourceGroup: "",
  storageAccount: "",
  containerName: "",
  storageClassName: "",
  storageSize: "",
  mountPath: "",
};

function fromDTO(t: TeamDTO): FormState {
  const { marketplace, npm } = splitTeamSkills(t.skills);
  return {
    name: t.name,
    uid: t.uid, resourceVersion: t.resourceVersion,
    agentPolicyRef: t.agentPolicyRef, sandboxRefs: t.sandboxRefs, defaultSandboxRef: t.defaultSandboxRef,
    role: t.role || "",
    description: (t.description || []).join("\n"),
    skills: (t.skills || []).join("\n"),
    marketplaceSkills: marketplace,
    npmSkills: npm.join("\n"),
    storage: t.skillStorage || { ...emptyStorage },
    storageEnabled: !!t.skillStorage,
    modelRouters: (t.modelRouters || []).map((router) => ({ ...router })),
  };
}

function splitLines(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line.length > 0);
}

/** With the Skill Marketplace, the skills are the chosen marketplace names and the npm entries. */
function toRequest(s: FormState, marketplace: boolean): TeamUpsertRequest {
  const req: TeamUpsertRequest = {
    name: s.name.trim(),
    uid: s.uid || undefined, resourceVersion: s.resourceVersion || undefined,
    agentPolicyRef: s.agentPolicyRef, sandboxRefs: s.sandboxRefs, defaultSandboxRef: s.defaultSandboxRef,
    role: s.role.trim() || undefined,
    description: splitLines(s.description),
    skills: marketplace ? mergeSkillSelection(s.marketplaceSkills, splitLines(s.npmSkills)) : splitLines(s.skills),
  };
  if (s.modelRouters.length > 0) req.modelRouters = toModelRouterRequests(s.modelRouters);
  if (s.storageEnabled) {
    req.skillStorage = {
      resourceGroup: s.storage.resourceGroup.trim(),
      storageAccount: s.storage.storageAccount.trim(),
      containerName: s.storage.containerName.trim(),
      storageClassName: s.storage.storageClassName?.trim() || undefined,
      storageSize: s.storage.storageSize?.trim() || undefined,
      mountPath: s.storage.mountPath?.trim() || undefined,
    };
  }
  if (req.description?.length === 0) delete req.description;
  if (req.skills?.length === 0) delete req.skills;
  return req;
}

export function TeamEditPage() {
  const { name } = useParams<{ name: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const isCreate = !name;

  const [form, setForm] = useState<FormState>({
    name: "",
    uid: "", resourceVersion: "", agentPolicyRef: "", sandboxRefs: [], defaultSandboxRef: "",
    role: "",
    description: "",
    skills: "",
    marketplaceSkills: [],
    npmSkills: "",
    storage: { ...emptyStorage },
    storageEnabled: false,
    modelRouters: [],
  });
  const [submitError, setSubmitError] = useState<Error | null>(null);

  const { data: existing, isLoading, error } = useQuery<TeamDTO, Error>({
    queryKey: ["admin", "team", name],
    queryFn: () => getTeam(name!),
    enabled: !isCreate,
  });

  useEffect(() => {
    if (existing) setForm(fromDTO(existing));
  }, [existing]);

  const policiesQuery = useQuery({ queryKey: ["admin", "policies"], queryFn: listPolicies, retry: false });
  const sandboxesQuery = useQuery({ queryKey: ["admin", "sandboxes"], queryFn: listSandboxes, retry: false });
  // Tier and classifier pickers suggest the shared models that are not routers of the Gateway themselves,
  // which the operator refuses there; the decision classifier offers the Gateway's decision models. The
  // operator checks the routers.
  const catalogQuery = useQuery({ queryKey: ["ai-gateway", "catalog"], queryFn: getAIGatewayCatalog, retry: false });
  const sharedModelNames = (catalogQuery.data?.sharedModels || [])
    .filter((model) => !model.routerModels?.length && !model.routerUnverifiable)
    .map((model) => model.name);
  const decisionModelNames = catalogQuery.data ? catalogQuery.data.decisionModels || [] : undefined;
  const policies = policiesQuery.data || [];
  const sandboxes = (sandboxesQuery.data || []).filter(box => !box.deleting);
  const configurationError = policiesQuery.error || sandboxesQuery.error;
  const loadingConfiguration = policiesQuery.isFetching || sandboxesQuery.isFetching;
  const selectedPolicy = policies.find(policy => policy.name === form.agentPolicyRef);
  const allowsMultipleSandboxes = !!selectedPolicy?.sandboxSelection?.allowMultiple;
  const hasTooManySandboxes = !allowsMultipleSandboxes && form.sandboxRefs.length > 1;
  const validBindings = policies.some(policy => policy.name === form.agentPolicyRef) &&
    form.sandboxRefs.length > 0 && form.sandboxRefs.every(ref => sandboxes.some(box => box.name === ref)) &&
    form.sandboxRefs.includes(form.defaultSandboxRef) && !hasTooManySandboxes;
  // With the Skill Marketplace, Team skills are chosen from its catalog; npm packages stay a list.
  // Until the features are known, neither skills editor is shown, so no edit is lost when the editor changes.
  const { data: features } = useFeatures();
  const marketplaceEnabled = !!features?.skillMarketplaceEnabled;
  const marketplaceQuery = useSkillMarketplace(marketplaceEnabled);
  // A policy without an allowed list allows no skills.
  const allowedSkills = selectedPolicy ? selectedPolicy.skills?.allowed ?? [] : undefined;

  const onSuccess = () => {
    qc.invalidateQueries({ queryKey: ["teams"] });
    qc.invalidateQueries({ queryKey: ["instance-create-options"] });
    qc.invalidateQueries({ queryKey: ["admin", "teams"] });
    qc.invalidateQueries({ queryKey: ["admin", "skill-marketplace"] });
    if (name) qc.invalidateQueries({ queryKey: ["admin", "team", name] });
    navigate("/admin/teams");
  };

  const createMut = useMutation({
    mutationFn: (req: TeamUpsertRequest) => createTeam(req),
    onSuccess,
    onError: (e: Error) => setSubmitError(e),
  });
  const updateMut = useMutation({
    mutationFn: (req: TeamUpsertRequest) => updateTeam(name!, req),
    onSuccess,
    onError: (e: Error) => setSubmitError(e),
  });

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (createMut.isPending || updateMut.isPending) return;
    setSubmitError(null);
    if (loadingConfiguration || configurationError || !validBindings) {
      setSubmitError(new Error(hasTooManySandboxes
        ? "The selected Agent Policy allows one Sandbox per Team. Remove extra Sandboxes before saving."
        : "Choose an available Agent Policy, allowed Sandboxes, and a default Sandbox."));
      return;
    }
    const routerProblem = modelRouterProblem(form.modelRouters);
    if (routerProblem) {
      setSubmitError(new Error(routerProblem));
      return;
    }
    const skillsProblem = marketplaceEnabled ? npmSkillsProblem(splitLines(form.npmSkills)) : null;
    if (skillsProblem) {
      setSubmitError(new Error(skillsProblem));
      return;
    }
    const req = toRequest(form, marketplaceEnabled);
    if (isCreate) {
      if (!/^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/.test(req.name || "")) {
        setSubmitError(new Error(
          "Name must be a DNS-1123 label: lowercase, start with a letter, max 63 chars."
        ));
        return;
      }
      createMut.mutate(req);
    } else {
      updateMut.mutate(req);
    }
  };

  if (!isCreate && isLoading) return <Spinner label="Loading team..." />;
  if (!isCreate && error) {
    return <MessageBar intent="error">Failed to load: {error.message}</MessageBar>;
  }

  const pending = createMut.isPending || updateMut.isPending;

  return (
    <form onSubmit={onSubmit} style={{ maxWidth: 720, display: "flex", flexDirection: "column", gap: 16 }}>
      <Title2>{isCreate ? "New team" : `Team: ${name}`}</Title2>

      <Field label="Name" required>
        <Input
          value={form.name}
          disabled={!isCreate}
          onChange={(_, data) => setForm({ ...form, name: data.value })}
          placeholder="e.g. aks"
        />
      </Field>

      <Divider />
      <Text weight="semibold">Agent policy and Sandbox</Text>
      {loadingConfiguration && <Spinner label="Loading policies and Sandboxes..." />}
      {configurationError && <MessageBar intent="error">
        <MessageBarBody>{configurationError.message}</MessageBarBody>
        <MessageBarActions>
          <Button type="button" onClick={() => { void policiesQuery.refetch(); void sandboxesQuery.refetch(); }}>Retry configuration</Button>
        </MessageBarActions>
      </MessageBar>}
      {/* MessageBar lays its children out as a row; MessageBarBody keeps the sentence and its link together. */}
      {!loadingConfiguration && !configurationError && policies.length === 0 && <MessageBar intent="info"><MessageBarBody>Create an <Link to="/admin/policies/new">Agent Policy</Link> before creating this Team.</MessageBarBody></MessageBar>}
      {!loadingConfiguration && !configurationError && sandboxes.length === 0 && <MessageBar intent="info"><MessageBarBody>Create a <Link to="/admin/sandboxes/new">Sandbox</Link> before creating this Team.</MessageBarBody></MessageBar>}
      <Field label="Agent Policy" required hint="Controls the harnesses, Skills and model access available to this Team.">
        <Select value={form.agentPolicyRef} disabled={loadingConfiguration} onChange={event => setForm({ ...form, agentPolicyRef: event.target.value })}>
          <option value="">Choose a policy</option>
          {form.agentPolicyRef && !policies.some(policy => policy.name === form.agentPolicyRef) && <option value={form.agentPolicyRef} disabled>{form.agentPolicyRef} (unavailable)</option>}
          {policies.map(policy => <option key={policy.name} value={policy.name}>{policy.name}</option>)}
        </Select>
      </Field>
      {selectedPolicy && !allowsMultipleSandboxes && <Text size={300}>This policy allows one Sandbox per Team.</Text>}
      {hasTooManySandboxes && <MessageBar intent="warning">This Team keeps its existing Sandbox selections, but the selected policy allows only one. Remove extra Sandboxes before saving.</MessageBar>}
      <fieldset style={{ border: 0, padding: 0, margin: 0, display: "flex", flexDirection: "column", gap: tokens.spacingVerticalXXS }}>
        {/* A label like the other fields', rather than the browser's indented legend. */}
        <legend style={{ padding: 0, marginBottom: tokens.spacingVerticalXXS }}><Label required>Allowed Sandboxes</Label></legend>
        <Text size={200}>Reusable runtime, resource and storage settings. Manage them in Sandboxes.</Text>
        <div style={{ display: "flex", flexDirection: "column" }}>
          {[...new Set([...sandboxes.map(box => box.name), ...form.sandboxRefs])].map(ref => <Checkbox key={ref}
            label={sandboxes.some(box => box.name === ref) ? ref : `${ref} (unavailable)`}
            checked={form.sandboxRefs.includes(ref)} disabled={loadingConfiguration || (!allowsMultipleSandboxes && form.sandboxRefs.length > 0 && !form.sandboxRefs.includes(ref))}
            onChange={(_, data) => {
              const refs = data.checked ? [...form.sandboxRefs, ref] : form.sandboxRefs.filter(value => value !== ref);
              setForm({ ...form, sandboxRefs: refs, defaultSandboxRef: refs.includes(form.defaultSandboxRef) ? form.defaultSandboxRef : refs[0] || "" });
            }} />)}
        </div>
      </fieldset>
      <Field label="Default Sandbox" required hint="Used when a member creates an Agent.">
        <Select value={form.defaultSandboxRef} disabled={loadingConfiguration || form.sandboxRefs.length === 0} onChange={event => setForm({ ...form, defaultSandboxRef: event.target.value })}>
          <option value="">Choose a default Sandbox</option>
          {form.sandboxRefs.map(ref => <option key={ref} value={ref}>{ref}</option>)}
        </Select>
      </Field>
      <Text size={300}>Changing settings for a Team with Agents may require suspending those Agents first.</Text>

      <Field label="Role" hint="A short label describing the team's role.">
        <Input
          value={form.role}
          onChange={(_, data) => setForm({ ...form, role: data.value })}
          placeholder="e.g. AKS Engineer"
        />
      </Field>

      <Field
        label="Description"
        hint="One bullet per line. Used as additional context for team members."
      >
        <Textarea
          rows={4}
          value={form.description}
          onChange={(_, data) => setForm({ ...form, description: data.value })}
        />
      </Field>

      <Divider />
      {marketplaceEnabled ? (
        <>
          {marketplaceQuery.error && <MessageBar intent="error">
            <MessageBarBody>Failed to load the skill catalog: {marketplaceQuery.error.message}</MessageBarBody>
            <MessageBarActions>
              <Button type="button" onClick={() => void marketplaceQuery.refetch()}>Retry skill catalog</Button>
            </MessageBarActions>
          </MessageBar>}
          {marketplaceQuery.isLoading ? <Spinner size="tiny" label="Loading the skill catalog..." /> : (
            <TeamSkillsSection
              value={form.marketplaceSkills}
              allowed={allowedSkills}
              catalog={marketplaceQuery.data?.catalog}
              onChange={(marketplaceSkills) => setForm({ ...form, marketplaceSkills })}
            />
          )}
          <Field label="npm skill packages, one per line" hint="Each starts with npm:. The Agent Policy must allow each package.">
            <Textarea
              rows={2}
              value={form.npmSkills}
              onChange={(_, data) => setForm({ ...form, npmSkills: data.value })}
              placeholder="npm:@scope/skill-a"
            />
          </Field>
        </>
      ) : features && (
        <Field label="Skills" hint="One skill identifier per line.">
          <Textarea
            rows={4}
            value={form.skills}
            onChange={(_, data) => setForm({ ...form, skills: data.value })}
            placeholder={"@anthropic/skill-foo\n@scope/skill-bar"}
          />
        </Field>
      )}

      <Divider />
      <TeamModelRoutersSection
        routers={form.modelRouters}
        status={existing?.modelRouterStatus}
        sharedModels={sharedModelNames}
        decisionModels={decisionModelNames}
        onChange={(modelRouters) => setForm({ ...form, modelRouters })}
      />

      <Divider />
      <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
        <Text size={400} weight="semibold">Skill storage</Text>
        <label style={{ display: "flex", alignItems: "center", gap: 6 }}>
          <input
            type="checkbox"
            checked={form.storageEnabled}
            onChange={(e) => setForm({ ...form, storageEnabled: e.target.checked })}
          />
          <Text size={300}>Enable shared skill storage</Text>
        </label>
      </div>

      {form.storageEnabled && (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "1fr 1fr",
            gap: 12,
            padding: 12,
            border: `1px solid ${tokens.colorNeutralStroke2}`,
            borderRadius: 4,
          }}
        >
          <Field label="Resource group" required>
            <Input
              value={form.storage.resourceGroup}
              onChange={(_, data) =>
                setForm({ ...form, storage: { ...form.storage, resourceGroup: data.value } })
              }
            />
          </Field>
          <Field label="Storage account" required>
            <Input
              value={form.storage.storageAccount}
              onChange={(_, data) =>
                setForm({ ...form, storage: { ...form.storage, storageAccount: data.value } })
              }
            />
          </Field>
          <Field label="Container name" required>
            <Input
              value={form.storage.containerName}
              onChange={(_, data) =>
                setForm({ ...form, storage: { ...form.storage, containerName: data.value } })
              }
            />
          </Field>
          <Field label="Storage class">
            <Input
              value={form.storage.storageClassName || ""}
              onChange={(_, data) =>
                setForm({ ...form, storage: { ...form.storage, storageClassName: data.value } })
              }
            />
          </Field>
          <Field label="Storage size">
            <Input
              value={form.storage.storageSize || ""}
              onChange={(_, data) =>
                setForm({ ...form, storage: { ...form.storage, storageSize: data.value } })
              }
              placeholder="e.g. 10Gi"
            />
          </Field>
          <Field label="Mount path">
            <Input
              value={form.storage.mountPath || ""}
              onChange={(_, data) =>
                setForm({ ...form, storage: { ...form.storage, mountPath: data.value } })
              }
              placeholder="/home/vibe/.aks/skills (default); skills are also in ~/.agents/skills"
            />
          </Field>
        </div>
      )}

      <div style={{ display: "flex", flexDirection: "column", gap: 8, marginTop: 8 }}>
        {submitError && <AdminConfigurationErrorMessage error={submitError} />}
        <div style={{ display: "flex", gap: 8 }}>
          <Button appearance="primary" type="submit" disabled={pending || loadingConfiguration || !!configurationError || !validBindings}>
            {pending ? "Saving..." : isCreate ? "Create" : "Save"}
          </Button>
          <Button
            appearance="secondary"
            type="button"
            onClick={() => navigate("/admin/teams")}
          >
            Cancel
          </Button>
        </div>
      </div>
    </form>
  );
}
