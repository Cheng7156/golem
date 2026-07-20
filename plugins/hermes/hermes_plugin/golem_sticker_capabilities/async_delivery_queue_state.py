"""Adapter-local queues used by durable async completion ordering."""

from __future__ import annotations

from collections import deque
from typing import Any, Deque, Dict


BINDING_ATTR = "_golem_async_delivery_binding_v1"
ADAPTER_ATTR = "_golem_async_delivery_adapter_v1"
COMPLETIONS_ATTR = "_golem_async_completion_queues_v1"
DEFERRED_USERS_ATTR = "_golem_async_deferred_users_v1"
ACTIVE_ATTR = "_golem_async_active_sessions_v1"
ACTIVE_EVENTS_ATTR = "_golem_async_active_completion_events_v1"
COMPLETION_EVENT_ATTR = "_golem_async_completion_event_v1"
SHUTTING_DOWN_ATTR = "_golem_async_completion_shutdown_active_v1"


def enqueue(
    adapter: Any, attribute: str, session_key: str, *, event: Any
) -> None:
    queue_map(adapter, attribute).setdefault(session_key, deque()).append(event)


def queue_map(adapter: Any, attribute: str) -> Dict[str, Deque[Any]]:
    value = getattr(adapter, attribute, None)
    if value is None:
        value = {}
        setattr(adapter, attribute, value)
    return value


def active_bindings(adapter: Any) -> Dict[str, Any]:
    value = getattr(adapter, ACTIVE_ATTR, None)
    if value is None:
        value = {}
        setattr(adapter, ACTIVE_ATTR, value)
    return value


def active_events(adapter: Any) -> Dict[str, Dict[str, Any]]:
    value = getattr(adapter, ACTIVE_EVENTS_ATTR, None)
    if value is None:
        value = {}
        setattr(adapter, ACTIVE_EVENTS_ATTR, value)
    return value


def snapshot_completion_events(adapter: Any) -> list[Dict[str, Any]]:
    payloads = list(active_events(adapter).values())
    queues = getattr(adapter, COMPLETIONS_ATTR, {})
    for queue in queues.values():
        for event in queue:
            payload = getattr(event, COMPLETION_EVENT_ATTR, None)
            if not isinstance(payload, dict):
                raise RuntimeError("queued async completion has no source event")
            payloads.append(payload)
    return [dict(payload) for payload in payloads]


def clear_adapter(adapter: Any) -> None:
    attributes = (
        COMPLETIONS_ATTR, DEFERRED_USERS_ATTR, ACTIVE_ATTR, ACTIVE_EVENTS_ATTR,
    )
    for attribute in attributes:
        value = getattr(adapter, attribute, None)
        if value is not None:
            value.clear()
