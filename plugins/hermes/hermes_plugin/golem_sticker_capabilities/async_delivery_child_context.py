"""Bind Hermes async delegation workers to Golem delivery registrations."""

from __future__ import annotations

import contextvars
import threading
from typing import Any

from .async_delivery_state import current_child_delegation_id


_PATCH_MARKER = "_golem_async_child_context_v1"
_EXECUTOR_PATCH_MARKER = "_golem_context_propagating_submit_v1"
_PATCH_LOCK = threading.Lock()


def install(async_delegation: Any = None) -> None:
    if async_delegation is None:
        _patch_daemon_executor()
        from tools import async_delegation as async_delegation
    _patch_dispatch_function(async_delegation, "dispatch_async_delegation")
    _patch_dispatch_function(async_delegation, "dispatch_async_delegation_batch")


def _patch_daemon_executor(executor_class: Any = None) -> None:
    if executor_class is None:
        from tools.daemon_pool import DaemonThreadPoolExecutor

        executor_class = DaemonThreadPoolExecutor
    original = executor_class.submit
    if getattr(original, _EXECUTOR_PATCH_MARKER, False):
        return

    def wrapped(self: Any, function: Any, *args: Any, **kwargs: Any):
        context = contextvars.copy_context()
        return original(self, context.run, function, *args, **kwargs)

    setattr(wrapped, _EXECUTOR_PATCH_MARKER, True)
    executor_class.submit = wrapped


def _patch_dispatch_function(async_delegation: Any, name: str) -> None:
    original = getattr(async_delegation, name)
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(*args: Any, **kwargs: Any) -> Any:
        runner = kwargs.get("runner")
        if not callable(runner):
            return original(*args, **kwargs)
        delegation_id: dict[str, str] = {}
        kwargs = dict(kwargs)
        kwargs["runner"] = _child_context_runner(runner, delegation_id)
        return _dispatch_with_recorded_id(
            async_delegation, original, delegation_id, args, kwargs
        )

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(async_delegation, name, wrapped)


def _child_context_runner(runner: Any, delegation_id: dict[str, str]) -> Any:
    def wrapped() -> Any:
        value = delegation_id.get("value", "")
        if not value:
            return runner()
        token = current_child_delegation_id.set(value)
        try:
            return runner()
        finally:
            current_child_delegation_id.reset(token)

    return wrapped


def _dispatch_with_recorded_id(
    async_delegation: Any,
    original: Any,
    delegation_id: dict[str, str],
    args: tuple[Any, ...],
    kwargs: dict[str, Any],
) -> Any:
    with _PATCH_LOCK:
        original_new_id = async_delegation._new_delegation_id
        async_delegation._new_delegation_id = _recording_new_id(
            original_new_id, delegation_id
        )
        try:
            return original(*args, **kwargs)
        finally:
            async_delegation._new_delegation_id = original_new_id


def _recording_new_id(original_new_id: Any, delegation_id: dict[str, str]) -> Any:
    def wrapped() -> str:
        value = original_new_id()
        delegation_id["value"] = value
        return value

    return wrapped
