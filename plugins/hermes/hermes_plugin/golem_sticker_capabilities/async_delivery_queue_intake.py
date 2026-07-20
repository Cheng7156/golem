"""Attach durable delivery identity to synthetic completion messages."""

from __future__ import annotations

from typing import Any

from . import async_delivery_queue_lifecycle as lifecycle
from .async_delivery_queue_state import (
    ADAPTER_ATTR,
    BINDING_ATTR,
    COMPLETION_EVENT_ATTR,
)


_PATCH_MARKER = "_golem_async_completion_intake_v1"


def install(
    adapter_class: Any, delivery_context: Any, completion_event_context: Any
) -> None:
    original = adapter_class.handle_message
    if getattr(original, _PATCH_MARKER, False):
        return

    async def wrapped(self: Any, event: Any) -> Any:
        binding = delivery_context.get()
        if binding is None:
            return await original(self, event)
        payload = completion_event_context.get()
        if not isinstance(payload, dict):
            raise RuntimeError("async completion source event is unavailable")
        if lifecycle.is_shutting_down(self):
            lifecycle.requeue_completion(payload)
            return None
        setattr(event, BINDING_ATTR, binding)
        setattr(event, ADAPTER_ATTR, self)
        setattr(event, COMPLETION_EVENT_ATTR, dict(payload))
        return await original(self, event)

    setattr(wrapped, _PATCH_MARKER, True)
    adapter_class.handle_message = wrapped
