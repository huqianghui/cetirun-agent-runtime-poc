# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Checks the CetiRun AI Gateway applies to every request.

The image's entrypoint registers ``handler`` with LiteLLM before the proxy
loads its configuration, always, unlike the request log, which the chart can
turn off. Settings saved in LiteLLM's database cannot remove it either.

Request fields a caller could use to change the Gateway for its own call are
dropped:

- litellm_request_debug makes LiteLLM print that request's prompt and the
  provider's raw response to the log.
- num_retries, in the body or the x-litellm-num-retries header, which LiteLLM
  copies into num_retries before this callback sees the request, and
  model_group_retry_policy turn the Gateway's retries back on. The Gateway does
  not retry: agents retry on their own, and the two multiply. Each made one
  failing request four upstream calls. The same fields are removed from
  router_settings_override, a set of router settings LiteLLM adds to the
  request after this callback runs, and takes from the request body when
  neither the key nor its team has router settings of its own.

A request for one of LiteLLM's complexity routers (auto routers) that fails at
the model of the tier the router chose goes on to the router's other models
rather than failing: a tier whose model is unavailable, such as an Agent's
GitHub Copilot model while its proxy is down, failed with 503 although the
router's other tiers could answer. The guard gives such a request fallbacks of
its own, for this request only: for each model of the router, the other models
of its tier, the models of the tiers above it in order, then the router's
default model, then the models of the tiers below it. LiteLLM tries them in
that order, each only if the calling key may use it and only before a response
has started. Fallbacks the administrator configured for a model, or for the
router's name, apply instead; a client that sends fallbacks of its own, or
none, keeps them. A refusal by the guard or another check, which is not the
model's failure, goes to no other model.

LiteLLM appends its internal names to an error it returns to a caller: the
model groups a request went to, their fallbacks, deployments' timeouts. For a
Team's router, the model group is its internal name. The guard turns that off
(litellm.expose_router_debug_in_errors) when it is loaded; litellm_settings in
the Gateway's configuration can turn it on again.

A key the CetiRun operator gives an alias for a Team's router, the router's
name for its internal name, lists the router under its name only: LiteLLM
lists a key alias next to its target in GET /v1/models.

A per-Agent GitHub Copilot route, a deployment in an access group named
cetirun-copilot-<Agent UID>, is offered only to a key that holds that group.
LiteLLM lets a key granted every model, such as the master key, reach every
deployment; a key reaches these only if it holds the route's access group.

A key the CetiRun operator issued reaches only the deployments in its access
groups, whichever way a request gets to them. LiteLLM checks a key against the
model the request names; when that is an auto router, the model the router
picks is not checked against the key, as LiteLLM's own access-group filter
stops when the key holds none of that model's groups. The operator grants a
router only to Agents that may use every model it sends requests to; this holds
each request to that, also before the operator has withdrawn a grant after a
configuration change. A model the key may not use at all is refused with 403,
which LiteLLM passes on unchanged, without retrying, falling back or cooling
the model down. When the key may use the model but none of its deployments is
available, LiteLLM's own error applies. The call a router makes to classify a
request is exempt: the router makes it on the key's behalf, to a model of its
own configuration that the key need not hold.

A decision model, one whose deployments LiteLLM lists with the evaluation mode,
such as Microsoft-Decision-1, answers only LiteLLM's Decisions API
(/v1/systemone and /v1/decisions). Asked for through any other API, such as
chat completions, the provider refused the request with 404 and LiteLLM cooled
the deployment down, so every router classifying with it fell back to its
default classifier, for every caller, for a few seconds. Such a request is
refused with 400 before any deployment is called, for every caller; a key the
CetiRun operator issued gets the 403 above for any request of its own to a
decision model.

A router's request to a decision model to classify a request is held to the
model's context. LiteLLM sends a jev classifier the caller's whole system
prompt and its latest user messages, however long they are;
Microsoft-Decision-1 refused a request over its 32,768 tokens with 422, and
the router then classified the request with its fallback instead, without an
error. Such a request is shortened to as many bytes as the model takes tokens,
less its questions and a reserve: the newest message stays whole where it
fits, the rest keeps its beginning and its end, and each change is logged. A
client's own call to the Decisions API is left as it is.

Error messages returned to callers no longer include LiteLLM's internal list of
deployments in cooldown: when no deployment of a model is available, LiteLLM's
message lists the IDs of every deployment in cooldown, of any model. The proxy
builds the response from the error after the failure hook runs, so the hook
removes the list from the error's message in place. The status (429), the
"Try again in N seconds" wording and the retry-after header, which LiteLLM
derives from the error, stay as they are.

Encrypted reasoning without the Gateway's mark is removed from Responses API
requests while encrypted content affinity is on for the model. A client that
keeps no state on the provider, such as OpenClaw, sends every earlier
reasoning item back, encrypted by the deployment that produced it, which alone
can read it. With affinity on, LiteLLM marks each item it returns with that
deployment and routes or strips by the mark; an item without one, from before
affinity was turned on or from elsewhere, is forwarded as it is, and a
deployment of another provider account refuses the whole request (400
invalid_encrypted_content). The item keeps its readable summary, as LiteLLM
keeps it when it strips a marked item. Encrypted content a client shortened,
which no deployment can read, is removed the same way.

The mark is written in a form clients keep. LiteLLM writes it as
litellm_enc:<base64>;<content>; OpenClaw keeps encrypted reasoning only when
it looks like an opaque token (letters, digits, + / _ - and trailing =), and
otherwise stores it through its secret redaction, which shortens the
provider's gAAAA... content to 11 characters. Every following turn then sent
that back, the deployment refused it, and OpenClaw retried without its
reasoning. The guard replaces LiteLLM's two functions that write and read the
mark: it writes ctrnenc<hex of the deployment ID>x<content>, and reads both
forms, so items marked before the change still route.

A request whose encrypted reasoning a deployment could not read (400
invalid_encrypted_content) does not go on to the model's fallbacks: their
deployments cannot read it either, so each fallback fails the same way, and
the client, which handles this error by sending the request again without the
reasoning, gets the first error instead of the last.

A GitHub Copilot model returns its reasoning items with a status, and a client
that keeps no state on the provider sends them back as they are. Azure OpenAI
refuses that field on a reasoning item (400 unknown_parameter
"input[n].status"), and an item ID longer than 64 characters, such as
Copilot's IDs for a Grok model's messages. A conversation that moved from a
Copilot model to an Azure deployment, by an auto router's tier or a failover,
failed there, although encrypted content affinity removed the reasoning's
encrypted content. The guard removes both from a request to an Azure OpenAI
deployment, and logs each change.

LiteLLM prices each request that an auto router answers against the most
expensive model of the router's hardest tier (proxy/spend_tracking/savings.py),
after the response. It names that model's provider with get_llm_provider. For
the github_copilot provider, the provider of a GitHub Copilot model whose
base_model is github_copilot/<id>, get_llm_provider asks GitHub for a token: in
the Gateway, which has none, it started GitHub's interactive device sign-in and
blocked the worker for about three minutes, until the liveness probe restarted
the container. LiteLLM's other callers take such a provider from the name
(declared_authenticating_provider). The guard replaces the savings module's
lookup with one that does the same, and asks get_llm_provider for every other
name.

A request to a model of the GPT-5 or GPT-6 series that reasons, asked for by
name or chosen by an auto router, loses its sampling parameters: temperature
other than 1, top_p, logprobs and top_logprobs. LiteLLM, the Gateway's for an
Azure deployment and the Copilot proxy's for a Copilot route, refuses such a
request with 400 unless the reasoning effort resolves to none. Hermes sends its
side tasks, such as a conversation title, with temperature 0 to the model the
user chose or to auto; with a reasoning model each failed, out of the user's
sight. LiteLLM's own rule decides, read from LiteLLM, and each change is logged.
"""

import importlib
import json
import re
import sys

import litellm
from fastapi import HTTPException
from litellm._logging import verbose_proxy_logger
from litellm.integrations.custom_logger import CustomLogger
from litellm.proxy._types import UserAPIKeyAuth

# A request body field that makes LiteLLM log that request's content.
DEBUG_PARAMETER = "litellm_request_debug"
# Request fields that change the Gateway's retries for that request.
RETRY_PARAMETERS = ("num_retries", "model_group_retry_policy")
REQUEST_PARAMETERS = (DEBUG_PARAMETER, *RETRY_PARAMETERS)
# Router settings LiteLLM merges into the request after the pre-call hooks:
# those of the key or its team, or the request body's own when neither has
# any (route_llm_request.py).
ROUTER_SETTINGS_OVERRIDE = "router_settings_override"
# The request fields that set or turn off a request's fallbacks, the key of a
# fallback list entry that applies to every model, and LiteLLM's model value
# for its complexity routers, under the prefix of all its auto routers.
FALLBACKS = "fallbacks"
DISABLE_FALLBACKS = "disable_fallbacks"
GENERIC_FALLBACK = "*"
AUTO_ROUTER_PREFIX = "auto_router/"
COMPLEXITY_ROUTER = "auto_router/complexity_router"
# LiteLLM's tiers, lowest first, and the tiers it takes a complexity router's
# default model from when the router names none (router.py), after the
# configuration's fallback_tier.
TIER_ORDER = ("NON_REASONING", "SIMPLE", "MEDIUM", "COMPLEX", "REASONING")
DEFAULT_MODEL_TIERS = ("MEDIUM", "SIMPLE")

# The access groups the CetiRun operator gives each Agent's GitHub Copilot
# routes: this prefix and the Agent's UID.
COPILOT_GROUP_PREFIX = "cetirun-copilot-"
# A key the CetiRun operator issued holds access groups under this prefix only
# (shared models, model routers, the Agent's Copilot routes), or, with none,
# LiteLLM's no-access placeholder. The Gateway chart refuses model names under
# the prefix, so a key granted models by name never looks like one.
OPERATOR_GROUP_PREFIX = "cetirun-"
NO_MODELS = "no-default-models"
# Where the proxy puts the calling key on a request: chat completions use
# metadata, the Responses and Anthropic Messages APIs litellm_metadata.
METADATA_BUCKETS = ("litellm_metadata", "metadata")
CALLER_KEY = "user_api_key_auth"
# How LiteLLM marks the call an auto router makes to classify a request. The
# proxy removes the field from the metadata a client sends.
CALL_ORIGIN = "internal_call_origin"
CLASSIFIER_ORIGIN = "autorouter_classifier"
# Where the proxy keeps the request's logging object, which records the API the
# request came through as its call type, and the call types of LiteLLM's
# Decisions API, the only API a decision model answers.
LOGGING_OBJECT = "litellm_logging_obj"
DECISIONS_CALL_TYPES = frozenset(("adecisions", "decisions", "asystemone", "systemone"))
# The proxy's module, which holds the live router. Read from sys.modules only:
# the callback never imports the proxy itself.
PROXY_SERVER_MODULE = "litellm.proxy.proxy_server"

# LiteLLM's message when no deployment of a model can take a request, and the
# list it appends: cooldown_list=['<deployment id>', ...].
NO_DEPLOYMENTS = "No deployments available"
COOLDOWN_LIST = re.compile(r",?\s*cooldown_list=\[[^\]]*\]")

# The marks LiteLLM's encrypted content affinity puts on the items it returns:
# the producing deployment in the item's ID, and in its encrypted_content.
MARKED_ITEM_ID = "encitem_"
MARKED_CONTENT = "litellm_enc:"
# The Gateway's form of the mark in encrypted_content: this prefix, the
# deployment ID in lowercase hex, an x, then the provider's content. Only
# characters an opaque token may hold.
GATEWAY_MARK = "ctrnenc"
GATEWAY_MARK_FORM = re.compile(r"ctrnenc((?:[0-9a-f]{2})+)x(.+)", re.DOTALL)
# A client's shortened copy of a long value: kept start, ellipsis, kept end.
SHORTENED = "\u2026"
# LiteLLM's callback for encrypted content affinity, found by name so the
# check needs no import of LiteLLM's internals.
AFFINITY_CHECK = "EncryptedContentAffinityCheck"
# How a provider refuses encrypted reasoning it cannot read.
UNREADABLE_REASONING = ("invalid_encrypted_content", "could not be verified", "could not be decrypted")
# A field GitHub Copilot puts on the reasoning items it returns, which Azure
# OpenAI refuses on a reasoning item a request sends back (400,
# unknown_parameter "input[n].status"), and the longest item ID Azure OpenAI
# takes: Copilot's IDs for a Grok model's messages had 396 characters (400,
# "Invalid 'input[n].id': string too long").
REASONING_STATUS = "status"
AZURE_MAX_ITEM_ID = 64

# LiteLLM's module that prices an auto router's request against its savings
# baseline, and the module of its helper that names a provider that signs in
# to answer get_llm_provider (github_copilot, chatgpt) from the model name.
SAVINGS_MODULE = "litellm.proxy.spend_tracking.savings"
PROVIDER_LOGIC_MODULE = "litellm.litellm_core_utils.get_llm_provider_logic"

# Sampling parameters a model of the GPT-5 and GPT-6 series accepts only while
# its reasoning effort is none; LiteLLM refuses the request (400) otherwise,
# temperature only when it is not 1.
SAMPLING_PARAMETERS = ("temperature", "top_p", "logprobs", "top_logprobs")
# Where an auto router records its choice on a request, with its own name.
ROUTING_DECISION = "routing_decision"
ROUTER_NAME = "router_model_name"
# The modes of a model Agents can hold a conversation with; a deployment without one counts.
CHAT_MODES = (None, "", "chat", "completion", "responses")

# The line LiteLLM's complexity router writes before the newest user message in
# the request it sends a classifier, last; the caller's system prompt, earlier
# user messages and the conversation's length come before it
# (complexity_router.py, _build_classifier_user_payload).
CLASSIFY_MARKER = "\nClassify this message:\n"
# Microsoft-Decision-1's context in tokens, for a decision model whose
# deployments state none in model_info.max_input_tokens. It refuses a longer
# request with 422: "invalid model request: Question prompt exceeds the
# 32768-token context limit including the decision token".
DECISION_CONTEXT_TOKENS = 32768
# Bytes left for the service's own template around the request and for the
# decision token, and the fewest bytes a request is shortened to.
DECISION_RESERVE_BYTES = 512
DECISION_MINIMUM_BYTES = 1024
# LiteLLM quotes a long message by its beginning and its end, 30% and 70% of
# the room (complexity_router.py, _truncate); the guard splits the same way.
HEAD_SHARE = 0.3
ELISION = "\n[...]\n"
# What a shortened request keeps, for the warning.
KEPT_NEWEST = "the newest message whole and the beginning and the end of the rest"
KEPT_BOTH_ENDS = "the beginning and the end of the newest message and of the rest"
KEPT_REQUEST_ENDS = "the beginning and the end of the request"


def mark_encrypted_content(encrypted_content, model_id):
    """encrypted_content with the producing deployment's ID, in the Gateway's form."""
    return f"{GATEWAY_MARK}{str(model_id).encode('utf-8').hex()}x{encrypted_content}"


def read_marked_content(content, legacy=None):
    """(deployment ID or None, provider content) of encrypted_content in either form of the mark.

    legacy reads LiteLLM's own litellm_enc: form; without it that form is
    returned unread.
    """
    if isinstance(content, str) and content.startswith(GATEWAY_MARK):
        match = GATEWAY_MARK_FORM.fullmatch(content)
        if match:
            try:
                return bytes.fromhex(match.group(1)).decode("utf-8"), match.group(2)
            except ValueError:
                pass
        return None, content
    if legacy is not None:
        return legacy(content)
    return None, content


def install_mark_form():
    """Make LiteLLM write the Gateway's form of the mark and read both forms."""
    try:
        from litellm.responses.utils import ResponsesAPIRequestUtils
    except Exception:  # noqa: BLE001
        return False
    current = ResponsesAPIRequestUtils.__dict__.get("_unwrap_encrypted_content_with_model_id")
    legacy = getattr(current, "__func__", current)
    if getattr(legacy, "cetirun_mark_form", False):
        return True

    def unwrap(wrapped_content):
        return read_marked_content(wrapped_content, legacy)

    unwrap.cetirun_mark_form = True
    ResponsesAPIRequestUtils._wrap_encrypted_content_with_model_id = staticmethod(mark_encrypted_content)
    ResponsesAPIRequestUtils._unwrap_encrypted_content_with_model_id = staticmethod(unwrap)
    return True


FAILOVER_EXCLUDED = "_excluded_deployment_ids"


def deployment_id(deployment):
    """The model_info.id of a deployment dict, or None."""
    info = deployment.get("model_info") if isinstance(deployment, dict) else None
    return info.get("id") if isinstance(info, dict) else None


def install_failover_past_affinity():
    """Let a request's failover leave the deployment that key affinity kept it on.

    LiteLLM 1.105.6, like 1.105.1, runs its deployment-affinity filter before
    it removes the deployments a weighted failover already tried. A key kept on
    a deployment that fails is narrowed to that deployment, which the exclusion
    then removes: no deployment is left, so the failover is not attempted and
    the key's requests fail until the deployment is in cooldown. Here the
    filter is given the deployments the failover may still use, so a kept
    deployment that already failed this request is not chosen again and
    affinity applies among the others.
    """
    try:
        from litellm.router_utils.pre_call_checks.deployment_affinity_check import DeploymentAffinityCheck
    except Exception:  # noqa: BLE001
        return False
    current = DeploymentAffinityCheck.__dict__.get("async_filter_deployments")
    if current is None or getattr(current, "cetirun_failover", False):
        return current is not None

    async def async_filter_deployments(self, model, healthy_deployments, messages, request_kwargs=None,
                                       parent_otel_span=None):
        excluded = (request_kwargs or {}).get(FAILOVER_EXCLUDED)
        if excluded and isinstance(healthy_deployments, list):
            excluded = set(excluded)
            healthy_deployments = [d for d in healthy_deployments if deployment_id(d) not in excluded]
        return await current(self, model, healthy_deployments, messages, request_kwargs=request_kwargs,
                             parent_otel_span=parent_otel_span)

    async_filter_deployments.cetirun_failover = True
    DeploymentAffinityCheck.async_filter_deployments = async_filter_deployments
    return True


# The internal names the CetiRun operator registers a Team's routers under.
TEAM_ROUTER_PREFIX = "cetirun-router-"
LISTING_MODULE = "litellm.proxy.common_utils.model_listing_utils"


def without_aliased_routers(entries):
    """The (listed name, model) rows of a model listing without a Team router's internal name where the
    caller's alias lists the router."""
    aliased = {model for listed, model in entries
               if listed != model and isinstance(model, str) and model.startswith(TEAM_ROUTER_PREFIX)}
    return tuple(entry for entry in entries if not (entry[0] == entry[1] and entry[0] in aliased))


def install_router_alias_listing():
    """List a Team's router that a key reaches under an alias only under the alias.

    The CetiRun operator gives an Agent's key an alias for each Team router the Agent may use: the router's
    name for its internal name, cetirun-router-<hash>. LiteLLM 1.105.6 lists a key alias next to its
    target in GET /v1/models, so the router was listed twice, once under a name that means nothing to the
    Agent's owner. LiteLLM's alias_listing_entries, which adds the aliases' rows, is replaced by one that
    then leaves the internal name out. The proxy module imports the function by name, so it is replaced
    there too when the proxy is already loaded. A lookup of the internal name still answers.
    """
    try:
        listing = importlib.import_module(LISTING_MODULE)
    except Exception:  # noqa: BLE001
        return False
    current = getattr(listing, "alias_listing_entries", None)
    if current is None:
        return False
    if not getattr(current, "cetirun_router_aliases", False):
        original = current

        def alias_listing_entries(entries, aliases):
            return without_aliased_routers(original(entries, aliases))

        alias_listing_entries.cetirun_router_aliases = True
        listing.alias_listing_entries = alias_listing_entries
        current = alias_listing_entries
    proxy = sys.modules.get(PROXY_SERVER_MODULE)
    if proxy is not None and getattr(proxy, "alias_listing_entries", current) is not current:
        proxy.alias_listing_entries = current
    return True


def install_savings_provider_lookup():
    """Name the provider of an auto router's savings baseline without signing in to it.

    After each request an auto router answers, LiteLLM 1.105.6 prices the request
    against the most expensive model of the router's hardest tier. Its
    _resolve_model names that model's provider with get_llm_provider, which, for
    github_copilot, asks GitHub for a token: in the Gateway, which has none, that
    started the interactive device sign-in and blocked the worker for minutes.
    The replacement answers a provider that signs in from the model name, as
    LiteLLM's other callers do (declared_authenticating_provider); every other
    name still goes to LiteLLM's own lookup. The savings module calls the
    function through its module global, so replacing that global is enough.
    """
    try:
        savings = importlib.import_module(SAVINGS_MODULE)
        declared_provider = importlib.import_module(PROVIDER_LOGIC_MODULE).declared_authenticating_provider
    except Exception:  # noqa: BLE001
        return False
    current = getattr(savings, "_resolve_model", None)
    identity = getattr(savings, "_ModelIdentity", None)
    if current is None or identity is None:
        return False
    if getattr(current, "cetirun_declared_provider", False):
        return True
    original = current

    def _resolve_model(model, custom_llm_provider):
        provider = declared_provider(model, custom_llm_provider) if isinstance(model, str) and model else None
        if provider is None:
            return original(model, custom_llm_provider)
        return identity(model=model.removeprefix(f"{provider}/"), provider=provider)

    _resolve_model.cetirun_declared_provider = True
    savings._resolve_model = _resolve_model
    return True


def azure_openai(deployment):
    """Whether a deployment is on Azure OpenAI, by LiteLLM's provider prefix of its model."""
    params = deployment.get("litellm_params") if isinstance(deployment, dict) else None
    model = params.get("model") if isinstance(params, dict) else None
    return isinstance(model, str) and model.startswith("azure/")


def strip_azure_refused_fields(request_input):
    """Remove from a Responses API request's input, in place, what Azure OpenAI refuses in items that another
    provider returned; (reasoning statuses removed, item IDs removed).

    The status of a reasoning item goes: Azure OpenAI takes the status of a
    message. The ID of any other item goes when it is longer than Azure OpenAI
    takes; an ID LiteLLM marked (encitem_) stays, as LiteLLM reads and restores
    it itself. A reasoning item's ID is left to encrypted content affinity,
    which removes it with the encrypted content that another deployment
    produced. The list is changed in place, as LiteLLM forwards it and its
    fallback hops share it.
    """
    if not isinstance(request_input, list):
        return 0, 0
    statuses = ids = 0
    for index, item in enumerate(request_input):
        if not isinstance(item, dict):
            continue
        drop = set()
        if item.get("type") == "reasoning":
            if REASONING_STATUS in item:
                drop.add(REASONING_STATUS)
                statuses += 1
        else:
            item_id = item.get("id")
            if isinstance(item_id, str) and len(item_id) > AZURE_MAX_ITEM_ID and not item_id.startswith(MARKED_ITEM_ID):
                drop.add("id")
                ids += 1
        if drop:
            request_input[index] = {key: value for key, value in item.items() if key not in drop}
    return statuses, ids

def marked_content(content):
    """Whether encrypted_content carries the producing deployment in either form of the mark."""
    if content.startswith(MARKED_CONTENT):
        return True
    return read_marked_content(content)[0] is not None


def unmarked_encrypted_reasoning(item):
    """Whether an input item is encrypted reasoning that carries no mark of the deployment that produced it."""
    if not isinstance(item, dict) or item.get("type") != "reasoning":
        return False
    content = item.get("encrypted_content")
    if not isinstance(content, str) or not content or marked_content(content):
        return False
    item_id = item.get("id")
    return not (isinstance(item_id, str) and item_id.startswith(MARKED_ITEM_ID))


def shortened_encrypted_reasoning(item):
    """Whether an input item is encrypted reasoning a client shortened, which no deployment can read."""
    if not isinstance(item, dict) or item.get("type") != "reasoning":
        return False
    content = item.get("encrypted_content")
    return isinstance(content, str) and SHORTENED in content


def readable(value):
    """Whether a reasoning item's summary or content carries text a model can read."""
    if isinstance(value, str):
        return bool(value.strip())
    if isinstance(value, list):
        return any(isinstance(block, dict) and bool(block.get("text")) for block in value)
    return False


def affinity_enabled(model):
    """Whether LiteLLM's encrypted content affinity applies to requests for the model."""
    for callback in getattr(litellm, "callbacks", None) or ():
        if type(callback).__name__ != AFFINITY_CHECK:
            continue
        enabled = getattr(callback, "_is_enabled_for_model_group", None)
        try:
            return bool(enabled(model)) if callable(enabled) else True
        except Exception:  # noqa: BLE001
            return True
    return False


def strip_unmarked_reasoning(request_input):
    """Remove unmarked or shortened encrypted reasoning, in place; the number of items changed.

    Like LiteLLM's own stripping, an item keeps its readable summary without
    encrypted_content and id, and an item with nothing readable is dropped.
    The list is changed in place: LiteLLM forwards this list, and its fallback
    hops share it.
    """
    if not isinstance(request_input, list):
        return 0
    kept, changed = [], 0
    for item in request_input:
        if not (unmarked_encrypted_reasoning(item) or shortened_encrypted_reasoning(item)):
            kept.append(item)
            continue
        changed += 1
        if readable(item.get("summary")) or readable(item.get("content")):
            kept.append({key: value for key, value in item.items() if key not in ("encrypted_content", "id")})
    if changed:
        request_input[:] = kept
    return changed


class UnreadableReasoningFallback(Exception):
    """Ends a fallback that follows a deployment's refusal of encrypted reasoning."""


def unreadable_reasoning_refusal(request_kwargs):
    """The refusal of a request's encrypted reasoning when the request is a fallback after it, else None."""
    if not isinstance(request_kwargs, dict):
        return None
    for name in METADATA_BUCKETS:
        bucket = request_kwargs.get(name)
        if not isinstance(bucket, dict) or not bucket.get("attempted_fallbacks"):
            continue
        for attempt in bucket.get("previous_models") or ():
            text = attempt.get("exception_string") if isinstance(attempt, dict) else None
            if isinstance(text, str) and any(marker in text for marker in UNREADABLE_REASONING):
                return text
    return None


def refusal_error(model, text):
    """The error that ends the fallback: the deployment's refusal again, as a 400 the client recognizes."""
    error_class = getattr(litellm, "BadRequestError", None)
    if error_class is not None:
        try:
            return error_class(message=text, model=model, llm_provider="")
        except Exception:  # noqa: BLE001
            pass
    return UnreadableReasoningFallback(text)


def routing_router(model, request_kwargs):
    """The auto router that chose model for the request, or None when no router did."""
    if not isinstance(request_kwargs, dict):
        return None
    for name in METADATA_BUCKETS:
        bucket = request_kwargs.get(name)
        decision = bucket.get(ROUTING_DECISION) if isinstance(bucket, dict) else None
        router = decision.get(ROUTER_NAME) if isinstance(decision, dict) else None
        if isinstance(router, str) and router and router != model:
            return router
    return None


def deployment_model_names(deployment):
    """The model names a deployment is known by, without provider prefixes: its base_model and its model."""
    names = []
    for holder, key in (("model_info", "base_model"), ("litellm_params", "model")):
        section = deployment.get(holder) if isinstance(deployment, dict) else None
        value = section.get(key) if isinstance(section, dict) else None
        if isinstance(value, str) and value:
            names.append(value.split("/")[-1])
    return names


def requested_effort(request_kwargs):
    """The reasoning effort a request asks for, in the Chat Completions or the Responses API form, or None."""
    effort = request_kwargs.get("reasoning_effort")
    if effort is None and isinstance(request_kwargs.get("reasoning"), dict):
        effort = request_kwargs["reasoning"].get("effort")
    if isinstance(effort, dict):
        effort = effort.get("effort")
    return effort if isinstance(effort, str) else None


def refuses_sampling(name, effort):
    """Whether LiteLLM refuses sampling parameters for the model name at the reasoning effort.

    LiteLLM's own rule for the GPT-5 and GPT-6 series: only temperature 1 and
    no top_p or logprobs, unless the effort, asked for or the model's default,
    resolves to none.
    """
    try:
        from litellm.llms.openai.chat.gpt_5_transformation import OpenAIGPT5Config
    except Exception:  # noqa: BLE001
        return False
    try:
        if not OpenAIGPT5Config.is_model_gpt_5_model(name):
            return False
        return not (OpenAIGPT5Config._supports_reasoning_effort_level(name, "none")
                    and OpenAIGPT5Config.effort_resolves_to_none(name, effort))
    except Exception:  # noqa: BLE001
        return True


def strip_sampling(request_kwargs, deployments):
    """Remove the sampling parameters the requested or routed model refuses, in place; the names removed.

    Removed when any of the deployments refuses them: which one serves the
    request is chosen later, and the others accept the request without them.
    """
    present = [name for name in SAMPLING_PARAMETERS
               if request_kwargs.get(name) is not None and not (name == "temperature" and request_kwargs[name] == 1)]
    if not present:
        return []
    effort = requested_effort(request_kwargs)
    if not any(refuses_sampling(name, effort) for deployment in deployments for name in deployment_model_names(deployment)):
        return []
    for name in present:
        request_kwargs.pop(name, None)
    return present


def refuses_no_reasoning(deployment):
    """Whether LiteLLM refuses a reasoning effort of none for the deployment before calling it.

    LiteLLM's own rule, for Chat Completions to Azure: a model of the GPT-5 or
    GPT-6 series, by base_model or deployment name, takes none only when the
    price table marks it as supporting none; otherwise LiteLLM answers 400.
    Other providers take the request as it is. The Responses API's form,
    reasoning.effort, is LiteLLM's to check: since 1.105.6 it answers a level
    the table marks as unsupported with its own 400.
    """
    params = deployment.get("litellm_params") if isinstance(deployment, dict) else None
    model = params.get("model") if isinstance(params, dict) else None
    if not isinstance(model, str) or not model.startswith("azure/"):
        return False
    info = deployment.get("model_info")
    base = params.get("base_model") or (info.get("base_model") if isinstance(info, dict) else None)
    name = base if isinstance(base, str) and base else model[len("azure/"):]
    try:
        from litellm.llms.azure.chat.gpt_5_transformation import AzureOpenAIGPT5Config
    except Exception:  # noqa: BLE001
        return False
    try:
        return bool(AzureOpenAIGPT5Config.is_model_gpt_5_model(name)) and not AzureOpenAIGPT5Config._supports_reasoning_effort_level(name, "none")
    except Exception:  # noqa: BLE001
        return True


def strip_refused_no_reasoning(request_kwargs, deployments):
    """Remove a reasoning effort of none that a deployment refuses, in place; whether it was removed.

    The same as LiteLLM's drop_params would do for this one parameter: the model
    then reasons at its default effort. Removed when any of the deployments
    refuses it, as for the sampling parameters.
    """
    effort = request_kwargs.get("reasoning_effort")
    level = effort.get("effort") if isinstance(effort, dict) else effort
    if level != "none" or not any(refuses_no_reasoning(deployment) for deployment in deployments):
        return False
    request_kwargs.pop("reasoning_effort", None)
    return True

def access_groups(deployment):
    """The access groups a deployment belongs to."""
    info = deployment.get("model_info") if isinstance(deployment, dict) else None
    groups = info.get("access_groups") if isinstance(info, dict) else None
    if not isinstance(groups, (list, tuple)):
        return ()
    return tuple(group for group in groups if isinstance(group, str))


def copilot_groups(deployment):
    """The per-Agent GitHub Copilot access groups a deployment belongs to."""
    return tuple(group for group in access_groups(deployment) if group.startswith(COPILOT_GROUP_PREFIX))


def caller_key(request_kwargs):
    """The calling key the proxy put on the request, or None when there is none."""
    if not isinstance(request_kwargs, dict):
        return None
    for name in METADATA_BUCKETS:
        bucket = request_kwargs.get(name)
        if isinstance(bucket, dict) and bucket.get(CALLER_KEY) is not None:
            return bucket[CALLER_KEY]
    return None


def caller_models(request_kwargs):
    """The models the calling key was granted, or None when the caller is unknown.

    An empty list, as the master key has, means every model to LiteLLM; it
    holds no access group, so it reaches no Copilot route.
    """
    caller = caller_key(request_kwargs)
    if caller is None:
        return None
    models = caller.get("models") if isinstance(caller, dict) else getattr(caller, "models", None)
    return [model for model in models if isinstance(model, str)] if isinstance(models, (list, tuple)) else []


def operator_groups(caller):
    """The access groups of a key the CetiRun operator issued, or None for any other caller.

    Only the key the proxy authenticated counts, as LiteLLM's UserAPIKeyAuth,
    never a dict. The master key, which has no models, keys granted "*",
    every model or models by name, are other callers.
    """
    if not isinstance(caller, UserAPIKeyAuth):
        return None
    models = caller.models
    if not models or not isinstance(models, (list, tuple)):
        return None
    if not all(isinstance(name, str) and (name.startswith(OPERATOR_GROUP_PREFIX) or name == NO_MODELS)
               for name in models):
        return None
    return frozenset(models)


def classifier_call(request_kwargs):
    """Whether the request is the call an auto router makes to classify a request."""
    return isinstance(request_kwargs, dict) and any(
        isinstance(bucket := request_kwargs.get(name), dict) and bucket.get(CALL_ORIGIN) == CLASSIFIER_ORIGIN
        for name in METADATA_BUCKETS
    )


def request_call_type(request_kwargs):
    """The API the request came through, as the proxy recorded it, or None when it recorded none.

    The proxy sets the request's logging object for every request it serves,
    before routing, from the route that received the request rather than from
    its body. Calls LiteLLM makes itself, such as an auto router's classifier
    call, have none.
    """
    if not isinstance(request_kwargs, dict):
        return None
    call_type = getattr(request_kwargs.get(LOGGING_OBJECT), "call_type", None)
    call_type = getattr(call_type, "value", call_type)
    return call_type if isinstance(call_type, str) and call_type else None


def model_groups(model, deployments, team_id):
    """The access groups of every deployment of the model, available or not.

    The callback is given only the deployments still available; the live
    router knows the rest. Without it, the deployments given are all there is
    to go on.
    """
    router = live_router()
    if router is not None:
        try:
            return set(router.get_model_access_groups(model_name=model, team_id=team_id))
        except Exception:  # the answer only chooses between two refusals; fall back to the deployments given
            pass
    return {group for deployment in deployments for group in access_groups(deployment)}


def conversation_model(name, router):
    """Whether a model the listing offers answers conversations: one of its deployments has a chat mode
    or none, as the CetiRun operator decides which models to give Agents. Without the router, or for a
    name with no deployment of its own, the name is kept."""
    if router is None:
        return True
    try:
        deployments = router.get_model_list(model_name=name) or []
    except Exception:  # a listing never fails on this filter
        return True
    if not deployments:
        return True
    return any(((deployment.get("model_info") or {}).get("mode") if isinstance(deployment, dict) else None) in CHAT_MODES
               for deployment in deployments)


def evaluation_only(deployment):
    """Whether the deployment is a decision model, which LiteLLM lists with the evaluation mode."""
    info = deployment.get("model_info") if isinstance(deployment, dict) else None
    return isinstance(info, dict) and info.get("mode") == "evaluation"


def decision_context(deployments):
    """The decision model's context in tokens: the smallest model_info.max_input_tokens its deployments
    state, or Microsoft-Decision-1's when none states one."""
    sizes = []
    for deployment in deployments:
        info = deployment.get("model_info") if isinstance(deployment, dict) else None
        size = info.get("max_input_tokens") if isinstance(info, dict) else None
        if isinstance(size, int) and not isinstance(size, bool) and size > 0:
            sizes.append(size)
    return min(sizes) if sizes else DECISION_CONTEXT_TOKENS


def utf8_size(text):
    """The bytes text takes as UTF-8. A lone surrogate, which JSON can carry, counts as three."""
    return len(text.encode("utf-8", "surrogatepass"))


def utf8_head(text, size):
    """The longest beginning of text that takes at most size bytes of UTF-8, cut between characters."""
    return text.encode("utf-8", "surrogatepass")[:max(size, 0)].decode("utf-8", "ignore")


def utf8_tail(text, size):
    """The longest end of text that takes at most size bytes of UTF-8, cut between characters."""
    if size <= 0:
        return ""
    data = text.encode("utf-8", "surrogatepass")
    if size >= len(data):
        return text
    return data[len(data) - size:].decode("utf-8", "ignore")


def beginning_and_end(text, size):
    """text in at most size bytes of UTF-8: whole when it fits, else its beginning and its end with an
    elision line between."""
    if utf8_size(text) <= size:
        return text
    room = size - len(ELISION)
    if room <= 0:
        return utf8_tail(text, size)
    head = int(room * HEAD_SHARE)
    return utf8_head(text, head) + ELISION + utf8_tail(text, room - head)


def question_size(questions):
    """The bytes the questions take as compact JSON, which the decision model reads with the request."""
    try:
        text = json.dumps(questions, ensure_ascii=False, separators=(",", ":"), default=str)
    except (TypeError, ValueError, RecursionError):
        text = repr(questions)
    return utf8_size(text)


def bounded_classifier_state(state, budget):
    """(the request a router sends a decision model in at most budget bytes of UTF-8, what it keeps);
    (state, None) when it fits already, so a second pass changes nothing.

    The newest message, after the last "Classify this message:" line, which LiteLLM writes last, is kept
    whole when it fits in the budget less what the rest needs, a quarter of the budget at most; otherwise
    the line and the message's beginning and end. The rest, the caller's system prompt and earlier
    messages, keeps its beginning and its end in the room left. Without that line, the request keeps its
    beginning and its end.
    """
    if utf8_size(state) <= budget:
        return state, None
    split = state.rfind(CLASSIFY_MARKER)
    if split < 0:
        return beginning_and_end(state, budget), KEPT_REQUEST_ENDS
    rest, newest = state[:split], state[split:]
    reserved = min(utf8_size(rest), budget // 4)
    if budget - reserved < len(CLASSIFY_MARKER):
        return beginning_and_end(state, budget), KEPT_REQUEST_ENDS
    kept = KEPT_NEWEST
    if utf8_size(newest) > budget - reserved:
        message = newest[len(CLASSIFY_MARKER):]
        newest = CLASSIFY_MARKER + beginning_and_end(message, budget - reserved - len(CLASSIFY_MARKER))
        kept = KEPT_BOTH_ENDS
    return beginning_and_end(rest, budget - utf8_size(newest)) + newest, kept


def bound_classifier_request(request_kwargs, deployments):
    """Shorten, in place, the request a router sends a decision model to classify, to fit the model's
    context: (bytes before, bytes after, the context in tokens, what it keeps), or None when it fits or is
    not such a request.

    LiteLLM's complexity router sends a jev classifier the caller's whole system prompt, up to three
    earlier user messages and the newest one, however long they are. Microsoft-Decision-1 refuses a
    request over its context, and the router then classified the request with its fallback instead. The
    decision model takes as many tokens as its context, less the questions, the service's own template
    and the decision token. A token of the byte-level tokenizers decision models use is at least one
    byte of UTF-8 (Microsoft-Decision-1 is post-trained from Qwen3.5-9B), so a request of at most that
    many bytes fits in any language: Chinese text takes about three bytes a token, English about four.
    That leaves several times what the router needs; LiteLLM quotes at most 8,000 characters of earlier
    messages.

    Only a router's own call is changed: marked as the classifier's, with the Decisions API's state and
    questions. A client's own call to the Decisions API, and a classifier call that asks a chat model,
    are left as they are.
    """
    if not classifier_call(request_kwargs):
        return None
    state, questions = request_kwargs.get("state"), request_kwargs.get("questions")
    if not isinstance(state, str) or not isinstance(questions, dict):
        return None
    context = decision_context(deployments)
    budget = max(context - question_size(questions) - DECISION_RESERVE_BYTES, DECISION_MINIMUM_BYTES)
    shortened, kept = bounded_classifier_state(state, budget)
    if kept is None:
        return None
    request_kwargs["state"] = shortened
    return utf8_size(state), utf8_size(shortened), context, kept


def live_router():
    """The proxy's router, or None before it has one."""
    return getattr(sys.modules.get(PROXY_SERVER_MODULE), "llm_router", None)


def router_params(name, router, prefix=AUTO_ROUTER_PREFIX):
    """The litellm_params of the auto router the model name stands for, one whose model starts with prefix,
    or None when the name is not one or the router cannot tell."""
    if router is None or not isinstance(name, str) or not name:
        return None
    try:
        deployments = router.get_model_list(model_name=name) or []
    except Exception:  # noqa: BLE001  # a request never fails on this lookup
        return None
    for deployment in deployments:
        params = deployment.get("litellm_params") if isinstance(deployment, dict) else None
        model = params.get("model") if isinstance(params, dict) else None
        if isinstance(model, str) and model.startswith(prefix):
            return params
    return None


def tier_models(value):
    """The model names a tier of a complexity router names: one name, or a list of names or of entries with a
    model_name."""
    names = []
    for entry in value if isinstance(value, list) else [value]:
        name = entry.get("model_name") if isinstance(entry, dict) else entry
        if isinstance(name, str) and name and name not in names:
            names.append(name)
    return names


def router_tiers(params):
    """[(tier, its model names)] of a complexity router, LiteLLM's tiers lowest first, then any other tiers in
    the order the configuration lists them."""
    config = params.get("complexity_router_config")
    tiers = config.get("tiers") if isinstance(config, dict) else None
    if not isinstance(tiers, dict):
        return []
    rank = {tier: index for index, tier in enumerate(TIER_ORDER)}
    ordered = sorted(tiers, key=lambda tier: rank.get(tier, len(TIER_ORDER)))
    return [(tier, models) for tier in ordered if (models := tier_models(tiers[tier]))]


def router_default_model(params, tiers):
    """The model a complexity router sends a request it cannot classify, as LiteLLM chooses it: the router's
    complexity_router_default_model, else the first model of its fallback_tier, of MEDIUM, then of SIMPLE."""
    default = params.get("complexity_router_default_model")
    if isinstance(default, str) and default:
        return default
    config = params.get("complexity_router_config")
    fallback_tier = config.get("fallback_tier") if isinstance(config, dict) else None
    by_tier = dict(tiers)
    for tier in (fallback_tier, *DEFAULT_MODEL_TIERS):
        if isinstance(tier, str) and by_tier.get(tier):
            return by_tier[tier][0]
    return None


def tier_fallbacks(params, is_router):
    """{model: the models to try after it} for each model a complexity router sends requests to answer.

    After a tier's model: the other models of its tier, the models of the tiers above it, lowest first, then
    the router's default model, then the models of the tiers below it, highest first. A model of several
    tiers follows its highest. The default model, when it is no tier's model, is placed like the medium
    tier, or the lowest. A model that is itself a router is left out.
    """
    tiers = router_tiers(params)
    default = router_default_model(params, tiers)

    def chain(model, position, in_tier):
        peers = tiers[position][1] if in_tier else []
        above = [name for _, names in tiers[position + 1 if in_tier else position:] for name in names]
        below = [name for _, names in reversed(tiers[:position]) for name in names]
        ordered = []
        for name in (*peers, *above, *([default] if default else []), *below):
            if name != model and name not in ordered and not is_router(name):
                ordered.append(name)
        return ordered

    chains = {}
    for position, (_, names) in enumerate(tiers):
        for name in names:
            chains[name] = chain(name, position, True)
    if default and default not in chains and not is_router(default):
        tier_names = [tier for tier, _ in tiers]
        position = tier_names.index("MEDIUM") if "MEDIUM" in tier_names else 0
        chains[default] = chain(default, position, False)
    return {name: ordered for name, ordered in chains.items() if ordered and not is_router(name)}


def add_tier_fallbacks(data, router):
    """Give a request for a complexity router fallbacks to the router's other models, in place; the router's
    name when it does, else None.

    Left as they are: a request that names its own fallbacks or turns them off, one whose key or team router
    settings name fallbacks, and the models the router setting fallbacks already names, or the router itself.
    Fallbacks for every model ("*") follow a model's own.
    """
    if not isinstance(data, dict) or FALLBACKS in data or data.get(DISABLE_FALLBACKS):
        return None
    override = data.get(ROUTER_SETTINGS_OVERRIDE)
    if isinstance(override, dict) and override.get(FALLBACKS):
        return None
    name = data.get("model")
    params = router_params(name, router, COMPLEXITY_ROUTER)
    if params is None:
        return None
    configured = [entry for entry in getattr(router, "fallbacks", None) or [] if isinstance(entry, dict) and entry]
    keys = {next(iter(entry)) for entry in configured}
    if name in keys:
        return None
    generic = next((entry[GENERIC_FALLBACK] for entry in configured
                    if isinstance(entry.get(GENERIC_FALLBACK), list)), [])
    added = []
    for model, ordered in tier_fallbacks(params, lambda other: router_params(other, router) is not None).items():
        if model in keys:
            continue
        ordered = ordered + [target for target in generic
                             if isinstance(target, str) and target != model and target not in ordered]
        added.append({model: ordered})
    if not added:
        return None
    data[FALLBACKS] = [*configured, *added]
    return name


def strip_retry_override(data):
    """Remove the retry settings from the router settings LiteLLM merges into the request, in place; whether
    there were any."""
    override = data.get(ROUTER_SETTINGS_OVERRIDE) if isinstance(data, dict) else None
    if not isinstance(override, dict) or not any(name in override for name in RETRY_PARAMETERS):
        return False
    data[ROUTER_SETTINGS_OVERRIDE] = {key: value for key, value in override.items() if key not in RETRY_PARAMETERS}
    return True


def hide_router_details():
    """Keep LiteLLM from appending its internal model groups, fallbacks and timeouts to the errors it returns."""
    if hasattr(litellm, "expose_router_debug_in_errors"):
        litellm.expose_router_debug_in_errors = False


def has_deployment(name, router):
    """Whether the name is a model with a deployment of its own. A Team's router is registered under the
    same name as its access group, so being one of the key's groups does not make a name a placeholder.
    Without the router, or when it cannot answer, it is not known to be a model."""
    if router is None:
        return False
    try:
        return bool(router.get_model_list(model_name=name))
    except Exception:  # a listing never fails on this filter
        return False


def without_cooldown_list(text):
    """The text without LiteLLM's list of deployments in cooldown, or None when it has none."""
    if not isinstance(text, str) or NO_DEPLOYMENTS not in text or "cooldown_list=" not in text:
        return None
    return COOLDOWN_LIST.sub("", text)


def remove_cooldown_list(error):
    """Remove the list from the error's message in place. True when there was one."""
    changed = False
    arguments = getattr(error, "args", ())
    if isinstance(arguments, tuple):
        cleaned = tuple(without_cooldown_list(argument) or argument for argument in arguments)
        if cleaned != arguments:
            error.args = cleaned
            changed = True
    message = getattr(error, "message", None)
    cleaned = without_cooldown_list(message)
    if cleaned is not None:
        try:
            error.message = cleaned
            changed = True
        except (AttributeError, TypeError):
            pass
    return changed


class ModelRefused(HTTPException):
    """The refusal of a model the calling key may not use.

    LiteLLM treats a 403 HTTPException from a callback as a guardrail's
    decision: it does not retry it, fall back or cool the model down, and the
    caller gets a 403 permission_error with the detail as its message. Without
    deployments LiteLLM would answer "No deployments available" with 429, which
    agents retry; the message must not contain that text, which the proxy maps
    to 429 too.
    """

    def __init__(self, model):
        super().__init__(status_code=403, detail=f"This key may not use the model {model}.")
        self.refused_model = model


class DecisionModelRefused(HTTPException):
    """The refusal of a decision model asked for through an API other than LiteLLM's Decisions API.

    Raised before any deployment is called, so the provider never sees the
    request and LiteLLM does not cool the deployment down; as for ModelRefused,
    LiteLLM treats a 400 HTTPException from a callback as a guardrail's
    decision and neither retries nor falls back.
    """

    def __init__(self, model, call_type):
        super().__init__(status_code=400,
                         detail=f"The model {model} is a decision model: call it through /v1/systemone or /v1/decisions.")
        self.refused_model = model
        self.refused_call_type = call_type


class Guard(CustomLogger):
    """Request sanitization, the deployments a key reaches, and caller-facing errors."""

    async def async_pre_call_hook(self, user_api_key_dict, cache, data, call_type):
        if isinstance(data, dict):
            for name in REQUEST_PARAMETERS:
                data.pop(name, None)
            strip_retry_override(data)
            add_tier_fallbacks(data, live_router())
        return None

    async def async_filter_deployments(
        self,
        model,
        healthy_deployments,
        messages,
        request_kwargs=None,
        parent_otel_span=None,
    ):
        """Leave out the deployments the calling key may not use.

        For every caller, the Copilot routes whose access group the key does
        not hold. For a key the operator issued, then also every deployment
        outside its access groups, except on a router's classifier call. An
        auto router's tier selection goes through this again with the same
        request, so a tier is held to the same rules. A request for a decision
        model through any API but the Decisions API is refused, for every
        caller. A router's call to a decision model to classify a request is
        shortened to fit the model's context. While encrypted content
        affinity applies to the model, unmarked or shortened encrypted reasoning
        is removed from a Responses API request first. A fallback after a
        deployment refused the request's encrypted reasoning is ended here, so
        the client gets that first error. A request to a model that reasons loses
        the sampling parameters the model refuses, and a request to Azure OpenAI
        the fields it refuses in items that another provider returned.
        """
        refusal = unreadable_reasoning_refusal(request_kwargs)
        if refusal is not None:
            verbose_proxy_logger.warning(
                "CetiRun guard: no fallback to %s: a deployment could not read the request's encrypted reasoning, "
                "and no other deployment can.", model)
            raise refusal_error(model, refusal)
        if isinstance(request_kwargs, dict) and isinstance(request_kwargs.get("input"), list) and affinity_enabled(model):
            removed = strip_unmarked_reasoning(request_kwargs["input"])
            if removed:
                verbose_proxy_logger.warning(
                    "CetiRun guard: removed the encrypted content of %d reasoning item%s without the Gateway's mark, "
                    "or shortened by the client, from a request for %s; no deployment may be able to read it.",
                    removed, "" if removed == 1 else "s", model)
        granted = set(caller_models(request_kwargs) or ())
        kept = [
            deployment
            for deployment in healthy_deployments
            if not (groups := copilot_groups(deployment)) or granted.intersection(groups)
        ]
        caller = caller_key(request_kwargs)
        held = operator_groups(caller)
        decision_model = bool(kept) and not classifier_call(request_kwargs) and all(map(evaluation_only, kept))
        if held is not None and decision_model:
            # A decision model answers only a router's classifier calls. Called directly, the provider
            # refused the request and LiteLLM cooled the deployment down, so every router classifying
            # with it fell back to its default classifier for everyone.
            raise ModelRefused(model)
        if decision_model:
            # Every other caller may use the Decisions API itself. A request through any other API is
            # refused here, where no deployment has been called yet; one the proxy recorded no API for,
            # such as a call LiteLLM makes itself, is left to LiteLLM.
            call_type = request_call_type(request_kwargs)
            if call_type is not None and call_type not in DECISIONS_CALL_TYPES:
                raise DecisionModelRefused(model, call_type)
        if held is not None and kept and not classifier_call(request_kwargs):
            allowed = [deployment for deployment in kept if held.intersection(access_groups(deployment))]
            if not allowed:
                # Nothing left, though the key may use the model: its deployments are
                # unavailable (cooldown, health checks), which LiteLLM reports itself.
                if held.intersection(model_groups(model, healthy_deployments, getattr(caller, "team_id", None))):
                    return allowed
                raise ModelRefused(model)
            kept = allowed
        shortened = bound_classifier_request(request_kwargs, kept) if kept else None
        if shortened is not None:
            before, after, context, what = shortened
            verbose_proxy_logger.warning(
                "CetiRun guard: shortened a router's request to the decision model %s from %d to %d bytes, to fit "
                "its %d-token context; kept %s.", model, before, after, context, what)
        if isinstance(request_kwargs, dict) and kept:
            router = routing_router(model, request_kwargs)
            target = f"the router {router} sent to" if router else "for"
            if any(map(azure_openai, kept)):
                statuses, ids = strip_azure_refused_fields(request_kwargs.get("input"))
                if statuses or ids:
                    verbose_proxy_logger.warning(
                        "CetiRun guard: removed the status of %d reasoning item%s and the ID of %d item%s longer than "
                        "%d characters from a request %s %s; Azure OpenAI refuses both.",
                        statuses, "" if statuses == 1 else "s", ids, "" if ids == 1 else "s", AZURE_MAX_ITEM_ID,
                        target, model)
            # First: without the effort of none, the model reasons, and the sampling
            # parameters it then refuses go too.
            if strip_refused_no_reasoning(request_kwargs, kept):
                verbose_proxy_logger.warning(
                    "CetiRun guard: removed reasoning_effort none from a request %s %s, which does not take it; "
                    "the model reasons at its default effort.", target, model)
            removed = strip_sampling(request_kwargs, kept)
            if removed:
                verbose_proxy_logger.warning(
                    "CetiRun guard: removed %s from a request %s %s, which takes no sampling parameters while it "
                    "reasons.",
                    ", ".join(removed), target, model)
        return kept

    async def async_filter_listed_models(self, user_api_key_dict, model_names):
        """Keep a key the operator issued from listing what is not a model it can talk to.

        LiteLLM lists an access group of the key that has no deployment, such as an Agent's GitHub
        Copilot group before its owner signs in, as if it were a model (a Team's router, registered under
        its group's name, is a model and stays), and lists a decision or
        embedding model the key may reach because a router classifies with it. The CetiRun operator
        leaves both out of the Agent's configuration; the listing does the same, and a lookup of
        either by name answers 404. Every other caller sees LiteLLM's listing.
        """
        held = operator_groups(user_api_key_dict)
        if held is None:
            return model_names
        router = live_router()
        return [name for name in model_names
                if not (isinstance(name, str)
                        and ((name in held and not has_deployment(name, router)) or not conversation_model(name, router)))]

    async def async_post_call_failure_hook(self, request_data, original_exception, user_api_key_dict,
                                           traceback_str=None):
        # Logged here rather than where it is raised: a router also asks the
        # filter whether its choice can be served, so a refused request passes
        # through the filter more than once, and through this hook once.
        if isinstance(original_exception, ModelRefused):
            verbose_proxy_logger.warning(
                "CetiRun guard: refused the model %s to the key %s, which holds none of the model's access groups",
                original_exception.refused_model, getattr(user_api_key_dict, "key_alias", None))
        if isinstance(original_exception, DecisionModelRefused):
            verbose_proxy_logger.warning(
                "CetiRun guard: refused a %s request for the decision model %s from the key %s; a decision model "
                "answers only the Decisions API",
                original_exception.refused_call_type, original_exception.refused_model,
                getattr(user_api_key_dict, "key_alias", None))
        remove_cooldown_list(original_exception)
        return None


handler = Guard()
# The entrypoint imports this module before the proxy loads its configuration,
# so LiteLLM writes the Gateway's form of the mark from the first request, the
# proxy imports the replaced listing function, and litellm_settings in the
# configuration can still turn the router's details in errors on again. The
# image build checks that each replacement took.
PATCHED = {
    "mark_form": install_mark_form(),
    "failover_past_affinity": install_failover_past_affinity(),
    "router_alias_listing": install_router_alias_listing(),
    "savings_provider_lookup": install_savings_provider_lookup(),
}
hide_router_details()
