"""Trusted async-delivery context used by video tools."""

from __future__ import annotations

from typing import Any

from . import async_delivery_state as _state
from . import async_delivery_api as _async_api
from . import capability_client as _client
from . import cron_delivery_api as _cron_api
from .cron_delivery_state import CronDeliveryBinding, current_cron_delivery
from .errors import CapabilityError
from .video_delivery_types import VideoSearchRequest


MAX_INVOCATION_ID_LENGTH = 256


def current_binding() -> Any:
    binding = _state.current_delivery.get()
    if binding is not None:
        return binding
    delegation_id = _state.current_child_delegation_id.get()
    cron_child = False
    if delegation_id:
        registration = _state.wait_registration(
            delegation_id, _client._timeout_seconds()
        )
        if registration is not None and registration.status == "ready":
            if registration.binding is None:
                raise CapabilityError("Golem async delivery binding is unavailable")
            return registration.binding
        if registration is None or registration.status != "cron":
            raise CapabilityError("Golem async delivery binding is unavailable")
        cron_child = True
    binding = current_cron_delivery.get()
    if cron_child and binding is None:
        raise CapabilityError("Golem cron delivery binding is unavailable")
    return binding


def video_search(
    binding: Any,
    request: VideoSearchRequest,
) -> dict[str, Any]:
    if isinstance(binding, CronDeliveryBinding):
        return _cron_api.video_search(binding, request)
    return _async_api.video_search(
        binding, request.category, request.query, request.provider_id, request.limit
    )


def video_send(binding: Any, candidate_id: str, invocation: str) -> dict[str, Any]:
    if isinstance(binding, CronDeliveryBinding):
        return _cron_api.video_send(binding, candidate_id, invocation)
    return _async_api.video_send(binding, candidate_id, invocation)


def video_status(binding: Any, job_id: str) -> dict[str, Any]:
    if isinstance(binding, CronDeliveryBinding):
        return _cron_api.video_status(binding, job_id)
    return _async_api.video_status(binding, job_id)


def invocation_id() -> str:
    raw = _state.current_tool_invocation_id.get()
    if not isinstance(raw, str):
        raise CapabilityError("Hermes tool invocation_id is unavailable")
    value = raw.strip()
    if not value or len(value) > MAX_INVOCATION_ID_LENGTH:
        raise CapabilityError("Hermes tool invocation_id is invalid")
    return value
