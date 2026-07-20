"""Hermes adapter shutdown integration for async completion queues."""

from __future__ import annotations

from typing import Any

from .async_delivery_queue_state import (
    SHUTTING_DOWN_ATTR,
    clear_adapter,
    snapshot_completion_events,
)


_PATCH_MARKER = "_golem_async_completion_shutdown_v1"


def install(adapter_class: Any) -> None:
    original = adapter_class.cancel_background_tasks
    if getattr(original, _PATCH_MARKER, False):
        return

    async def wrapped(self: Any) -> None:
        setattr(self, SHUTTING_DOWN_ATTR, True)
        payloads = snapshot_completion_events(self)
        try:
            await original(self)
        finally:
            clear_adapter(self)
            setattr(self, SHUTTING_DOWN_ATTR, False)
            for payload in payloads:
                requeue_completion(payload)

    setattr(wrapped, _PATCH_MARKER, True)
    adapter_class.cancel_background_tasks = wrapped


def is_shutting_down(adapter: Any) -> bool:
    return bool(getattr(adapter, SHUTTING_DOWN_ATTR, False))


def requeue_completion(payload: dict[str, Any]) -> None:
    from tools.process_registry import process_registry

    process_registry.completion_queue.put(dict(payload))
