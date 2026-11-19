# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""One JSON line on stdout for every model call the Gateway handles.

LiteLLM writes no per-request record to its own log, and of its export
callbacks the Azure Blob Storage one needs a LiteLLM Enterprise license while
the others need a third-party service. A line on stdout reaches whatever
collects the cluster's container logs, such as Container Insights into a Log
Analytics workspace, with nothing more to deploy or authorize.

The record is metadata only: the caller (virtual key, user, Team, and the Agent
the CetiRun operator recorded on the key), the model, tokens, spend, timing and
the outcome. Request and response content is never written. Every string in it
is capped, and a line never exceeds MAX_LINE_BYTES (see ``shortened``).

A call to one of LiteLLM's auto routers also records the tier the router chose,
why, and its score; the model it chose is the call's provider model. A call
LiteLLM makes itself is marked with its origin in call_origin: the call an auto
router makes to classify a request, when the router uses a model for that, is
a line of its own with the classifier's origin, and a background health check
probe is "health_check", so usage figures can leave probes out. A call
LiteLLM made after an attempt of the request failed, to another model or to
another deployment of the same model, records the model the request was for,
how many times LiteLLM sent it on and the class of the first failure.

A client can ask LiteLLM for per-request debug output with
litellm_request_debug, which makes LiteLLM print that request's prompt and the
provider's raw response to the log. The image's guard callback (guard.py),
which is always registered, drops that field from every request, so content
stays out of the log, with the fields that would change the Gateway's retries.

The image's entrypoint registers ``handler`` with LiteLLM before the proxy
loads its configuration, when CETIRUN_REQUEST_LOG is set, rather than listing
it in litellm_settings.callbacks, which settings saved in LiteLLM's database
can replace.
"""

import datetime
import json
import math
import sys
from urllib.parse import urlsplit

from litellm.integrations.custom_logger import CustomLogger

LOG_TYPE = "cetirun.ai_gateway.request"
SCHEMA_VERSION = 1

# The container runtime splits a line longer than 16 KiB, and a collector that
# does not join the pieces again stores each as its own line, none of them
# valid JSON. Container Insights does that unless multiline logging is on.
MAX_LINE_BYTES = 15 * 1024
# Every string in a record is cut to FREE_TEXT_MAX_CHARS characters, apart from
# the request ID and the error text, which have limits of their own. Clients
# choose many of them: the session ID LiteLLM records as trace_id, the call ID
# header, the model name, the end user, the user agent and the tags.
FREE_TEXT_MAX_CHARS = 256
REQUEST_ID_MAX_CHARS = 1024
ERROR_MESSAGE_MAX_CHARS = 1024
MAX_REQUEST_TAGS = 20
# Metadata the CetiRun operator puts on each Agent's virtual key.
KEY_METADATA_PREFIX = "cetirun_"
OPERATOR_KEY_METADATA = ("cetirun_agent", "cetirun_agent_uid", "cetirun_user", "cetirun_team")
# LiteLLM tags every request with the client's user agent, which the record
# already carries in its own field.
USER_AGENT_TAG_PREFIX = "User-Agent:"
# Where LiteLLM marks a call it makes itself, such as an auto router's
# classifier call: in the call's own metadata, not in the logging payload.
CALL_ORIGIN_KEY = "internal_call_origin"
# The identity LiteLLM gives its background health check probes, as key hash,
# alias and Team. Clients can choose request tags, not these.
HEALTH_CHECK_ACCOUNT = "litellm-internal-health-check"
HEALTH_CHECK_ORIGIN = "health_check"


def call_origin(kwargs):
    """The origin LiteLLM recorded on a call it made itself, or None."""
    params = as_dict(kwargs.get("litellm_params"))
    for name in ("metadata", "litellm_metadata"):
        origin = as_dict(params.get(name)).get(CALL_ORIGIN_KEY)
        if isinstance(origin, str) and origin:
            return origin
    return None


def fallback(kwargs):
    """(the model the request asked for, the number of fallbacks, the class of the first failure) of a call
    LiteLLM made after an attempt of the request failed, to another model (a fallback) or to another
    deployment of the same model (a failover), or None for any other call.

    LiteLLM records them in the call's own metadata, not in the logging payload: the model group the
    request was for, how many times it sent the request on, and each failed attempt.
    """
    params = as_dict(kwargs.get("litellm_params"))
    for name in ("litellm_metadata", "metadata"):
        metadata = as_dict(params.get(name))
        attempts = metadata.get("attempted_fallbacks")
        if not finite(attempts) or attempts <= 0:
            continue
        failures = metadata.get("previous_models")
        first = as_dict(failures[0]) if isinstance(failures, (list, tuple)) and failures else {}
        return metadata.get("original_model_group"), attempts, first.get("exception_type")
    return None


def is_health_check(metadata):
    """Whether the call is one of LiteLLM's background health check probes."""
    return HEALTH_CHECK_ACCOUNT in (
        metadata.get("user_api_key_hash"),
        metadata.get("user_api_key_alias"),
        metadata.get("user_api_key_team_id"),
    )


def iso_time(timestamp):
    if not isinstance(timestamp, (int, float)) or timestamp <= 0:
        return None
    moment = datetime.datetime.fromtimestamp(timestamp, tz=datetime.timezone.utc)
    return moment.isoformat(timespec="milliseconds").replace("+00:00", "Z")


def milliseconds(start, end):
    if not isinstance(start, (int, float)) or not isinstance(end, (int, float)) or end < start:
        return None
    return round((end - start) * 1000)


def endpoint_origin(api_base):
    """Scheme and host of the provider endpoint, without any path, query or credentials."""
    if not api_base:
        return None
    parts = urlsplit(str(api_base))
    if not parts.scheme or not parts.hostname:
        return None
    origin = f"{parts.scheme}://{parts.hostname}"
    return f"{origin}:{parts.port}" if parts.port else origin


def finite(value):
    """Whether value is a number JSON can carry: not a bool, NaN or infinity."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return isinstance(value, int) or math.isfinite(value)


def first_number(*values):
    for value in values:
        if finite(value):
            return value
    return None


def as_dict(value):
    return value if isinstance(value, dict) else {}


def token_details(usage):
    """Cached, cache-write and reasoning tokens, in whichever shape the provider reported them."""
    prompt_details = as_dict(usage.get("prompt_tokens_details")) or as_dict(usage.get("input_tokens_details"))
    completion_details = as_dict(usage.get("completion_tokens_details")) or as_dict(usage.get("output_tokens_details"))
    return {
        "cached_tokens": first_number(prompt_details.get("cached_tokens"), usage.get("cache_read_input_tokens")),
        "cache_creation_tokens": first_number(
            usage.get("cache_creation_input_tokens"), prompt_details.get("cache_creation_tokens")
        ),
        "reasoning_tokens": first_number(completion_details.get("reasoning_tokens")),
    }


def dump(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), default=str)


def clip(value, limit=FREE_TEXT_MAX_CHARS):
    if value is None:
        return None
    text = str(value)
    return text if len(text) <= limit else text[:limit]


def without_empty(record):
    return {key: value for key, value in record.items() if value is not None and value != {} and value != []}


def shortened(record):
    """The record cut down until its line is at most MAX_LINE_BYTES.

    Every string in a record is capped, so this only happens when JSON escaping
    lengthens many characters: a control character takes six bytes. The error
    text goes first, then the client's tags, then key metadata beyond what the
    operator sets. If the line is still too long, the longest strings are cut
    until it fits: cutting n characters from a string shortens its JSON by at
    least n bytes. The record says "truncated": true. Should the line still be
    too long with every string empty, only the fields that identify the call
    remain.
    """
    cut = dict(record, truncated=True)
    if isinstance(cut.get("key_metadata"), dict):
        cut["key_metadata"] = dict(cut["key_metadata"])

    def excess():
        return len(dump(cut).encode("utf-8")) - MAX_LINE_BYTES

    over = excess()
    message = cut.get("error_message")
    if over > 0 and message:
        data = message.encode("utf-8")
        cut["error_message"] = data[: max(0, len(data) - over)].decode("utf-8", "ignore")
    if excess() > 0:
        cut.pop("request_tags", None)
    if excess() > 0 and isinstance(cut.get("key_metadata"), dict):
        cut["key_metadata"] = {
            name: value for name, value in cut["key_metadata"].items() if name in OPERATOR_KEY_METADATA
        }
    places = [cut] + ([cut["key_metadata"]] if isinstance(cut.get("key_metadata"), dict) else [])
    while (over := excess()) > 0:
        strings = [(place, name) for place in places for name, value in place.items() if isinstance(value, str) and value]
        if not strings:
            return {
                "log_type": LOG_TYPE,
                "schema_version": SCHEMA_VERSION,
                "time": clip(record.get("time"), 64),
                "status": clip(record.get("status"), 64),
                "call_id": clip(record.get("call_id"), 64),
                "truncated": True,
            }
        place, name = max(strings, key=lambda found: len(dump(found[0][found[1]]).encode("utf-8")))
        place[name] = place[name][: max(0, len(place[name]) - over)]
    return without_empty(cut)


class RequestLog(CustomLogger):
    """Writes the record of each completed or failed call to stdout."""

    def __init__(self, stream=None):
        super().__init__()
        self.stream = stream

    async def async_log_success_event(self, kwargs, response_obj, start_time, end_time):
        self.write(kwargs)

    async def async_log_failure_event(self, kwargs, response_obj, start_time, end_time):
        self.write(kwargs)

    def write(self, kwargs):
        payload = kwargs.get("standard_logging_object")
        if not isinstance(payload, dict):
            return
        stream = self.stream or sys.stdout
        stream.write(self.line(payload, call_origin(kwargs), fallback(kwargs)) + "\n")
        stream.flush()

    def record(self, payload, origin=None, fallback_details=None):
        metadata = as_dict(payload.get("metadata"))
        hidden = as_dict(payload.get("hidden_params"))
        usage = as_dict(metadata.get("usage_object")) or as_dict(hidden.get("usage_object"))
        error = as_dict(payload.get("error_information"))
        # Only the router's verdict: its signals and keywords quote the prompt,
        # and the tier's parameters can carry credentials.
        routing = as_dict(metadata.get("routing_decision"))
        if is_health_check(metadata):
            origin = HEALTH_CHECK_ORIGIN
        key_metadata = {
            clip(name): clip(value) if isinstance(value, str) else value
            for name, value in as_dict(metadata.get("user_api_key_auth_metadata")).items()
            if isinstance(name, str) and name.startswith(KEY_METADATA_PREFIX)
            and (isinstance(value, (str, bool)) or finite(value))
        }
        tags = [
            clip(tag)
            for tag in payload.get("request_tags") or []
            if isinstance(tag, str) and not tag.startswith(USER_AGENT_TAG_PREFIX)
        ][:MAX_REQUEST_TAGS]
        start = payload.get("startTime")
        end = payload.get("endTime")
        stream = payload.get("stream") is True
        status = payload.get("status")

        record = {
            "log_type": LOG_TYPE,
            "schema_version": SCHEMA_VERSION,
            "time": iso_time(end),
            "start_time": iso_time(start),
            "status": clip(status),
            "request_id": clip(payload.get("id"), REQUEST_ID_MAX_CHARS),
            "call_id": clip(payload.get("litellm_call_id")),
            "trace_id": clip(payload.get("trace_id")),
            "route": clip(metadata.get("user_api_key_request_route")),
            "call_type": clip(payload.get("call_type")),
            "call_origin": clip(origin),
            "stream": stream,
            "model": clip(payload.get("model_group") or payload.get("model")),
            "provider_model": clip(hidden.get("litellm_model_name") or payload.get("model")),
            "provider": clip(payload.get("custom_llm_provider")),
            "deployment_id": clip(payload.get("model_id")),
            "api_base": clip(endpoint_origin(payload.get("api_base") or hidden.get("api_base"))),
            "routing_tier": clip(routing.get("tier")),
            "routing_cause": clip(routing.get("cause")),
            "routing_score": first_number(routing.get("score")),
            # A keyword or a prompt too long for the decided tier moved it up.
            "routing_escalated": (
                True if routing.get("escalated") is True or routing.get("context_escalated") is True else None
            ),
            "classifier_model": clip(routing.get("classifier_model")),
            "classifier_cost": first_number(routing.get("classifier_cost")),
            "duration_ms": milliseconds(start, end),
            "time_to_first_token_ms": milliseconds(start, payload.get("completionStartTime")) if stream else None,
            "prompt_tokens": first_number(payload.get("prompt_tokens")),
            "completion_tokens": first_number(payload.get("completion_tokens")),
            "total_tokens": first_number(payload.get("total_tokens")),
            **token_details(usage),
            "spend": first_number(payload.get("response_cost")),
            "response_cache_hit": payload.get("cache_hit") if isinstance(payload.get("cache_hit"), bool) else None,
            "key_alias": clip(metadata.get("user_api_key_alias")),
            "key_hash": clip(metadata.get("user_api_key_hash")),
            "user_id": clip(metadata.get("user_api_key_user_id")),
            "team_id": clip(metadata.get("user_api_key_team_id")),
            "team_alias": clip(metadata.get("user_api_key_team_alias")),
            "end_user": clip(payload.get("end_user") or metadata.get("user_api_key_end_user_id")),
            "key_metadata": key_metadata,
            "client_ip": clip(payload.get("requester_ip_address") or metadata.get("requester_ip_address")),
            "user_agent": clip(payload.get("user_agent") or metadata.get("user_agent")),
            "request_tags": tags,
        }
        if fallback_details is not None:
            requested, attempts, cause = fallback_details
            # An attempt failed and LiteLLM sent the request on: model is
            # the call's, fallback_from the model the request was for, such
            # as an auto router whose tier's model failed.
            record.update(
                {
                    "fallback_from": clip(requested),
                    "attempted_fallbacks": first_number(attempts),
                    "fallback_cause": clip(cause),
                }
            )
        if status == "failure":
            message = error.get("error_message") or payload.get("error_str")
            record.update(
                {
                    "error_class": clip(error.get("error_class")),
                    "error_code": None if error.get("error_code") is None else clip(error.get("error_code")),
                    "error_provider": clip(error.get("llm_provider")),
                    "error_message": clip(message, ERROR_MESSAGE_MAX_CHARS) if message else None,
                    "rate_limit_type": clip(error.get("error_rate_limit_type")),
                    "budget_entity_type": clip(error.get("error_budget_entity_type")),
                }
            )
        return without_empty(record)

    def line(self, payload, origin=None, fallback_details=None):
        """The record as one line of JSON, never longer than MAX_LINE_BYTES."""
        record = self.record(payload, origin, fallback_details)
        text = dump(record)
        if len(text.encode("utf-8")) <= MAX_LINE_BYTES:
            return text
        return dump(shortened(record))


handler = RequestLog()
