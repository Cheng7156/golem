"""Direct video delivery tool for Hermes background children."""

from __future__ import annotations

import json
import time
from typing import Any, Dict

from . import video_async_context as _context
from . import video_tools as _video
from .errors import CapabilityError, RetryableCapabilityError


ATTACH_SCHEMA = {
    "name": "golem_video_attach",
    "description": (
        "Attach one configured video candidate to the current Golem reply. In a "
        "background child or Golem-bound cron run this prepares the media and queues "
        "a durable WeChat video Outbox. Use only candidate_id returned by "
        "golem_video_search."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "candidate_id": {"type": "string", "minLength": 1, "maxLength": 2048}
        },
        "required": ["candidate_id"],
        "additionalProperties": False,
    },
}


def handle_attach(args: Dict[str, Any], **_: Any) -> str:
    candidate_id = _candidate_id(args)
    binding = _context.current_binding()
    if binding is None:
        return _video.handle_select(args)
    started = _context.video_send(binding, candidate_id, _context.invocation_id())
    job_id = _video._text(started.get("job_id"), 256)
    if not job_id or started.get("state") not in {
        "pending", "completed", "waiting_delivery", "delivered"
    }:
        raise CapabilityError("Golem did not start an async video job")
    return _wait_for_delivery(binding, job_id)


def _candidate_id(args: Dict[str, Any]) -> str:
    _video._validate_args(args, {"candidate_id"})
    candidate_id = _video._text(args.get("candidate_id"), 2048)
    if not candidate_id:
        raise CapabilityError("candidate_id is required")
    return candidate_id


def _wait_for_delivery(binding: Any, job_id: str) -> str:
    deadline = time.monotonic() + _video._prepare_timeout()
    last_error = ""
    while time.monotonic() < deadline:
        try:
            result = _context.video_status(binding, job_id)
        except RetryableCapabilityError as exc:
            last_error = str(exc)
            time.sleep(0.5)
            continue
        state = _video._text(result.get("state"), 32)
        if state in {"completed", "waiting_delivery", "delivered"}:
            return _queued_result(result)
        if state in {"failed", "ambiguous", "dead_letter"}:
            reason = _video._text(result.get("error"), 1000)
            raise CapabilityError(reason or f"Golem video delivery ended as {state}")
        if state != "pending":
            raise CapabilityError("Golem returned an invalid async video job state")
        time.sleep(0.5)
    message = "Async video preparation timed out"
    if last_error:
        message += "; last capability error: " + last_error
    raise RetryableCapabilityError(message)


def _queued_result(result: Dict[str, Any]) -> str:
    outbox_id = _video._text(result.get("outbox_id"), 256)
    sequence = result.get("sequence")
    count = result.get("direct_output_count")
    if result.get("queued") is not True or not outbox_id:
        raise CapabilityError("Golem did not queue the async video")
    if not isinstance(sequence, int) or isinstance(sequence, bool) or sequence < 1:
        raise CapabilityError("Golem returned an invalid async video sequence")
    if not isinstance(count, int) or isinstance(count, bool) or count < 1:
        raise CapabilityError("Golem returned an invalid async video count")
    return json.dumps(
        {
            "queued": True,
            "kind": "video",
            "outbox_id": outbox_id,
            "sequence": sequence,
        },
        ensure_ascii=False,
        separators=(",", ":"),
    )


def register(ctx: Any, common: Dict[str, Any]) -> None:
    video_common = dict(common)
    base_check = video_common.get("check_fn")
    video_common["check_fn"] = lambda: _video._enabled() and (
        base_check() if base_check else True
    )
    ctx.register_tool(
        name="golem_video_attach",
        schema=ATTACH_SCHEMA,
        handler=handle_attach,
        emoji="attach",
        **video_common,
    )
