"""Hermes tools for Golem-managed WeChat sticker replies."""

from __future__ import annotations

import json
import os
from typing import Any, Dict

from . import async_delivery_api as _async_api
from . import async_delivery_child_context as _async_child_context
from . import async_delivery_runtime as _async_runtime
from . import async_delivery_state as _async_state
from . import async_delivery_text as _async_text
from . import capability_client as _client
from . import cron_delivery as _cron_delivery
from . import image_tools as _image_tools
from . import video_async_tools as _video_async_tools
from . import video_fetch_tools as _video_fetch_tools
from . import video_tools as _video_tools
from . import vision_inspect as _vision
from .errors import CapabilityError
from .tool_schemas import ATTACH_SCHEMA, SEARCH_SCHEMA, SELECT_SCHEMA


TOOLSET = "golem_stickers"
INSPECT_ENABLED_ENV = "GOLEM_STICKER_INSPECT_ENABLED"
INSPECT_SCHEMA = _vision.INSPECT_SCHEMA
MAX_INVOCATION_ID_LENGTH = 256
SEARCH_PATH = _client.SEARCH_PATH
SELECT_PATH = _client.SELECT_PATH
_current_context = _client.current_context
_base_url = _client.base_url
_post = _client.post_json
_materialize = _client.materialize
_opener = _client._opener


def _text(value: Any, maximum: int) -> str:
    if not isinstance(value, str):
        return ""
    return value.strip()[:maximum]


def _json_result(value: Dict[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def _search_args(args: Dict[str, Any]) -> tuple[str, int]:
    if not isinstance(args, dict):
        raise CapabilityError("Search arguments must be an object")
    if set(args) - {"query", "limit"}:
        raise CapabilityError("Search contains unsupported arguments")
    raw_query = args.get("query")
    if not isinstance(raw_query, str):
        raise CapabilityError("query is required")
    query = raw_query.strip()
    if not query:
        raise CapabilityError("query is required")
    if len(query) > 80:
        raise CapabilityError("query must be at most 80 characters")
    raw_limit = args.get("limit", 5)
    if not isinstance(raw_limit, int) or isinstance(raw_limit, bool):
        raise CapabilityError("limit must be an integer between 1 and 10")
    if raw_limit < 1 or raw_limit > 10:
        raise CapabilityError("limit must be an integer between 1 and 10")
    return query, raw_limit


def _candidate_id(args: Dict[str, Any]) -> str:
    if not isinstance(args, dict):
        raise CapabilityError("Selection arguments must be an object")
    if set(args) - {"candidate_id"}:
        raise CapabilityError("Selection contains unsupported arguments")
    raw_candidate_id = args.get("candidate_id")
    if not isinstance(raw_candidate_id, str):
        raise CapabilityError("candidate_id is required")
    candidate_id = raw_candidate_id.strip()
    if not candidate_id:
        raise CapabilityError("candidate_id is required")
    if len(candidate_id) > 2048:
        raise CapabilityError("candidate_id is too long")
    return candidate_id


def _handle_search(args: Dict[str, Any], **_: Any) -> str:
    query, limit = _search_args(args)
    binding = _async_binding()
    if binding is not None:
        result = _async_api.sticker_search(binding, query, limit)
        return _sticker_search_result(result, limit)
    result = _post(
        SEARCH_PATH,
        {"query": query, "limit": limit, "context": _current_context()},
    )
    return _sticker_search_result(result, limit)


def _sticker_search_result(result: Dict[str, Any], limit: int) -> str:
    raw_candidates = result.get("candidates")
    if not isinstance(raw_candidates, list):
        raise CapabilityError("Golem sticker search returned no candidate list")
    candidates = []
    for item in raw_candidates[:limit]:
        candidate = _candidate_result(item)
        if candidate is not None:
            candidates.append(candidate)
    expires_in = result.get("expires_in")
    if not isinstance(expires_in, (int, float)) or isinstance(expires_in, bool):
        expires_in = None
    return _json_result({"candidates": candidates, "expires_in": expires_in})


def _candidate_result(item: Any) -> Dict[str, Any] | None:
    if not isinstance(item, dict):
        return None
    raw_candidate_id = item.get("id")
    if not isinstance(raw_candidate_id, str):
        return None
    candidate_id = raw_candidate_id.strip()
    if not candidate_id or len(candidate_id) > 2048:
        return None
    return {
        "id": candidate_id,
        "description": _text(item.get("description"), 300),
        "format": _text(item.get("format"), 32),
        "animated": bool(item.get("animated", False)),
    }


def _handle_select(args: Dict[str, Any], **_: Any) -> str:
    candidate_id = _candidate_id(args)
    result = _post(
        SELECT_PATH,
        {"candidate_id": candidate_id, "context": _current_context()},
    )
    if result.get("staged") is not True:
        raise CapabilityError("Golem did not stage the selected sticker")
    effect_only_token = _effect_only_token(result)
    return _json_result(
        {
            "staged": True,
            "effect_only_token": effect_only_token,
            "description": _text(result.get("description"), 300),
        }
    )


def _effect_only_token(result: Dict[str, Any]) -> str:
    raw_token = result.get("effect_only_token")
    if not isinstance(raw_token, str):
        raise CapabilityError("Golem did not return an effect-only completion token")
    token = raw_token.strip()
    if not token:
        raise CapabilityError("Golem did not return an effect-only completion token")
    if len(token) > 256:
        raise CapabilityError("Golem returned an invalid effect-only completion token")
    return token


def _handle_attach(args: Dict[str, Any], **_: Any) -> str:
    binding = _async_binding()
    if binding is None:
        return _handle_select(args)
    candidate_id = _candidate_id(args)
    invocation_id = _invocation_id()
    result = _async_api.sticker_send(binding, candidate_id, invocation_id)
    return _json_result(
        {
            "queued": True,
            "kind": "emoji",
            "outbox_id": _text(result.get("outbox_id"), 256),
            "sequence": result.get("sequence"),
        }
    )


def _invocation_id() -> str:
    raw = _async_state.current_tool_invocation_id.get()
    if not isinstance(raw, str):
        raise CapabilityError("Hermes tool invocation_id is unavailable")
    value = raw.strip()
    if not value or len(value) > MAX_INVOCATION_ID_LENGTH:
        raise CapabilityError("Hermes tool invocation_id is invalid")
    return value


def _async_binding() -> Any:
    binding = _async_state.current_delivery.get()
    if binding is not None:
        return binding
    delegation_id = _async_state.current_child_delegation_id.get()
    if not delegation_id:
        return None
    state = _async_state.wait_registration(
        delegation_id, _client._timeout_seconds()
    )
    if state is None or state.binding is None or state.status != "ready":
        raise CapabilityError("Golem async delivery binding is unavailable")
    return state.binding


async def _handle_inspect(args: Dict[str, Any], **kwargs: Any) -> str:
    return await _vision.handle_inspect(
        args,
        materialize=_materialize,
        current_context=_current_context,
        task_id=kwargs.get("task_id"),
    )


def _check_available() -> bool:
    return _client.token_configured()


def _inspect_enabled() -> bool:
    raw = os.getenv(INSPECT_ENABLED_ENV, "true").strip().lower()
    if raw in {"1", "true", "yes", "on"}:
        return True
    if raw in {"0", "false", "no", "off"}:
        return False
    raise CapabilityError(f"{INSPECT_ENABLED_ENV} must be true or false")


def register(ctx) -> None:
    _client.cache_token_from_environment()
    inspect_enabled = _inspect_enabled()
    async_enabled = _async_api.enabled()
    _async_child_context.install()

    if async_enabled:
        _async_runtime.install()
        _cron_delivery.install()
        ctx.register_middleware("tool_execution", _async_runtime.tool_execution)
        ctx.register_hook("on_gateway_startup", _async_runtime.gateway_startup)
        ctx.register_hook("pre_gateway_dispatch", _async_runtime.pre_gateway_dispatch)
        ctx.register_hook("on_session_reset", _async_runtime.session_reset)
    # Ambient authorization is required even when detached async delivery is
    # disabled, so keep these hooks independent of that feature.
    ctx.register_hook("pre_tool_call", _async_runtime.pre_tool_call)
    ctx.register_hook("transform_llm_output", _async_text.normalize_text)
    common = {
        "toolset": TOOLSET,
        "check_fn": _check_available,
        "requires_env": ["GOLEM_CAPABILITIES_TOKEN"],
    }
    ctx.register_tool(
        name="golem_sticker_search",
        schema=SEARCH_SCHEMA,
        handler=_handle_search,
        emoji="search",
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_attach",
        schema=ATTACH_SCHEMA,
        handler=_handle_attach,
        emoji="attach",
        **common,
    )
    if inspect_enabled:
        ctx.register_tool(
            name="golem_sticker_inspect",
            schema=INSPECT_SCHEMA,
            handler=_handle_inspect,
            emoji="inspect",
            is_async=True,
            **common,
        )
    ctx.register_tool(
        name="golem_image_search_current_session",
        schema=_image_tools.SEARCH_SCHEMA,
        handler=_image_tools._handle_search,
        emoji="image-search",
        **common,
    )
    ctx.register_tool(
        name="golem_image_read_current_session",
        schema=_image_tools.READ_SCHEMA,
        handler=_image_tools._handle_read,
        emoji="image-read",
        is_async=True,
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_select",
        schema=SELECT_SCHEMA,
        handler=_handle_select,
        emoji="select",
        **common,
    )
    _video_tools.register(ctx, common)
    _video_async_tools.register(ctx, common)
    _video_fetch_tools.register(ctx, common)
