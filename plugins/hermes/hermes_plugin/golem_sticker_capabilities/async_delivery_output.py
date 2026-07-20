"""Final-text delivery boundary for async completion turns."""

from __future__ import annotations

import asyncio
import inspect
import logging
from typing import Any

from .async_delivery_api import complete_direct, deliver_v2, direct_output_count
from .async_delivery_outputs import OutputBuilder, current_output_builder
from .async_delivery_state import (
    current_delivery,
    is_binding_ready,
    mark_consumed,
    mark_failed,
)
from .async_delivery_text import sanitize_text
from .errors import CapabilityError, RetryableCapabilityError


logger = logging.getLogger(__name__)
_PATCH_MARKER = "_golem_async_delivery_output_v1"
_RETRY_DELAY_SECONDS = 2.0


def install(runner_class: Any, adapter_class: Any) -> None:
    patch_message_handler(runner_class)
    patch_send(adapter_class)


def patch_message_handler(runner_class: Any) -> None:
    original = runner_class._handle_message
    if getattr(original, _PATCH_MARKER, False):
        return

    async def wrapped(self: Any, event: Any) -> Any:
        binding = current_delivery.get()
        if binding is not None and not is_binding_ready(binding):
            logger.info("dropping inactive async completion %s", binding.delegation_id)
            return None
        builder_token = None
        if binding is not None:
            builder_token = current_output_builder.set(OutputBuilder())
        try:
            response = await original(self, event)
        finally:
            builder = current_output_builder.get()
            if builder_token is not None:
                current_output_builder.reset(builder_token)
        if binding is None:
            return response
        if not is_binding_ready(binding):
            logger.info("discarding async result after session boundary %s", binding.delegation_id)
            return None
        try:
            direct_count = await asyncio.to_thread(direct_output_count, binding)
            if direct_count > 0:
                await complete_direct_until_terminal(binding)
                mark_consumed(binding)
                return None
        except CapabilityError as exc:
            mark_failed(binding.delegation_id, str(exc))
            raise
        if isinstance(response, str):
            builder.append_text(sanitize_text(response))
        if builder is None or not builder.has_outputs():
            raise CapabilityError("Hermes async completion produced no outputs")
        try:
            await deliver_until_terminal(binding, builder.outputs())
        except CapabilityError as exc:
            mark_failed(binding.delegation_id, str(exc))
            raise
        mark_consumed(binding)
        return None

    setattr(wrapped, _PATCH_MARKER, True)
    runner_class._handle_message = wrapped


def patch_send(adapter_class: Any) -> None:
    original = adapter_class.send
    if getattr(original, _PATCH_MARKER, False):
        return
    signature = inspect.signature(original)

    async def wrapped(
        self: Any, chat_id: str, content: str, *args: Any, **kwargs: Any,
    ) -> Any:
        bound = signature.bind(self, chat_id, content, *args, **kwargs)
        bound.apply_defaults()
        reply_to = bound.arguments["reply_to"]
        metadata = bound.arguments["metadata"]
        binding = current_delivery.get()
        if binding is None:
            return await original(
                self, chat_id, content, reply_to=reply_to, metadata=metadata
            )
        from gateway.platforms.base import SendResult
        if chat_id != binding.chat_id:
            return SendResult(success=False, error="async delivery chat binding mismatch")
        return SendResult(
            success=False,
            error="async completion Relay sends are disabled; final delivery is durable",
        )

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(wrapped, "__signature__", signature)
    adapter_class.send = wrapped


async def deliver_until_terminal(binding: Any, outputs: list[dict[str, Any]]) -> dict[str, Any]:
    while True:
        if not is_binding_ready(binding):
            raise CapabilityError("Hermes async delivery is no longer active")
        try:
            return await asyncio.to_thread(deliver_v2, binding, outputs)
        except RetryableCapabilityError as exc:
            if not is_binding_ready(binding):
                raise CapabilityError(
                    "Hermes async delivery stopped at a session boundary"
                ) from exc
            logger.exception(
                "async durable commit unavailable for %s; retrying same ticket",
                binding.delegation_id,
            )
            await asyncio.sleep(_RETRY_DELAY_SECONDS)


async def complete_direct_until_terminal(binding: Any) -> dict[str, Any]:
    while True:
        if not is_binding_ready(binding):
            raise CapabilityError("Hermes async delivery is no longer active")
        try:
            return await asyncio.to_thread(complete_direct, binding)
        except RetryableCapabilityError as exc:
            if not is_binding_ready(binding):
                raise CapabilityError(
                    "Hermes async delivery stopped at a session boundary"
                ) from exc
            logger.exception(
                "async direct completion unavailable for %s; retrying same ticket",
                binding.delegation_id,
            )
            await asyncio.sleep(_RETRY_DELAY_SECONDS)
