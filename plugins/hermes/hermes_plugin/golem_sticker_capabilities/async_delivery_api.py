"""HTTP protocol for Golem's durable async delivery capability."""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
import secrets
import time
from typing import Any, Dict

from . import capability_client as client
from .async_delivery_state import DeliveryBinding
from .errors import CapabilityError, RetryableCapabilityError


PRODUCER_EPOCH = "ade_" + secrets.token_hex(16)
IDEMPOTENT_ATTEMPTS = 3
RETRY_DELAY_SECONDS = 1.0
MIN_DIRECT_OUTPUT_COUNT = 1


@dataclass(frozen=True)
class RegistrationInput:
    delegation_id: str
    hermes_session_id: str
    context: Dict[str, str]


def register_delivery(value: RegistrationInput) -> DeliveryBinding:
    ticket = "adt_" + secrets.token_urlsafe(32)
    context = dict(value.context)
    context["profile"] = _context_profile(context)
    payload: Dict[str, Any] = {
        "ticket": ticket,
        "delegation_id": value.delegation_id,
        "producer_epoch": PRODUCER_EPOCH,
        "hermes_session_id": value.hermes_session_id,
        "context": context,
    }
    result = _post_idempotent(client.ASYNC_REGISTER_PATH, payload)
    if result.get("state") != "pending":
        raise CapabilityError("Golem did not create a pending async delivery")
    return DeliveryBinding(
        ticket=ticket,
        delegation_id=value.delegation_id,
        producer_epoch=PRODUCER_EPOCH,
        hermes_session_id=value.hermes_session_id,
        relay_session_key=context["session_key"],
        chat_id=context["chat_id"],
        profile=context["profile"],
    )


def delivery_status(binding: DeliveryBinding) -> str:
    result = _post_idempotent(client.ASYNC_STATUS_PATH, binding.request_fields())
    state = result.get("state")
    if state not in {"pending", "consumed", "revoked", "abandoned"}:
        raise CapabilityError("Golem returned an invalid async delivery state")
    return str(state)


def direct_output_count(binding: DeliveryBinding) -> int:
    result = _post_idempotent(client.ASYNC_STATUS_PATH, binding.request_fields())
    count = result.get("direct_output_count")
    if not isinstance(count, int) or isinstance(count, bool) or count < 0:
        raise CapabilityError("Golem returned an invalid direct output count")
    return count


def deliver(binding: DeliveryBinding, content: str) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["content"] = content
    result = _post_idempotent(client.ASYNC_DELIVER_PATH, payload)
    disposition = result.get("Disposition", result.get("disposition"))
    if disposition not in {"delivered", "silent", "discarded"}:
        raise CapabilityError("Golem returned an invalid async delivery receipt")
    return result


def deliver_v2(binding: DeliveryBinding, outputs: list[Dict[str, Any]]) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["outputs"] = outputs
    result = _post_idempotent_large(client.ASYNC_DELIVER_V2_PATH, payload)
    disposition = result.get("Disposition", result.get("disposition"))
    if disposition not in {"delivered", "silent", "discarded"}:
        raise CapabilityError("Golem returned an invalid async delivery receipt")
    return result


def complete_direct(binding: DeliveryBinding) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["outputs"] = []
    payload["silent"] = True
    result = _post_idempotent(client.ASYNC_DELIVER_V2_PATH, payload)
    disposition = result.get("Disposition", result.get("disposition"))
    if disposition not in {"silent", "discarded"}:
        raise CapabilityError("Golem did not close direct async delivery silently")
    return result


def sticker_search(binding: DeliveryBinding, query: str, limit: int) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["query"] = query
    payload["limit"] = limit
    return _post_idempotent(client.ASYNC_STICKER_SEARCH_PATH, payload)


def sticker_select(binding: DeliveryBinding, candidate_id: str) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["candidate_id"] = candidate_id
    return _post_idempotent_large(client.ASYNC_STICKER_SELECT_PATH, payload)


def sticker_send(
    binding: DeliveryBinding, candidate_id: str, invocation_id: str
) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["candidate_id"] = candidate_id
    payload["invocation_id"] = invocation_id
    result = _post_idempotent(client.ASYNC_STICKER_SEND_PATH, payload)
    if result.get("queued") is not True:
        raise CapabilityError("Golem did not queue the async sticker")
    if not isinstance(result.get("outbox_id"), str) or not result["outbox_id"]:
        raise CapabilityError("Golem returned an invalid async sticker receipt")
    sequence = result.get("sequence")
    count = result.get("direct_output_count")
    if not isinstance(sequence, int) or isinstance(sequence, bool) or sequence < 1:
        raise CapabilityError("Golem returned an invalid async sticker sequence")
    if not isinstance(count, int) or isinstance(count, bool) or count < MIN_DIRECT_OUTPUT_COUNT:
        raise CapabilityError("Golem returned an invalid async sticker count")
    return result


def video_search(
    binding: DeliveryBinding,
    category: str,
    query: str,
    provider_id: str,
    limit: int,
) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload.update(
        {
            "category": category,
            "query": query,
            "provider_id": provider_id,
            "limit": limit,
        }
    )
    return _post_idempotent(client.ASYNC_VIDEO_SEARCH_PATH, payload)


def video_send(
    binding: DeliveryBinding, candidate_id: str, invocation_id: str
) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["candidate_id"] = candidate_id
    payload["invocation_id"] = invocation_id
    return _post_idempotent(client.ASYNC_VIDEO_SEND_PATH, payload)


def video_status(binding: DeliveryBinding, job_id: str) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["job_id"] = job_id
    return _post_idempotent(client.ASYNC_VIDEO_STATUS_PATH, payload)


def revoke_session(hermes_session_id: str, profile: str) -> int:
    result = _post_idempotent(
        client.ASYNC_REVOKE_PATH,
        {
            "producer_epoch": PRODUCER_EPOCH,
            "hermes_session_id": hermes_session_id,
            "profile": profile,
        },
    )
    revoked = result.get("revoked")
    if not isinstance(revoked, int) or isinstance(revoked, bool) or revoked < 0:
        raise CapabilityError("Golem returned an invalid revocation count")
    return revoked


def reconcile(profile: str) -> int:
    result = _post_idempotent(
        client.ASYNC_RECONCILE_PATH,
        {"producer_epoch": PRODUCER_EPOCH, "profile": profile},
    )
    abandoned = result.get("abandoned")
    if not isinstance(abandoned, int) or isinstance(abandoned, bool) or abandoned < 0:
        raise CapabilityError("Golem returned an invalid reconciliation count")
    return abandoned


def active_profile() -> str:
    from hermes_cli.profiles import get_active_profile_name

    profile = get_active_profile_name()
    if not isinstance(profile, str) or not profile.strip():
        raise CapabilityError("Hermes active profile is unavailable")
    return profile.strip()


def delivery_profiles() -> tuple[str, ...]:
    from hermes_cli.config import load_config
    from hermes_cli.profiles import profiles_to_serve

    config = load_config()
    gateway = config.get("gateway", {})
    if not isinstance(gateway, dict):
        raise CapabilityError("Hermes gateway configuration is invalid")
    if not gateway.get("multiplex_profiles", False):
        return (active_profile(),)
    profiles = tuple(str(name).strip() for name, _ in profiles_to_serve(multiplex=True))
    if not profiles or any(not profile for profile in profiles):
        raise CapabilityError("Hermes multiplex profile list is invalid")
    return profiles


def _context_profile(context: Dict[str, str]) -> str:
    profile = context.get("profile", "")
    if not isinstance(profile, str):
        raise CapabilityError("Hermes Relay profile is invalid")
    return profile.strip() or active_profile()


def _post_idempotent(path: str, payload: Dict[str, Any]) -> Dict[str, Any]:
    return _post_idempotent_with_limit(path, payload, client.MAX_RESPONSE_BYTES)


def _post_idempotent_large(path: str, payload: Dict[str, Any]) -> Dict[str, Any]:
    return _post_idempotent_with_limit(
        path, payload, client.MAX_DELIVERY_RESPONSE_BYTES
    )


def _post_idempotent_with_limit(
    path: str, payload: Dict[str, Any], maximum: int
) -> Dict[str, Any]:
    last_error: CapabilityError | None = None
    for attempt in range(IDEMPOTENT_ATTEMPTS):
        try:
            return client.post_json_limited(path, payload, maximum)
        except RetryableCapabilityError as exc:
            last_error = exc
            if attempt + 1 < IDEMPOTENT_ATTEMPTS:
                time.sleep(RETRY_DELAY_SECONDS)
    assert last_error is not None
    raise last_error


def enabled() -> bool:
    raw = os.getenv("GOLEM_ASYNC_DELIVERY_ENABLED", "false").strip().lower()
    if raw in {"1", "true", "yes", "on"}:
        return True
    if raw in {"0", "false", "no", "off"}:
        return False
    raise CapabilityError("GOLEM_ASYNC_DELIVERY_ENABLED must be true or false")


def parse_delegate_result(raw: Any) -> str | None:
    if not isinstance(raw, str):
        return None
    try:
        value = json.loads(raw)
    except json.JSONDecodeError:
        return None
    if not isinstance(value, dict) or value.get("status") != "dispatched":
        return None
    delegation_id = value.get("delegation_id")
    if not isinstance(delegation_id, str) or not delegation_id.startswith("deleg_"):
        return None
    return delegation_id


def registration_failure(delegation_id: str, error: Exception) -> str:
    return json.dumps(
        {
            "status": "delivery_registration_failed",
            "delegation_id": delegation_id,
            "error": f"Golem async delivery registration failed: {error}",
            "note": "The child result will not be delivered to WeChat.",
        },
        ensure_ascii=False,
        separators=(",", ":"),
    )
