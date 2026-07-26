"""Version-locked Hermes v0.18.2 bridge for background delegation."""

from __future__ import annotations

import asyncio
import dataclasses
import importlib.metadata
import inspect
import logging
from typing import Any, Dict

from . import capability_client as client
from . import async_delivery_child_context
from . import async_delivery_lifecycle
from . import async_delivery_queue
from . import async_delivery_output
from . import cron_async_delegation
from .async_delivery_api import (
    RegistrationInput,
    activate_gateway_producer,
    delivery_profiles,
    delivery_status,
    parse_delegate_result,
    reconcile,
    register_delivery,
    registration_failure,
    revoke_session,
)
from .async_delivery_state import (
    current_completion_event,
    current_delivery,
    current_tool_invocation_id,
    bindings_for_session,
    is_binding_ready,
    mark_failed,
    mark_ready,
    mark_registering,
    mark_session_inactive,
    wait_registration,
)
from .errors import CapabilityError, RetryableCapabilityError

SUPPORTED_VERSION = "0.18.2"
_PATCH_MARKER = "_golem_async_delivery_v1"
_BLOCKED_ASYNC_TOOLS = {
    "delegate_task",
    "golem_sticker_collect_current_session",
    "golem_sticker_inspect",
    "golem_sticker_library_inventory",
    "golem_sticker_library_search",
    "golem_sticker_select",
    "golem_video_fetch",
}
_AMBIENT_MEDIA_PREFIXES = ("golem_sticker_", "golem_video_")
logger = logging.getLogger(__name__)
_REQUEUE_DELAY_SECONDS = 2.0


def install() -> None:
    _validate_version()
    from gateway.platforms.base import BasePlatformAdapter
    from gateway.relay.adapter import RelayAdapter
    from gateway.run import GatewayRunner

    _validate_signature(GatewayRunner._inject_watch_notification, "self", "synth_text", "evt")
    _validate_signature(GatewayRunner._build_process_event_source, "self", "evt")
    _validate_signature(GatewayRunner._handle_message, "self", "event")
    _validate_signature(GatewayRunner._is_user_authorized, "self", "source")
    async_delivery_queue.validate_signatures(
        BasePlatformAdapter, GatewayRunner, _validate_signature
    )
    _validate_signature(RelayAdapter.send, "self", "chat_id", "content", "reply_to", "metadata")
    async_delivery_queue.install(
        BasePlatformAdapter,
        GatewayRunner,
        delivery_context=current_delivery,
        completion_event_context=current_completion_event,
        binding_is_ready=is_binding_ready,
    )
    _patch_injection(GatewayRunner)
    _patch_process_event_source(GatewayRunner)
    async_delivery_child_context.install()
    async_delivery_output.install(GatewayRunner, RelayAdapter)


def gateway_startup(gateway: Any = None, **_: Any) -> None:
    """Establish producer ownership only at the root Gateway boundary."""
    marker = "_golem_async_delivery_producer_epoch"
    producer_epoch = getattr(gateway, marker, "") if gateway is not None else ""
    if not producer_epoch:
        producer_epoch = activate_gateway_producer()
        if gateway is not None:
            setattr(gateway, marker, producer_epoch)
    else:
        # A repeated hook for the same Gateway instance must keep the same
        # identity, including for subsequently spawned child processes.
        import os
        os.environ["_HERMES_GOLEM_ASYNC_PRODUCER_EPOCH"] = producer_epoch
    for profile in delivery_profiles():
        abandoned = reconcile(profile)
        logger.info(
            "reconciled Golem async producer profile=%s abandoned=%s",
            profile,
            abandoned,
        )


def tool_execution(**kwargs: Any) -> Any:
    next_call = kwargs["next_call"]
    args = kwargs.get("args")
    invocation_id = str(kwargs.get("tool_call_id") or "").strip()
    invocation_token = current_tool_invocation_id.set(invocation_id or None)
    try:
        result = next_call(args)
    finally:
        current_tool_invocation_id.reset(invocation_token)
    return cron_async_delegation.finish_tool_result(result, kwargs, _DELEGATION_CALLBACKS)


def _register_result(result: Any, delegation_id: str, session_id: str) -> Any:
    mark_registering(delegation_id, session_id)
    try:
        context = client.current_context()
        if not session_id:
            raise CapabilityError("Hermes session id is unavailable")
        binding = register_delivery(
            RegistrationInput(
                delegation_id=delegation_id,
                hermes_session_id=session_id,
                context=context,
            )
        )
    except Exception as exc:
        mark_failed(delegation_id, str(exc))
        _interrupt_delegation(delegation_id)
        logger.exception("async delivery registration failed for %s", delegation_id)
        return registration_failure(delegation_id, exc)
    if not mark_ready(binding):
        async_delivery_lifecycle.schedule_revoke(
            binding.hermes_session_id, binding.profile, revoke_session
        )
        error = CapabilityError("Hermes session ended during delivery registration")
        logger.error("async delivery registration crossed session boundary for %s", delegation_id)
        return registration_failure(delegation_id, error)
    return result


_DELEGATION_CALLBACKS = cron_async_delegation.DelegationCallbacks(parse_delegate_result, _register_result)


def pre_tool_call(tool_name: str = "", **_: Any) -> Dict[str, str] | None:
    if current_delivery.get() is not None and tool_name in _BLOCKED_ASYNC_TOOLS:
        return {
            "action": "block",
            "message": f"Tool {tool_name} is unavailable in an async completion turn",
        }
    if _session_trigger_kind() == "ambient" and (
        tool_name == "delegate_task" or tool_name.startswith(_AMBIENT_MEDIA_PREFIXES)
    ):
        return {
            "action": "block",
            "message": f"Tool {tool_name} is unavailable in an unaddressed ambient turn",
        }
    return None


def _session_trigger_kind() -> str:
    """Read only Hermes' task-local, transport-bound trigger authority."""
    try:
        from gateway.session_context import get_session_trigger_kind

        return get_session_trigger_kind()
    except (ImportError, AttributeError):
        # Older/non-gateway Hermes surfaces have no trusted trigger binding.
        return ""


def session_reset(old_session_id: str = "", **kwargs: Any) -> None:
    session_id = old_session_id or str(kwargs.get("session_id") or "")
    if session_id:
        _deactivate_session(session_id)


def pre_gateway_dispatch(
    event: Any = None,
    gateway: Any = None,
    session_store: Any = None,
    **_: Any,
) -> None:
    resolved = _authorized_command_session(event, gateway, session_store)
    if resolved is None:
        return None
    source, entry = resolved
    session_id = str(getattr(entry, "session_id", "") or "")
    session_key = str(getattr(entry, "session_key", "") or "")
    if session_id:
        _deactivate_session(session_id)
    if session_key:
        _interrupt_session_delegations(session_key)
        async_delivery_queue.discard_session(gateway, source, session_key)
    return None


def _deactivate_session(session_id: str) -> None:
    profiles = {binding.profile for binding in bindings_for_session(session_id)}
    mark_session_inactive(session_id)
    for profile in sorted(profiles):
        async_delivery_lifecycle.schedule_revoke(
            session_id, profile, revoke_session
        )


def _authorized_command_session(
    event: Any, gateway: Any, session_store: Any
) -> tuple[Any, Any] | None:
    if event is None or event.get_command() not in {"stop", "new", "reset"}:
        return None
    source = getattr(event, "source", None)
    if source is None or gateway is None:
        return None
    if not gateway._is_user_authorized(source):
        return None
    return source, session_store.get_or_create_session(source)


def _patch_injection(runner_class: Any) -> None:
    original = runner_class._inject_watch_notification
    if getattr(original, _PATCH_MARKER, False):
        return

    async def wrapped(self: Any, synth_text: str, evt: Dict[str, Any]) -> Any:
        if evt.get("type") != "async_delegation":
            return await original(self, synth_text, evt)
        delegation_id = _completion_delegation_id(evt)
        if delegation_id is None:
            return True
        state = await asyncio.to_thread(
            wait_registration, delegation_id, client._timeout_seconds()
        )
        if state is None:
            logger.error("dropping orphan async completion %s", delegation_id)
            _discard_terminal_completion(delegation_id, "orphan completion")
            return True
        if state.status == "registering":
            await _requeue_completion(evt)
            logger.error("async completion registration timed out for %s", delegation_id)
            return
        if cron_async_delegation.consume_internal_completion(state, delegation_id):
            _discard_terminal_completion(
                delegation_id, "internal completion handled outside transcript"
            )
            return True
        if state.status != "ready" or state.binding is None:
            logger.error("dropping unregistered async completion %s", delegation_id)
            _discard_terminal_completion(delegation_id, "unregistered completion")
            return True
        try:
            ticket_state = await asyncio.to_thread(delivery_status, state.binding)
        except RetryableCapabilityError:
            await _requeue_completion(evt)
            logger.exception("async delivery status failed for %s", delegation_id)
            return
        except CapabilityError as exc:
            mark_failed(delegation_id, str(exc))
            logger.exception("dropping invalid async delivery %s", delegation_id)
            _discard_terminal_completion(
                delegation_id, f"invalid async delivery: {exc}"
            )
            return True
        if ticket_state != "pending":
            logger.info("dropping inactive async completion %s", delegation_id)
            _discard_terminal_completion(
                delegation_id, f"inactive async delivery ticket: {ticket_state}"
            )
            return True
        delivery_token = current_delivery.set(state.binding)
        event_token = current_completion_event.set(dict(evt))
        try:
            return await original(self, synth_text, evt)
        finally:
            current_completion_event.reset(event_token)
            current_delivery.reset(delivery_token)

    setattr(wrapped, _PATCH_MARKER, True)
    runner_class._inject_watch_notification = wrapped


def _discard_terminal_completion(delegation_id: str, reason: str) -> None:
    try:
        from tools.async_delegation import mark_completion_discarded

        mark_completion_discarded(delegation_id, reason)
    except Exception:
        logger.exception(
            "could not discard terminal async completion %s", delegation_id
        )


def _completion_delegation_id(evt: Dict[str, Any]) -> str | None:
    delegation_id = evt.get("delegation_id")
    if not isinstance(delegation_id, str) or not delegation_id.startswith("deleg_"):
        logger.error("dropping malformed async completion event")
        return None
    return delegation_id


def _patch_process_event_source(runner_class: Any) -> None:
    original = runner_class._build_process_event_source
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(self: Any, evt: Dict[str, Any]) -> Any:
        source = original(self, evt)
        binding = current_delivery.get()
        if source is None or binding is None:
            return source
        return dataclasses.replace(source, profile=binding.profile)

    setattr(wrapped, _PATCH_MARKER, True)
    runner_class._build_process_event_source = wrapped


def _interrupt_session_delegations(session_key: str) -> None:
    from tools import async_delegation

    interrupts = []
    with async_delegation._records_lock:
        for record in async_delegation._records.values():
            if record.get("session_key") != session_key or record.get("status") != "running":
                continue
            interrupt = record.get("interrupt_fn")
            if callable(interrupt):
                interrupts.append(interrupt)
    for interrupt in interrupts:
        interrupt()


def _interrupt_delegation(delegation_id: str) -> None:
    from tools import async_delegation

    with async_delegation._records_lock:
        record = async_delegation._records.get(delegation_id)
        interrupt = record.get("interrupt_fn") if record else None
    if callable(interrupt):
        interrupt()


async def _requeue_completion(evt: Dict[str, Any]) -> None:
    from tools.process_registry import process_registry
    await asyncio.sleep(_REQUEUE_DELAY_SECONDS)
    process_registry.completion_queue.put(dict(evt))


def _validate_version() -> None:
    version = importlib.metadata.version("hermes-agent")
    if version != SUPPORTED_VERSION:
        raise RuntimeError(
            f"Golem async delivery requires hermes-agent=={SUPPORTED_VERSION}; found {version}"
        )


def _validate_signature(function: Any, *names: str) -> None:
    actual = tuple(inspect.signature(function).parameters)
    if actual[: len(names)] != names:
        raise RuntimeError(f"unsupported Hermes method signature: {function.__qualname__}{actual}")
