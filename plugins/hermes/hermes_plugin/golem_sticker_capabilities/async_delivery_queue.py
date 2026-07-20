"""Busy-session queue integration for durable async completions."""

from __future__ import annotations

from collections import deque
import inspect
import logging
from typing import Any, Callable

from . import async_delivery_queue_intake
from . import async_delivery_queue_lifecycle
from .async_delivery_queue_state import (
    ADAPTER_ATTR as _ADAPTER_ATTR,
    BINDING_ATTR as _BINDING_ATTR,
    COMPLETION_EVENT_ATTR as _COMPLETION_EVENT_ATTR,
    COMPLETIONS_ATTR as _COMPLETIONS_ATTR,
    DEFERRED_USERS_ATTR as _DEFERRED_USERS_ATTR,
    active_bindings as _active_bindings,
    active_events as _active_events,
    enqueue as _enqueue,
    queue_map as _queue_map,
)


logger = logging.getLogger(__name__)
_PATCH_MARKER = "_golem_async_completion_queue_v1"


def install(
    adapter_class: Any,
    runner_class: Any,
    *,
    delivery_context: Any,
    completion_event_context: Any,
    binding_is_ready: Callable[[Any], bool],
) -> None:
    async_delivery_queue_intake.install(
        adapter_class, delivery_context, completion_event_context
    )
    _patch_start_processing(adapter_class)
    _patch_background_processing(adapter_class, delivery_context)
    _patch_busy_handler(runner_class)
    _patch_session_cleanup(adapter_class, delivery_context, binding_is_ready)
    async_delivery_queue_lifecycle.install(adapter_class)


def validate_signatures(
    adapter_class: Any,
    runner_class: Any,
    validate: Callable[..., None],
) -> None:
    validate(runner_class._adapter_for_source, "self", "source")
    validate(
        runner_class._handle_active_session_busy_message,
        "self", "event", "session_key",
    )
    validate(adapter_class.handle_message, "self", "event")
    validate(adapter_class._start_session_processing, "self", "event", "session_key")
    validate(
        adapter_class._process_message_background,
        "self", "event", "session_key",
    )
    validate(
        adapter_class._cleanup_finished_session_task,
        "self", "session_key", "interrupt_event",
    )
    validate(adapter_class.cancel_background_tasks, "self")


def discard_session(gateway: Any, source: Any, session_key: str) -> None:
    adapter = gateway._adapter_for_source(source)
    if adapter is None:
        logger.error("cannot discard async completion queue: adapter unavailable")
        return
    _queue_map(adapter, _COMPLETIONS_ATTR).pop(session_key, None)
    _queue_map(adapter, _DEFERRED_USERS_ATTR).pop(session_key, None)
    _active_bindings(adapter).pop(session_key, None)
    _active_events(adapter).pop(session_key, None)


def _patch_start_processing(adapter_class: Any) -> None:
    original = adapter_class._start_session_processing
    if getattr(original, _PATCH_MARKER, False):
        return
    signature = inspect.signature(original)

    def wrapped(self: Any, event: Any, session_key: str, *args: Any, **kwargs: Any) -> bool:
        binding = getattr(event, _BINDING_ATTR, None)
        if binding is not None:
            _active_bindings(self)[session_key] = binding
            _active_events(self)[session_key] = getattr(
                event, _COMPLETION_EVENT_ATTR
            )
        try:
            started = original(self, event, session_key, *args, **kwargs)
        except Exception:
            if _active_bindings(self).get(session_key) is binding:
                _active_bindings(self).pop(session_key, None)
                _active_events(self).pop(session_key, None)
            raise
        if not started and _active_bindings(self).get(session_key) is binding:
            _active_bindings(self).pop(session_key, None)
            _active_events(self).pop(session_key, None)
        return started

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(wrapped, "__signature__", signature)
    adapter_class._start_session_processing = wrapped


def _patch_background_processing(adapter_class: Any, delivery_context: Any) -> None:
    original = adapter_class._process_message_background
    if getattr(original, _PATCH_MARKER, False):
        return

    async def wrapped(self: Any, event: Any, session_key: str) -> Any:
        binding = getattr(event, _BINDING_ATTR, None)
        if binding is None:
            return await original(self, event, session_key)
        token = delivery_context.set(binding)
        try:
            return await original(self, event, session_key)
        finally:
            delivery_context.reset(token)
            active = _active_bindings(self)
            if active.get(session_key) == binding:
                active.pop(session_key, None)
                _active_events(self).pop(session_key, None)

    setattr(wrapped, _PATCH_MARKER, True)
    adapter_class._process_message_background = wrapped


def _patch_busy_handler(runner_class: Any) -> None:
    original = runner_class._handle_active_session_busy_message
    if getattr(original, _PATCH_MARKER, False):
        return

    async def wrapped(self: Any, event: Any, session_key: str) -> bool:
        binding = getattr(event, _BINDING_ATTR, None)
        if binding is not None:
            adapter = getattr(event, _ADAPTER_ATTR, None)
            if adapter is None:
                logger.error("cannot queue async completion: adapter binding unavailable")
                return True
            _enqueue(adapter, _COMPLETIONS_ATTR, session_key, event=event)
            return True
        adapter = self._adapter_for_source(event.source)
        if adapter is None or not _must_defer_user(adapter, session_key):
            return await original(self, event, session_key)
        if not self._is_user_authorized(event.source):
            logger.warning("dropping unauthorized message during async completion")
            return True
        queue = _queue_map(adapter, _DEFERRED_USERS_ATTR).setdefault(
            session_key, deque()
        )
        if len(queue) >= self._BUSY_QUEUE_MAX_PENDING:
            logger.warning("dropping deferred message: Hermes busy queue is full")
            return True
        queue.append(event)
        return True

    setattr(wrapped, _PATCH_MARKER, True)
    runner_class._handle_active_session_busy_message = wrapped


def _patch_session_cleanup(
    adapter_class: Any,
    delivery_context: Any,
    binding_is_ready: Callable[[Any], bool],
) -> None:
    original = adapter_class._cleanup_finished_session_task
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(self: Any, session_key: str, interrupt_event: Any) -> None:
        original(self, session_key, interrupt_event)
        if async_delivery_queue_lifecycle.is_shutting_down(self):
            return
        if session_key in self._active_sessions:
            return
        _active_bindings(self).pop(session_key, None)
        _start_next(
            self,
            session_key,
            delivery_context=delivery_context,
            binding_is_ready=binding_is_ready,
        )

    setattr(wrapped, _PATCH_MARKER, True)
    adapter_class._cleanup_finished_session_task = wrapped


def _start_next(
    adapter: Any,
    session_key: str,
    *,
    delivery_context: Any,
    binding_is_ready: Callable[[Any], bool],
) -> None:
    if _start_deferred_user(adapter, session_key, delivery_context):
        return
    _start_completion(
        adapter,
        session_key,
        delivery_context=delivery_context,
        binding_is_ready=binding_is_ready,
    )


def _start_deferred_user(
    adapter: Any, session_key: str, delivery_context: Any
) -> bool:
    queues = _queue_map(adapter, _DEFERRED_USERS_ATTR)
    queue = queues.get(session_key)
    if not queue:
        return False
    event = queue.popleft()
    try:
        _start_event(
            adapter,
            event,
            session_key,
            delivery_context=delivery_context,
            binding=None,
        )
    except Exception:
        queue.appendleft(event)
        raise
    if not queue:
        queues.pop(session_key, None)
    return True


def _start_completion(
    adapter: Any,
    session_key: str,
    *,
    delivery_context: Any,
    binding_is_ready: Callable[[Any], bool],
) -> None:
    queues = _queue_map(adapter, _COMPLETIONS_ATTR)
    queue = queues.get(session_key)
    while queue:
        event = queue.popleft()
        binding = getattr(event, _BINDING_ATTR)
        if not binding_is_ready(binding):
            logger.info("dropping inactive queued completion %s", binding.delegation_id)
            continue
        try:
            _start_event(
                adapter,
                event,
                session_key,
                delivery_context=delivery_context,
                binding=binding,
            )
        except Exception:
            queue.appendleft(event)
            raise
        break
    if not queue:
        queues.pop(session_key, None)


def _start_event(
    adapter: Any,
    event: Any,
    session_key: str,
    *,
    delivery_context: Any,
    binding: Any,
) -> None:
    token = delivery_context.set(binding)
    try:
        started = adapter._start_session_processing(event, session_key)
    finally:
        delivery_context.reset(token)
    if not started:
        raise RuntimeError("Hermes failed to start queued session event")


def _must_defer_user(adapter: Any, session_key: str) -> bool:
    if session_key in _active_bindings(adapter):
        return True
    queue = _queue_map(adapter, _DEFERRED_USERS_ATTR).get(session_key)
    return bool(queue)
