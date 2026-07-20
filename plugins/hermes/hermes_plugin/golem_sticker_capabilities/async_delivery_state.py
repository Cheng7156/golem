"""Process-local state for Hermes background delegation delivery."""

from __future__ import annotations

from contextvars import ContextVar
from dataclasses import dataclass
import threading
from typing import Any, Dict, Optional


@dataclass(frozen=True)
class DeliveryBinding:
    ticket: str
    delegation_id: str
    producer_epoch: str
    hermes_session_id: str
    relay_session_key: str
    chat_id: str
    profile: str

    def request_fields(self) -> Dict[str, str]:
        return {
            "ticket": self.ticket,
            "delegation_id": self.delegation_id,
            "producer_epoch": self.producer_epoch,
            "hermes_session_id": self.hermes_session_id,
            "relay_session_key": self.relay_session_key,
            "chat_id": self.chat_id,
            "profile": self.profile,
        }


@dataclass(frozen=True)
class RegistrationState:
    status: str
    binding: Optional[DeliveryBinding] = None
    error: str = ""
    hermes_session_id: str = ""


_condition = threading.Condition()
_registrations: Dict[str, RegistrationState] = {}
current_delivery: ContextVar[Optional[DeliveryBinding]] = ContextVar(
    "golem_async_delivery", default=None
)
current_completion_event: ContextVar[Optional[Dict[str, Any]]] = ContextVar(
    "golem_async_completion_event", default=None
)
current_child_delegation_id: ContextVar[Optional[str]] = ContextVar(
    "golem_async_child_delegation_id", default=None
)
current_tool_invocation_id: ContextVar[Optional[str]] = ContextVar(
    "golem_tool_invocation_id", default=None
)


def mark_ready(binding: DeliveryBinding) -> bool:
    with _condition:
        current = _registrations.get(binding.delegation_id)
        if (
            current is not None
            and current.status == "inactive"
            and current.binding is None
        ):
            _registrations[binding.delegation_id] = RegistrationState(
                status="inactive",
                binding=binding,
                error="session boundary",
                hermes_session_id=binding.hermes_session_id,
            )
            _condition.notify_all()
            return False
        _registrations[binding.delegation_id] = RegistrationState(
            status="ready",
            binding=binding,
            hermes_session_id=binding.hermes_session_id,
        )
        _condition.notify_all()
        return True


def mark_registering(delegation_id: str, hermes_session_id: str) -> None:
    with _condition:
        _registrations[delegation_id] = RegistrationState(
            status="registering", hermes_session_id=hermes_session_id
        )
        _condition.notify_all()


def mark_failed(delegation_id: str, error: str) -> None:
    with _condition:
        _registrations[delegation_id] = RegistrationState(
            status="failed", error=error
        )
        _condition.notify_all()


def mark_cron(delegation_id: str, hermes_session_id: str) -> None:
    with _condition:
        _registrations[delegation_id] = RegistrationState(
            status="cron", hermes_session_id=hermes_session_id
        )
        _condition.notify_all()


def forget_registration(delegation_id: str) -> None:
    with _condition:
        _registrations.pop(delegation_id, None)
        _condition.notify_all()


def wait_registration(
    delegation_id: str, timeout_seconds: float
) -> Optional[RegistrationState]:
    def completed() -> bool:
        value = _registrations.get(delegation_id)
        return value is not None and value.status in {
            "ready", "failed", "inactive", "consumed", "cron",
        }

    with _condition:
        if not completed():
            _condition.wait_for(completed, timeout=timeout_seconds)
        return _registrations.get(delegation_id)


def bindings_for_session(hermes_session_id: str) -> list[DeliveryBinding]:
    with _condition:
        return [
            value.binding
            for value in _registrations.values()
            if value.status == "ready"
            and value.binding is not None
            and value.binding.hermes_session_id == hermes_session_id
        ]


def mark_session_inactive(hermes_session_id: str) -> None:
    with _condition:
        for delegation_id, value in list(_registrations.items()):
            binding = value.binding
            registered_session_id = (
                binding.hermes_session_id if binding else value.hermes_session_id
            )
            if registered_session_id != hermes_session_id:
                continue
            _registrations[delegation_id] = RegistrationState(
                status="inactive",
                binding=binding,
                error="session boundary",
                hermes_session_id=hermes_session_id,
            )
        _condition.notify_all()


def is_binding_ready(binding: DeliveryBinding) -> bool:
    with _condition:
        value = _registrations.get(binding.delegation_id)
        return bool(
            value is not None
            and value.status == "ready"
            and value.binding == binding
        )


def mark_consumed(binding: DeliveryBinding) -> None:
    with _condition:
        value = _registrations.get(binding.delegation_id)
        if value is None or value.status != "ready" or value.binding != binding:
            return
        _registrations[binding.delegation_id] = RegistrationState(
            status="consumed", binding=binding
        )
        _condition.notify_all()
