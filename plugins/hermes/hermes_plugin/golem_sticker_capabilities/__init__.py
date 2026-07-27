"""Hermes tools for Golem-managed WeChat sticker replies."""

from __future__ import annotations

import json
import os
import re
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
from .tool_schemas import (
    ATTACH_SCHEMA,
    COLLECT_SCHEMA,
    LIBRARY_INVENTORY_SCHEMA,
    MANAGE_RECENT_SCHEMA,
    LIBRARY_PICK_SCHEMA,
    LIBRARY_PREVIEW_SCHEMA,
    LIBRARY_SEARCH_SCHEMA,
    SEARCH_SCHEMA,
    SELECT_SCHEMA,
    SELECT_MANY_SCHEMA,
    SILENCE_RULE_ADD_SCHEMA,
)


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

_RELAY_ENVELOPE = "[Relay identity envelope]\n"
_MESSAGE_TEXT_MARKER = "\n[Message text]\n"
_NON_STICKER_MEDIA = ("视频", "图片", "照片", "文件", "链接", "语音", "歌曲", "音乐", "红包", "位置")
_PREVIEW_REQUIREMENT = (
    "[Current-turn Golem sticker action]\n"
    "The verified current Relay message requests a fresh sticker-library preview. "
    "You must call golem_sticker_library_preview exactly once in this turn. Stickers "
    "sent in earlier turns do not satisfy this request. Do not call inventory, "
    "library_search, select, or select_many first, and do not answer from memory."
)
_PICK_REQUIREMENT = (
    "[Current-turn Golem sticker action]\n"
    "The verified current Relay message requests a fresh sticker send. You must call "
    "golem_sticker_library_pick exactly once in this turn. A matching sticker sent in "
    "an earlier turn does not satisfy this request. Do not call library_search or "
    "select first, and do not answer from memory."
)
_UPDATE_RECENT_REQUIREMENT = (
    "[Current-turn Golem sticker action]\n"
    "The verified current Relay message explicitly replaces the description of the "
    "sticker just sent. You must call golem_sticker_library_manage_recent exactly once "
    "with action=update_description and the complete new description supplied by the "
    "user. Do not call inventory, search, image, collect, or selection tools first."
)
_DELETE_RECENT_REQUIREMENT = (
    "[Current-turn Golem sticker action]\n"
    "The verified current Relay message explicitly deletes the sticker just sent. You "
    "must call golem_sticker_library_manage_recent exactly once with action=delete and "
    "no description. Do not call inventory, search, image, collect, or selection tools first."
)
_ADD_SILENCE_RULE_REQUIREMENT = (
    "[Current-turn Golem silence-rule action]\n"
    "The verified current Relay message explicitly asks to add a Golem silence rule. "
    "You must call golem_silence_rule_add exactly once, using exact for whole/full "
    "matches, prefix for starts-with matches, or suffix for ends-with matches. Pass "
    "only the literal target text as value. Do not call skill_view, skill_manage, "
    "terminal, or file tools, and do not write any Skill rules JSON."
)


def _text(value: Any, maximum: int) -> str:
    if not isinstance(value, str):
        return ""
    return value.strip()[:maximum]


def _json_result(value: Dict[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def _verified_relay_message_text(user_message: Any, platform: Any) -> str:
    if str(platform or "").strip().lower() != "relay" or not isinstance(
        user_message, str
    ):
        return ""
    if not user_message.startswith(_RELAY_ENVELOPE):
        return ""
    envelope_text, marker, message_text = user_message[len(_RELAY_ENVELOPE) :].partition(
        _MESSAGE_TEXT_MARKER
    )
    if not marker:
        return ""
    try:
        envelope = json.loads(envelope_text.splitlines()[0])
    except (IndexError, json.JSONDecodeError):
        return ""
    if not isinstance(envelope, dict):
        return ""
    if (
        envelope.get("trust") != "verified_relay_current_actor"
        or envelope.get("trigger_kind") != "explicit"
        or envelope.get("require_visible_reply") is not True
    ):
        return ""
    return message_text.strip()


def _sticker_action_requirement(
    user_message: Any = "", platform: Any = "", **_: Any
) -> Dict[str, str] | None:
    message = _verified_relay_message_text(user_message, platform)
    if not message:
        return None
    if "静默规则" in message and any(
        phrase in message for phrase in ("添加", "加入", "加到", "加进", "新增")
    ) and not message.startswith(("怎么", "如何")):
        return {"context": _ADD_SILENCE_RULE_REQUIREMENT}
    recent_reference = any(
        phrase in message
        for phrase in ("刚才", "刚刚", "刚发", "这个表情", "那个表情", "上一个表情", "上一张表情")
    )
    if recent_reference and "表情" in message and not message.startswith(("怎么", "如何")):
        if any(phrase in message for phrase in ("删掉", "删除掉", "移除掉", "不要了")) or re.search(
            r"(?:^|请|把|帮我)(?:删除|移除).{0,40}表情", message
        ):
            return {"context": _DELETE_RECENT_REQUIREMENT}
        if re.search(
            r"(?:改成|改为|修改成|修改为|描述为|标签为|标记为|叫做)\s*[^，。！？?]{1,300}$",
            message,
        ):
            return {"context": _UPDATE_RECENT_REQUIREMENT}
    preview_request = "表情" in message and any(
        phrase in message
        for phrase in ("都有什么", "有哪些", "全部", "清单", "列表", "库存", "剩下", "其余")
    ) and any(phrase in message for phrase in ("发", "发送", "看看", "预览"))
    if preview_request:
        return {"context": _PREVIEW_REQUIREMENT}
    explicit_send = "表情" in message and (
        message.startswith(("发", "再发", "请发", "麻烦发"))
        or any(
            phrase in message
            for phrase in ("发给我", "给我发", "帮我发", "来一个", "来个", "整一个")
        )
    )
    elliptical_send = re.match(r"^(?:再)?(?:给我)?(?:发|来|整|甩)(?:一?个|张)?", message)
    if (explicit_send or elliptical_send) and not any(
        media in message for media in _NON_STICKER_MEDIA
    ):
        return {"context": _PICK_REQUIREMENT}
    return None


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


def _handle_library_search(args: Dict[str, Any], **_: Any) -> str:
    query, limit = _search_args(args)
    if _async_binding() is not None:
        raise CapabilityError(
            "The local sticker library is unavailable in an async completion turn"
        )
    result = _client.search_sticker_library(query, limit, _current_context())
    return _sticker_search_result(result, limit)


def _inventory_args(args: Dict[str, Any]) -> tuple[int, int]:
    if not isinstance(args, dict):
        raise CapabilityError("Inventory arguments must be an object")
    if set(args) - {"limit", "offset"}:
        raise CapabilityError("Inventory contains unsupported arguments")
    limit = args.get("limit", 20)
    offset = args.get("offset", 0)
    if not isinstance(limit, int) or isinstance(limit, bool) or limit < 1 or limit > 100:
        raise CapabilityError("limit must be an integer between 1 and 100")
    if (
        not isinstance(offset, int)
        or isinstance(offset, bool)
        or offset < 0
        or offset > 1000000
    ):
        raise CapabilityError("offset must be an integer between 0 and 1000000")
    return limit, offset


def _handle_library_inventory(args: Dict[str, Any], **_: Any) -> str:
    limit, offset = _inventory_args(args)
    if _async_binding() is not None:
        raise CapabilityError(
            "The local sticker library is unavailable in an async completion turn"
        )
    result = _client.sticker_library_inventory(limit, offset, _current_context())
    raw_items = result.get("items")
    total = result.get("total")
    if not isinstance(raw_items, list) or not isinstance(total, int) or isinstance(total, bool):
        raise CapabilityError("Golem sticker inventory returned an invalid result")
    items = []
    for item in raw_items[:limit]:
        candidate = _candidate_result(item)
        if candidate is not None:
            items.append(
                {
                    "id": candidate["id"],
                    "description": candidate["description"],
                }
            )
    return _json_result(
        {
            "scope": "global",
            "total": max(total, 0),
            "items": items,
            "limit": limit,
            "offset": offset,
            "has_more": bool(result.get("has_more", False)),
            "expires_in": result.get("expires_in"),
        }
    )


def _preview_args(args: Dict[str, Any]) -> tuple[int, int]:
    limit, offset = _inventory_args(args)
    if limit > 5:
        raise CapabilityError("limit must be an integer between 1 and 5")
    return limit, offset


def _handle_library_preview(args: Dict[str, Any], **_: Any) -> str:
    limit, offset = _preview_args(args)
    if _async_binding() is not None:
        raise CapabilityError(
            "Sticker library preview is unavailable in an async completion turn"
        )
    result = _client.preview_sticker_library(limit, offset, _current_context())
    integer_fields = ("staged_count", "total", "next_offset", "remaining_count")
    if any(
        not isinstance(result.get(field), int) or isinstance(result.get(field), bool)
        for field in integer_fields
    ):
        raise CapabilityError("Golem sticker library preview returned invalid counts")
    staged = result.get("staged") is True
    staged_count = result["staged_count"]
    if staged != (staged_count > 0) or staged_count > limit:
        raise CapabilityError("Golem sticker library preview returned an invalid result")
    raw_descriptions = result.get("descriptions")
    if not isinstance(raw_descriptions, list) or len(raw_descriptions) != staged_count:
        raise CapabilityError("Golem sticker library preview returned invalid descriptions")
    output = {
        "staged": staged,
        "staged_count": staged_count,
        "total": max(result["total"], 0),
        "offset": offset,
        "next_offset": max(result["next_offset"], 0),
        "remaining_count": max(result["remaining_count"], 0),
        "has_more": bool(result.get("has_more", False)),
        "descriptions": [_text(value, 300) for value in raw_descriptions],
    }
    if staged:
        output["effect_only_token"] = _effect_only_token(result)
    return _json_result(output)


def _handle_library_pick(args: Dict[str, Any], **_: Any) -> str:
    query, limit = _search_args(args)
    if _async_binding() is not None:
        raise CapabilityError(
            "Sticker library pick is unavailable in an async completion turn"
        )
    result = _client.pick_sticker_library(query, limit, _current_context())
    match_count = result.get("match_count")
    if not isinstance(match_count, int) or isinstance(match_count, bool) or match_count < 0:
        raise CapabilityError("Golem sticker library pick returned an invalid match count")
    if result.get("staged") is not True:
        if match_count != 0:
            raise CapabilityError("Golem sticker library pick returned an invalid result")
        return _json_result({"staged": False, "match_count": 0})
    return _json_result(
        {
            "staged": True,
            "match_count": match_count,
            "effect_only_token": _effect_only_token(result),
            "description": _text(result.get("description"), 300),
        }
    )


def _collect_args(args: Dict[str, Any]) -> tuple[str, str]:
    if not isinstance(args, dict):
        raise CapabilityError("Collection arguments must be an object")
    if set(args) - {"candidate_id", "description"}:
        raise CapabilityError("Collection contains unsupported arguments")
    candidate_id = args.get("candidate_id")
    description = args.get("description")
    if not isinstance(candidate_id, str) or not candidate_id.strip():
        raise CapabilityError("candidate_id is required")
    if not isinstance(description, str) or not description.strip():
        raise CapabilityError("description is required")
    candidate_id = candidate_id.strip()
    description = description.strip()
    if len(candidate_id) > 128:
        raise CapabilityError("candidate_id is too long")
    if len(description) > 300:
        raise CapabilityError("description must be at most 300 characters")
    return candidate_id, description


def _handle_collect(args: Dict[str, Any], **_: Any) -> str:
    candidate_id, description = _collect_args(args)
    if _async_binding() is not None:
        raise CapabilityError(
            "Sticker collection is unavailable in an async completion turn"
        )
    result = _client.collect_current_sticker(
        candidate_id, description, _current_context()
    )
    if result.get("stored") is not True:
        raise CapabilityError("Golem did not store the selected sticker")
    return _json_result(
        {
            "stored": True,
            "description": _text(result.get("description"), 300),
            "asset_created": bool(result.get("asset_created", False)),
            "label_created": bool(result.get("label_created", False)),
        }
    )


def _manage_recent_args(args: Dict[str, Any]) -> tuple[str, str]:
    if not isinstance(args, dict):
        raise CapabilityError("Sticker management arguments must be an object")
    if set(args) - {"action", "description"}:
        raise CapabilityError("Sticker management contains unsupported arguments")
    action = args.get("action")
    description = args.get("description", "")
    if action not in {"update_description", "delete"}:
        raise CapabilityError("action must be update_description or delete")
    if not isinstance(description, str):
        raise CapabilityError("description must be a string")
    description = description.strip()
    if action == "update_description" and not description:
        raise CapabilityError("description is required for update_description")
    if action == "delete" and description:
        raise CapabilityError("description must be omitted for delete")
    if len(description) > 300:
        raise CapabilityError("description must be at most 300 characters")
    return action, description


def _handle_manage_recent(args: Dict[str, Any], **_: Any) -> str:
    action, description = _manage_recent_args(args)
    if _async_binding() is not None:
        raise CapabilityError(
            "Sticker library management is unavailable in an async completion turn"
        )
    result = _client.manage_recent_sticker(
        action, description, _current_context()
    )
    found = result.get("found")
    if not isinstance(found, bool) or result.get("action") != action:
        raise CapabilityError("Golem sticker management returned an invalid result")
    output = {"found": found, "action": action}
    if found and action == "update_description":
        returned_description = _text(result.get("description"), 300)
        if returned_description != description:
            raise CapabilityError("Golem returned a different sticker description")
        output["description"] = returned_description
    return _json_result(output)


def _silence_rule_args(args: Dict[str, Any]) -> tuple[str, str]:
    if not isinstance(args, dict):
        raise CapabilityError("Silence rule arguments must be an object")
    if set(args) - {"match_type", "value"}:
        raise CapabilityError("Silence rule contains unsupported arguments")
    match_type = args.get("match_type")
    value = args.get("value")
    if match_type not in {"exact", "prefix", "suffix"}:
        raise CapabilityError("match_type must be exact, prefix, or suffix")
    if not isinstance(value, str):
        raise CapabilityError("value is required")
    value = value.strip()
    if not value:
        raise CapabilityError("value is required")
    if "\r" in value or "\n" in value:
        raise CapabilityError("value must be one line")
    if len(value) > 1000:
        raise CapabilityError("value must be at most 1000 characters")
    return match_type, value


def _handle_silence_rule_add(args: Dict[str, Any], **_: Any) -> str:
    match_type, value = _silence_rule_args(args)
    if _async_binding() is not None:
        raise CapabilityError(
            "Silence rule management is unavailable in an async completion turn"
        )
    result = _client.add_silence_rule(match_type, value, _current_context())
    if result.get("applied") is not True:
        raise CapabilityError("Golem did not apply the silence rule")
    if result.get("match_type") != match_type or result.get("value") != value:
        raise CapabilityError("Golem confirmed a different silence rule")
    created = result.get("created")
    if not isinstance(created, bool):
        raise CapabilityError("Golem returned an invalid silence rule result")
    return _json_result(
        {
            "applied": True,
            "created": created,
            "match_type": match_type,
            "value": value,
        }
    )


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


def _candidate_ids(args: Dict[str, Any]) -> list[str]:
    if not isinstance(args, dict):
        raise CapabilityError("Batch selection arguments must be an object")
    if set(args) - {"candidate_ids"}:
        raise CapabilityError("Batch selection contains unsupported arguments")
    raw_ids = args.get("candidate_ids")
    if not isinstance(raw_ids, list) or len(raw_ids) < 1 or len(raw_ids) > 5:
        raise CapabilityError("candidate_ids must contain between 1 and 5 items")
    candidate_ids = []
    for raw_id in raw_ids:
        if not isinstance(raw_id, str):
            raise CapabilityError("candidate_ids must contain strings")
        candidate_id = raw_id.strip()
        if not candidate_id or len(candidate_id) > 2048:
            raise CapabilityError("candidate_ids contains an invalid id")
        candidate_ids.append(candidate_id)
    if len(set(candidate_ids)) != len(candidate_ids):
        raise CapabilityError("candidate_ids must be distinct")
    return candidate_ids


def _handle_select_many(args: Dict[str, Any], **_: Any) -> str:
    if _async_binding() is not None:
        raise CapabilityError(
            "Batch sticker selection is unavailable in an async completion turn"
        )
    candidate_ids = _candidate_ids(args)
    result = _client.select_stickers(candidate_ids, _current_context())
    if result.get("staged") is not True or result.get("staged_count") != len(
        candidate_ids
    ):
        raise CapabilityError("Golem did not stage all selected stickers")
    raw_descriptions = result.get("descriptions")
    descriptions = []
    if isinstance(raw_descriptions, list):
        descriptions = [
            _text(value, 300) for value in raw_descriptions[: len(candidate_ids)]
        ]
    return _json_result(
        {
            "staged": True,
            "staged_count": len(candidate_ids),
            "effect_only_token": _effect_only_token(result),
            "descriptions": descriptions,
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
    ctx.register_hook("pre_llm_call", _sticker_action_requirement)
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
        name="golem_sticker_library_search",
        schema=LIBRARY_SEARCH_SCHEMA,
        handler=_handle_library_search,
        emoji="library-search",
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_library_inventory",
        schema=LIBRARY_INVENTORY_SCHEMA,
        handler=_handle_library_inventory,
        emoji="library-inventory",
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_library_preview",
        schema=LIBRARY_PREVIEW_SCHEMA,
        handler=_handle_library_preview,
        emoji="library-preview",
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_library_pick",
        schema=LIBRARY_PICK_SCHEMA,
        handler=_handle_library_pick,
        emoji="library-pick",
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_select_many",
        schema=SELECT_MANY_SCHEMA,
        handler=_handle_select_many,
        emoji="select-many",
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_collect_current_session",
        schema=COLLECT_SCHEMA,
        handler=_handle_collect,
        emoji="collect",
        **common,
    )
    ctx.register_tool(
        name="golem_sticker_library_manage_recent",
        schema=MANAGE_RECENT_SCHEMA,
        handler=_handle_manage_recent,
        emoji="library-manage",
        **common,
    )
    ctx.register_tool(
        name="golem_silence_rule_add",
        schema=SILENCE_RULE_ADD_SCHEMA,
        handler=_handle_silence_rule_add,
        emoji="silence-rule-add",
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
        name="golem_image_inspect_current_session",
        schema=_image_tools.INSPECT_SCHEMA,
        handler=_image_tools._handle_inspect,
        emoji="image-inspect",
        is_async=True,
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
