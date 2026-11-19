// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useId } from "react";
import { Button, Field, Input, MessageBar, Select, Text, tokens } from "@fluentui/react-components";
import type { TeamModelRouter, TeamModelRouterClassifier, TeamModelRouterStatus } from "../../api/adminTeam";

export const MAX_MODEL_ROUTERS = 10;

const TIERS = [
  { key: "simple", label: "Simple tier", required: true },
  { key: "medium", label: "Medium tier", required: true },
  { key: "complex", label: "Complex tier", required: true },
  { key: "reasoning", label: "Reasoning tier", required: false },
] as const;

export function emptyModelRouter(): TeamModelRouter {
  return { name: "", classifier: "heuristic", classifierModel: "", simple: "", medium: "", complex: "", reasoning: "" };
}

/** The classifier of a router as the Team API takes it; anything else is the heuristic one. */
function classifierOf(router: TeamModelRouter): TeamModelRouterClassifier {
  return router.classifier === "llm" || router.classifier === "jev" ? router.classifier : "heuristic";
}

/** The routers as the Team API takes them: trimmed, without empty optional fields. */
export function toModelRouterRequests(routers: TeamModelRouter[]): TeamModelRouter[] {
  return routers.map((router) => {
    const classifier = classifierOf(router);
    const request: TeamModelRouter = {
      name: router.name.trim(),
      classifier,
      simple: router.simple.trim(),
      medium: router.medium.trim(),
      complex: router.complex.trim(),
    };
    // The llm and jev classifiers call a model; the heuristic one does not.
    if (classifier !== "heuristic") request.classifierModel = (router.classifierModel || "").trim();
    if ((router.reasoning || "").trim()) request.reasoning = (router.reasoning || "").trim();
    return request;
  });
}

/** Why the routers cannot be saved, or null. The Team API checks the same rules. */
export function modelRouterProblem(routers: TeamModelRouter[]): string | null {
  if (routers.length > MAX_MODEL_ROUTERS) return `A Team can define at most ${MAX_MODEL_ROUTERS} smart-routing models.`;
  const names = new Set<string>();
  for (const router of toModelRouterRequests(routers)) {
    if (!/^[^\s/]{1,64}$/.test(router.name)) return "Smart-routing model names need 1 to 64 characters without spaces or slashes.";
    if (router.name.toLowerCase().startsWith("cetirun-")) return `Smart-routing model ${router.name}: names starting with cetirun- are reserved.`;
    if (names.has(router.name)) return `Smart-routing model ${router.name} is defined twice.`;
    names.add(router.name);
    const classifierModel = router.classifierModel || "";
    const noClassifierModel = !classifierModel || /\s/.test(classifierModel) || classifierModel.startsWith("copilot/");
    if (router.classifier === "llm" && noClassifierModel) {
      return `Smart-routing model ${router.name}: the model classifier needs a shared model.`;
    }
    if (router.classifier === "jev" && noClassifierModel) {
      return `Smart-routing model ${router.name}: the decision classifier needs a decision model.`;
    }
    for (const tier of TIERS) {
      const model = (router[tier.key] || "") as string;
      if ((tier.required && !model) || /\s/.test(model)) return `Smart-routing model ${router.name}: the ${tier.key} tier needs a model name without spaces.`;
    }
  }
  return null;
}

/**
 * The Team's smart-routing models. Each is a model name the Team's Agents can
 * request; the AI Gateway sends each request to the model of the tier its
 * classifier picks. Members see only whether they may use a router.
 */
export function TeamModelRoutersSection({ routers, status, sharedModels, decisionModels, onChange }: {
  routers: TeamModelRouter[];
  status?: TeamModelRouterStatus[];
  sharedModels: string[];
  /** The Gateway's decision models, which only the decision classifier uses; undefined until the catalog is read. */
  decisionModels?: string[];
  onChange: (routers: TeamModelRouter[]) => void;
}) {
  const listId = useId();
  const update = (index: number, change: Partial<TeamModelRouter>) =>
    onChange(routers.map((router, i) => (i === index ? { ...router, ...change } : router)));
  // The models a classifier may name: shared models for llm, decision models for jev.
  const classifierModels = (classifier: TeamModelRouterClassifier) =>
    classifier === "jev" ? decisionModels || [] : classifier === "llm" ? sharedModels : [];
  // A model the new classifier cannot use does not carry over.
  const changeClassifier = (index: number, classifier: TeamModelRouterClassifier) => {
    const model = routers[index].classifierModel || "";
    update(index, { classifier, classifierModel: classifierModels(classifier).includes(model) ? model : "" });
  };
  return (
    <section aria-label="Smart routing" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <Text size={400} weight="semibold">Smart routing</Text>
      <Text size={300}>
        Each smart-routing model is a model name members can choose, such as auto-team. It cannot be the name of a shared
        model, such as the Gateway's own router auto. The AI Gateway classifies each request and sends it to the model of its
        tier. Tiers name shared models, or copilot/&lt;model&gt; for a GitHub Copilot model, which an Agent can use once its
        owner has signed in to GitHub Copilot. An Agent gets a router only when it may use every model the router uses; with
        an Agent Policy, also list the router's name in the Policy's allowed shared models.
      </Text>
      <datalist id={listId}>
        {sharedModels.map((model) => <option key={model} value={model} />)}
      </datalist>
      {routers.map((router, index) => {
        const report = status?.find((entry) => entry.name === router.name);
        const classifier = classifierOf(router);
        const offered = classifierModels(classifier);
        const decision = classifier === "jev";
        return (
          <div key={index} role="group" aria-label={`Smart-routing model ${index + 1}`}
            style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, padding: 12, border: `1px solid ${tokens.colorNeutralStroke2}`, borderRadius: 4 }}>
            <Field label="Model name" required hint="The name members choose, e.g. auto-team. It cannot be the name of a shared model.">
              <Input value={router.name} onChange={(_, data) => update(index, { name: data.value })} placeholder="auto-team" />
            </Field>
            <Field label="Classifier"
              hint="Heuristic scores requests locally. A shared model or a decision model, such as Microsoft-Decision-1, classifies each request, charged to the Agent.">
              <Select value={classifier} onChange={(event) => changeClassifier(index, event.target.value as TeamModelRouterClassifier)}>
                <option value="heuristic">Heuristic</option>
                <option value="llm">Shared model</option>
                <option value="jev">Decision model</option>
              </Select>
            </Field>
            {classifier !== "heuristic" && (
              <Field label="Classifier model" required
                hint={decision && decisionModels?.length === 0 ? "The AI Gateway offers no decision model." : undefined}>
                <Select value={router.classifierModel || ""} onChange={(event) => update(index, { classifierModel: event.target.value })}>
                  <option value="">{decision ? "Choose a decision model" : "Choose a shared model"}</option>
                  {router.classifierModel && !offered.includes(router.classifierModel) && (
                    <option value={router.classifierModel}>
                      {router.classifierModel} {decision ? "(not a decision model)" : "(not a shared model)"}
                    </option>
                  )}
                  {offered.map((model) => <option key={model} value={model}>{model}</option>)}
                </Select>
              </Field>
            )}
            {TIERS.map((tier) => (
              <Field key={tier.key} label={tier.label} required={tier.required}
                hint={tier.key === "reasoning" ? "Empty uses the complex tier's model." : undefined}>
                <Input list={listId} value={(router[tier.key] || "") as string}
                  onChange={(_, data) => update(index, { [tier.key]: data.value })}
                  placeholder={tier.key === "simple" ? "shared model or copilot/<model>" : undefined} />
              </Field>
            ))}
            <div style={{ gridColumn: "1 / span 2", display: "flex", alignItems: "center", gap: 12 }}>
              {report && (report.ready
                ? <Text size={200}>Registered in the AI Gateway.</Text>
                : <MessageBar intent="warning">{report.message || "Not registered in the AI Gateway yet."}</MessageBar>)}
              <Button type="button" onClick={() => onChange(routers.filter((_, i) => i !== index))}>Remove</Button>
            </div>
          </div>
        );
      })}
      <div>
        <Button type="button" disabled={routers.length >= MAX_MODEL_ROUTERS} onClick={() => onChange([...routers, emptyModelRouter()])}>
          Add smart-routing model
        </Button>
      </div>
    </section>
  );
}
