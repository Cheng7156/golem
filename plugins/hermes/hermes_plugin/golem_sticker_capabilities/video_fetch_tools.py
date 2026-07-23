"""Arbitrary public URL video discovery and durable delivery."""

from __future__ import annotations

import json
from typing import Any, Dict

from . import video_async_context as _context
from . import video_async_tools as _async_video
from . import video_tools as _video
from .errors import CapabilityError


FETCH_SCHEMA = {
    "name": "golem_video_fetch",
    "description": (
        "Inspect a user-supplied public HTTP(S) URL and send its video to the current "
        "WeChat conversation. In a normal addressed/private turn, call once with url to "
        "create a durable background job and then tell the user it was accepted; do not "
        "delegate this simple task. In a background child, direct video, text URL, and "
        "unambiguous JSON responses are queued automatically. Only ambiguous JSON requires "
        "one exact returned media_url selection. Never invent or alter a returned URL."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "url": {
                "type": "string",
                "description": "The exact public HTTP(S) URL supplied by the user.",
                "minLength": 1,
                "maxLength": 4096,
            },
            "media_url": {
                "type": "string",
                "description": "For a JSON response, one exact media URL returned by the inspection.",
                "minLength": 1,
                "maxLength": 4096,
            },
            "title": {"type": "string", "maxLength": 300},
        },
        "required": ["url"],
        "additionalProperties": False,
    },
}


def handle_fetch(args: Dict[str, Any], **_: Any) -> str:
    _video._validate_args(args, {"url", "media_url", "title"})
    source_url = _video._text(args.get("url"), 4096)
    media_url = _video._text(args.get("media_url"), 4096)
    title = _video._text(args.get("title"), 300)
    if not source_url:
        raise CapabilityError("url is required")
    binding = _context.current_binding()
    if binding is None:
        if media_url:
            raise CapabilityError("media_url selection requires a background delivery binding")
        return _enqueue_inline(source_url, title)
    if media_url:
        return _send(binding, media_url, title)
    inspection = _context.video_inspect(binding, source_url)
    kind = _video._text(inspection.get("kind"), 32)
    if kind == "video":
        direct_url = _video._text(inspection.get("final_url"), 4096)
        if not direct_url:
            raise CapabilityError("Golem did not return the final video URL")
        return _send(binding, direct_url, title)
    if kind == "url":
        candidates = _clean_candidates(inspection.get("candidates"))
        if not candidates:
            raise CapabilityError("Golem did not find a URL in the text response")
        return _send(binding, candidates[0]["url"], title)
    if kind != "json":
        raise CapabilityError("Golem returned an invalid URL inspection kind")
    document = inspection.get("document")
    if not isinstance(document, (dict, list)):
        raise CapabilityError("Golem returned an invalid JSON inspection document")
    candidates = _clean_candidates(inspection.get("candidates"))
    automatic = _automatic_candidate(candidates)
    if automatic is not None:
        return _send(binding, automatic["url"], title)
    return json.dumps(
        {
            "status": "needs_selection",
            "instruction": (
                "Choose the exact video URL from candidates/document, then call "
                "golem_video_fetch again with url and media_url."
            ),
            "source_url": source_url,
            "candidates": candidates,
            "document": document,
        },
        ensure_ascii=False,
        separators=(",", ":"),
    )


def _enqueue_inline(source_url: str, title: str) -> str:
    result = _context.inline_video_fetch(
        source_url, title, _context.invocation_id()
    )
    return json.dumps(
        {
            "accepted": True,
            "job_id": _video._text(result.get("job_id"), 256),
            "state": _video._text(result.get("state"), 32),
            "deduplicated": bool(result.get("deduplicated", False)),
            "user_message": "视频任务已持久化接单，完成后会直接发送到当前会话。",
        },
        ensure_ascii=False,
        separators=(",", ":"),
    )


def _automatic_candidate(candidates: list[Dict[str, Any]]) -> Dict[str, Any] | None:
    """Select only candidates whose ranking is deterministic and unambiguous."""
    if len(candidates) == 1:
        return candidates[0]
    if len(candidates) < 2:
        return None
    first, second = candidates[0], candidates[1]
    if first["score"] >= 45 and first["score"] - second["score"] >= 20:
        return first
    return None


def _send(binding: Any, media_url: str, title: str) -> str:
    started = _context.video_send_url(
        binding, media_url, title, _context.invocation_id()
    )
    job_id = _video._text(started.get("job_id"), 256)
    if not job_id or started.get("state") not in {
        "pending", "completed", "waiting_delivery", "delivered"
    }:
        raise CapabilityError("Golem did not start a URL video job")
    return _async_video._wait_for_delivery(binding, job_id)


def _clean_candidates(value: Any) -> list[Dict[str, Any]]:
    if not isinstance(value, list):
        return []
    result = []
    for item in value[:32]:
        if not isinstance(item, dict):
            continue
        url = _video._text(item.get("url"), 4096)
        if not url:
            continue
        score = item.get("score")
        if not isinstance(score, int) or isinstance(score, bool):
            score = 0
        result.append(
            {
                "path": _video._text(item.get("path"), 512),
                "url": url,
                "label": _video._text(item.get("label"), 128),
                "score": score,
            }
        )
    return result


def register(ctx: Any, common: Dict[str, Any]) -> None:
    video_common = dict(common)
    base_check = video_common.get("check_fn")
    video_common["check_fn"] = lambda: _video._enabled() and (
        base_check() if base_check else True
    )
    ctx.register_tool(
        name="golem_video_fetch",
        schema=FETCH_SCHEMA,
        handler=handle_fetch,
        emoji="download",
        **video_common,
    )
