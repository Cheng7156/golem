"""Script-owned structured outputs for async completion delivery."""

from __future__ import annotations

from contextvars import ContextVar
from typing import Any, Dict, List, Optional

from .errors import CapabilityError


Output = Dict[str, Any]


class OutputBuilder:
    def __init__(self) -> None:
        self._outputs: List[Output] = []

    def append_text(self, content: str) -> None:
        value = content.strip()
        if not value:
            return
        self._outputs.append({"kind": "text", "payload": {"content": value}})

    def append_emoji(self, payload: Dict[str, Any]) -> None:
        data = payload.get("data")
        if not isinstance(data, str) or not data:
            raise CapabilityError("Golem returned invalid async emoji media")
        self._outputs.append({"kind": "emoji", "payload": _copy_payload(payload)})

    def outputs(self) -> List[Output]:
        return [_copy_output(output) for output in self._outputs]

    def has_outputs(self) -> bool:
        return bool(self._outputs)


current_output_builder: ContextVar[Optional[OutputBuilder]] = ContextVar(
    "golem_async_output_builder", default=None
)


def require_builder() -> OutputBuilder:
    builder = current_output_builder.get()
    if builder is None:
        raise CapabilityError("Async output builder is unavailable")
    return builder


def _copy_output(output: Output) -> Output:
    payload = output.get("payload")
    if not isinstance(payload, dict):
        raise CapabilityError("Async output payload is invalid")
    return {"kind": output.get("kind"), "payload": _copy_payload(payload)}


def _copy_payload(payload: Dict[str, Any]) -> Dict[str, Any]:
    return {str(key): value for key, value in payload.items()}
