"""Cron-specific handling for detached Hermes delegate_task calls."""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any, Callable

from .async_delivery_state import forget_registration, mark_cron
from .cron_delivery_state import current_cron_delivery


logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class DelegationCallbacks:
    parse: Callable[[Any], str | None]
    register: Callable[[Any, str, str], Any]


def finish_tool_result(
    result: Any,
    kwargs: dict[str, Any],
    callbacks: DelegationCallbacks,
) -> Any:
    if kwargs.get("tool_name") != "delegate_task":
        return result
    delegation_id = callbacks.parse(result)
    if not delegation_id:
        return result
    session_id = str(kwargs.get("session_id") or "")
    if current_cron_delivery.get() is not None:
        mark_cron(delegation_id, session_id)
        return result
    return callbacks.register(result, delegation_id, session_id)


def consume_internal_completion(state: Any, delegation_id: str) -> bool:
    if state.status != "cron":
        return False
    forget_registration(delegation_id)
    logger.info("dropping internal cron async completion %s", delegation_id)
    return True
