// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// ConditionTeamModelRoutersReady reports whether every smart-routing model a
// Team defines is registered in the AI Gateway. It is False with a message
// naming each router that is not, and why.
const ConditionTeamModelRoutersReady = "ModelRoutersReady"

// Classifier types a Team model router offers: heuristic, llm and jev.
// Without a LiteLLM licence, LiteLLM limits the heuristic_v2, capability and
// llm_v2 classifiers, custom tier sets and custom classifier prompts, a jev
// classifier's own instructions included, to one router per Gateway, so a
// Team cannot use them. A jev classifier with LiteLLM's own instructions is
// not limited.
const (
	ModelRouterClassifierHeuristic = "heuristic"
	ModelRouterClassifierLLM       = "llm"
	ModelRouterClassifierJev       = "jev"
)

// TeamModelRouter is one smart-routing model of a Team: a model name Agents
// request, behind which each request goes to the model of the complexity tier
// its classifier picks. Administrators define routers; an Agent receives a
// router only when everything the router uses is available to it.
type TeamModelRouter struct {
	// Name is the model name Agents request, such as auto-team or auto-claude.
	// It must differ from the name of every shared model of the AI Gateway,
	// such as the Gateway's own router auto that make deploy configures.
	// It has no whitespace or slash and must not start with the reserved
	// prefix cetirun- (in any letter case).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[^\s/]+$`
	// +kubebuilder:validation:XValidation:rule="!self.lowerAscii().startsWith('cetirun-')",message="a model router name must not start with the reserved prefix cetirun-"
	Name string `json:"name"`

	// Classifier decides the complexity tier of each request.
	// +kubebuilder:default={type: heuristic}
	// +optional
	Classifier TeamModelRouterClassifier `json:"classifier,omitempty"`

	// Tiers names the model each complexity tier sends requests to.
	Tiers TeamModelRouterTiers `json:"tiers"`
}

// TeamModelRouterClassifier selects how a router classifies requests.
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type in ['llm', 'jev']) == has(self.model)",message="classifier.model is required for the llm and jev classifiers and not allowed for the heuristic classifier"
type TeamModelRouterClassifier struct {
	// Type is heuristic, which scores each request locally without a model
	// call, llm, which asks a shared model to classify each request, or jev,
	// which asks a shared decision model, such as Microsoft-Decision-1, to
	// pick the tier.
	// +kubebuilder:validation:Enum=heuristic;llm;jev
	// +kubebuilder:default=heuristic
	// +optional
	Type string `json:"type,omitempty"`

	// Model is the shared model that classifies requests: a chat model for
	// the llm classifier, a decision model (a shared model whose
	// model_info.mode is evaluation) for the jev classifier. It must be a
	// shared model of the AI Gateway, not a GitHub Copilot model or another
	// router, and need not be listed by the AgentPolicy. Its calls are
	// charged to the Agent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\s]+$`
	// +optional
	Model string `json:"model,omitempty"`
}

// TeamModelRouterTiers names the model of each complexity tier: a shared
// model name, or copilot/<model> for the GitHub Copilot model of that ID,
// which each Agent reaches through its owner's own Copilot sign-in.
type TeamModelRouterTiers struct {
	// Simple serves the simplest requests.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\s]+$`
	Simple string `json:"simple"`

	// Medium serves ordinary requests. It is also the model the router uses
	// when a request cannot be classified.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\s]+$`
	Medium string `json:"medium"`

	// Complex serves complex requests.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\s]+$`
	Complex string `json:"complex"`

	// Reasoning serves requests that ask for step-by-step reasoning. Empty
	// uses the complex tier's model.
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\s]+$`
	// +optional
	Reasoning string `json:"reasoning,omitempty"`
}

// ReasoningModel is the model of the reasoning tier, which defaults to the
// complex tier's model.
func (t TeamModelRouterTiers) ReasoningModel() string {
	if t.Reasoning != "" {
		return t.Reasoning
	}
	return t.Complex
}

// Models lists the models of the four tiers in tier order, duplicates
// included.
func (t TeamModelRouterTiers) Models() []string {
	return []string{t.Simple, t.Medium, t.Complex, t.ReasoningModel()}
}

// TeamModelRouterStatus is the AI Gateway registration of one router.
type TeamModelRouterStatus struct {
	// Name is the router's name in spec.modelRouters.
	Name string `json:"name"`

	// GatewayModel is the internal Gateway model name of the router, which
	// Agents granted the router send their requests to.
	// +optional
	GatewayModel string `json:"gatewayModel,omitempty"`

	// Ready reports whether the router is registered in the Gateway with its
	// current configuration.
	Ready bool `json:"ready"`

	// Message explains why the router is not ready.
	// +optional
	Message string `json:"message,omitempty"`

	// SharedModels are the shared models the router's tiers send requests
	// to as registered, a GitHub Copilot tier that joins a shared model
	// included: an Agent with an AgentPolicy is granted the router only when
	// the policy allows each. The classifier is not listed.
	// +optional
	SharedModels []string `json:"sharedModels,omitempty"`

	// CopilotModels are the copilot/<ID> tiers registered under the Copilot
	// model's ID: an Agent needs GitHub Copilot and that model.
	// +optional
	CopilotModels []string `json:"copilotModels,omitempty"`
}

// AIGatewayModelRouterStatus reports one smart-routing model for the Agent, a
// router of its Team or one configured in the Gateway: whether the Agent's key
// was granted it, and if not why. A router is granted only when the Agent may
// use every model it sends requests to.
type AIGatewayModelRouterStatus struct {
	// Name is the router's name, the model name the owner chooses.
	Name string `json:"name"`

	// Model is the Gateway model name the Agent sends requests for the
	// router to: a Team router's internal name, or the name of a router
	// configured in the Gateway. Set only when the router is granted.
	// +optional
	Model string `json:"model,omitempty"`

	// Granted reports whether the Agent's key may use the router.
	Granted bool `json:"granted"`

	// Reason explains why the router is not granted.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Alias is the router's name when the Agent's key also reaches a Team's
	// router under it, through a key alias the operator keeps for each
	// granted Team router. A Hermes Agent is configured with the router under
	// this name. Empty when another model the key reaches, such as one of the
	// Agent's GitHub Copilot models or an alias that an administrator set,
	// has the name; the Agent then uses the internal name.
	// +optional
	Alias string `json:"alias,omitempty"`

	// ContextWindow and MaxTokens are the limits the Agent is configured with
	// for the router. They are the smallest known limits of the models that
	// the router sends requests to: its tier models and, for a router
	// configured in the Gateway, its default model. The Agent's own GitHub
	// Copilot routes under the names of these models count too. For a router
	// configured in the Gateway, limits that the router itself sets replace
	// those of its models.
	// +optional
	ContextWindow int64 `json:"contextWindow,omitempty"`
	// +optional
	MaxTokens int64 `json:"maxTokens,omitempty"`

	// Vision reports that the Agent is configured to send images to the
	// router: every model that the router sends requests to answer accepts
	// them. The Agent's own GitHub Copilot routes under the names of these
	// models count too.
	// +optional
	Vision bool `json:"vision,omitempty"`
}
