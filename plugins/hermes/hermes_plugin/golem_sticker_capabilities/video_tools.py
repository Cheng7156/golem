"""Run-bound Hermes tools for Golem-managed WeChat video replies."""

from __future__ import annotations

import json
import os
import time
from typing import Any, Dict

from . import capability_client as _client
from . import video_async_context as _async_context
from .errors import CapabilityError, RetryableCapabilityError

VIDEO_ENABLED_ENV = "GOLEM_VIDEO_ENABLED"

SEARCH_SCHEMA = {
    "name": "golem_video_search",
    "description": (
        "Search configured video APIs only when the user explicitly asks to receive a "
        "video. Select a category matching the request, such as beauty, funny, animals, "
        "or general. This tool returns short-lived candidates and does not send anything."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "category": {
                "type": "string",
                "description": "Configured source category, for example beauty or funny.",
                "minLength": 1,
                "maxLength": 64,
            },
            "query": {
                "type": "string",
                "description": "Optional concise search text when the source supports search.",
                "maxLength": 160,
            },
            "provider_id": {
                "type": "string",
                "description": "Optional configured provider id; normally omit this.",
                "maxLength": 64,
            },
            "limit": {
                "type": "integer",
                "minimum": 1,
                "maximum": 5,
                "default": 3,
            },
        },
        "required": ["category"],
        "additionalProperties": False,
    },
}


SELECT_SCHEMA = {
    "name": "golem_video_select",
    "description": (
        "Prepare and stage one video candidate for the current WeChat reply. Call this "
        "repeatedly, in the requested order, when the user asks for multiple videos. "
        "The tool waits for download, validation, conversion, and durable staging. A "
        "successful result includes effect_only_token; return that exact token when no "
        "ordinary text reply is needed."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "candidate_id": {"type": "string", "minLength": 1, "maxLength": 2048},
        },
        "required": ["candidate_id"],
        "additionalProperties": False,
    },
}


def _text(value: Any, maximum: int) -> str:
    return value.strip()[:maximum] if isinstance(value, str) else ""


def _json(value: Dict[str, Any]) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def _validate_args(args: Dict[str, Any], allowed: set[str]) -> None:
    if not isinstance(args, dict):
        raise CapabilityError("Video tool arguments must be an object")
    if set(args) - allowed:
        raise CapabilityError("Video tool contains unsupported arguments")


def handle_search(args: Dict[str, Any], **_: Any) -> str:
    _validate_args(args, {"category", "query", "provider_id", "limit"})
    category = _text(args.get("category"), 64).lower()
    if not category:
        raise CapabilityError("category is required")
    limit = args.get("limit", 3)
    if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 5:
        raise CapabilityError("limit must be an integer between 1 and 5")
    query = _text(args.get("query"), 160)
    provider_id = _text(args.get("provider_id"), 64)
    binding = _async_context.current_binding()
    if binding is not None:
        request = _async_context.VideoSearchRequest(
            category=category, query=query, provider_id=provider_id, limit=limit
        )
        result = _async_context.video_search(binding, request)
    else:
        result = _client.post_json(
            _client.VIDEO_SEARCH_PATH,
            {
                "category": category,
                "query": query,
                "provider_id": provider_id,
                "limit": limit,
                "context": _client.current_context(),
            },
        )
    return _json(_clean_search_result(result, limit))


def _clean_search_result(result: Dict[str, Any], limit: int) -> Dict[str, Any]:
    candidates = []
    raw_candidates = result.get("candidates")
    if not isinstance(raw_candidates, list):
        raise CapabilityError("Golem video search returned no candidate list")
    for item in raw_candidates[:limit]:
        if not isinstance(item, dict):
            continue
        candidate_id = _text(item.get("id"), 2048)
        if not candidate_id:
            continue
        candidates.append(_clean_candidate(item, candidate_id))
    failures = result.get("failures", [])
    return {
        "candidates": candidates,
        "failures": failures if isinstance(failures, list) else [],
        "expires_in": result.get("expires_in"),
    }


def _clean_candidate(item: Dict[str, Any], candidate_id: str = "") -> Dict[str, Any]:
    candidate_id = candidate_id or _text(item.get("id"), 2048)
    if not candidate_id:
        raise CapabilityError("Golem returned an invalid video candidate")
    duration = item.get("duration_seconds")
    if not isinstance(duration, (int, float)) or isinstance(duration, bool):
        duration = None
    return {
        "id": candidate_id,
        "provider_id": _text(item.get("provider_id"), 64),
        "title": _text(item.get("title"), 300),
        "duration_seconds": duration,
    }


def handle_select(args: Dict[str, Any], **_: Any) -> str:
    _validate_args(args, {"candidate_id"})
    candidate_id = _text(args.get("candidate_id"), 2048)
    if not candidate_id:
        raise CapabilityError("candidate_id is required")
    context = _client.current_context()
    started = _client.post_json(
        _client.VIDEO_SELECT_PATH, {"candidate_id": candidate_id, "context": context},
    )
    job_id = _text(started.get("job_id"), 2048)
    if not job_id:
        raise CapabilityError("Golem did not start a video preparation job")
    return _json(_wait_for_job(job_id, context))


def _wait_for_job(job_id: str, context: Dict[str, str]) -> Dict[str, Any]:
    deadline = time.monotonic() + _prepare_timeout()
    last_error = ""
    while time.monotonic() < deadline:
        try:
            result = _client.post_json(
                _client.VIDEO_STATUS_PATH, {"job_id": job_id, "context": context},
            )
        except RetryableCapabilityError as exc:
            last_error = str(exc)
            time.sleep(0.5)
            continue
        state = _text(result.get("state"), 32)
        if state == "completed":
            return _completed_job(result)
        if state == "failed":
            _raise_failed_job(result)
        if state != "pending":
            raise CapabilityError("Golem returned an invalid video job state")
        time.sleep(0.5)
    message = "Video preparation timed out"
    if last_error:
        message += f"; last capability error: {last_error}"
    raise RetryableCapabilityError(message)


def _completed_job(result: Dict[str, Any]) -> Dict[str, Any]:
    token = _text(result.get("effect_only_token"), 256)
    if result.get("staged") is not True or not token:
        raise CapabilityError("Golem completed the video job without staging a reply")
    return {
        "staged": True,
        "fallback": result.get("fallback") is True,
        "failure_reason": _text(result.get("failure_reason"), 1000),
        "fallback_url": _text(result.get("fallback_url"), 4096),
        "effect_only_token": token,
    }


def _raise_failed_job(result: Dict[str, Any]) -> None:
    reason = _text(result.get("failure_reason"), 1000) or "video preparation failed"
    fallback_url = _text(result.get("fallback_url"), 4096)
    if fallback_url:
        reason += f"; original link: {fallback_url}"
    raise CapabilityError(reason)


def _prepare_timeout() -> float:
    raw = os.getenv("GOLEM_VIDEO_PREPARE_TIMEOUT_SECONDS", "210").strip()
    try:
        value = float(raw)
    except ValueError as exc:
        raise CapabilityError("GOLEM_VIDEO_PREPARE_TIMEOUT_SECONDS is invalid") from exc
    if value <= 0 or value > 600:
        raise CapabilityError("GOLEM_VIDEO_PREPARE_TIMEOUT_SECONDS must be between 0 and 600")
    return value


def _enabled() -> bool:
    raw = os.getenv(VIDEO_ENABLED_ENV, "false").strip().lower()
    if raw in {"1", "true", "yes", "on"}:
        return True
    if raw in {"0", "false", "no", "off"}:
        return False
    raise CapabilityError(f"{VIDEO_ENABLED_ENV} must be true or false")


def register(ctx: Any, common: Dict[str, Any]) -> None:
    video_common = dict(common)
    base_check = video_common.get("check_fn")
    video_common["check_fn"] = lambda: _enabled() and (base_check() if base_check else True)
    tools = (
        ("golem_video_search", SEARCH_SCHEMA, handle_search, "🎬"),
        ("golem_video_select", SELECT_SCHEMA, handle_select, "📤"),
    )
    for name, schema, handler, emoji in tools:
        ctx.register_tool(name=name, schema=schema, handler=handler, emoji=emoji, **video_common)
