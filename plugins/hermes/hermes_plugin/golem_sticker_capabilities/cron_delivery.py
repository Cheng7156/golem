"""Version-locked bridge for durable Hermes cron delivery to Golem."""

from __future__ import annotations

import inspect
import secrets
import time
from typing import Any, Dict

from . import capability_client as client
from . import cron_delivery_api
from .cron_delivery_state import CronDeliveryBinding, current_cron_delivery
from .async_delivery_api import _post_idempotent as post_idempotent, active_profile
from .errors import CapabilityError, RetryableCapabilityError


SUPPORTED_VERSION = "0.18.2"
_PATCH_MARKER = "_golem_cron_delivery_v1"
_PROFILE_FIELD = "golem_cron_profile"
_RUN_FIELD = "_golem_cron_delivery_id"
_DELIVERY_RETRY_WINDOW_SECONDS = 120.0
_DELIVERY_RETRY_INITIAL_SECONDS = 1.0
_DELIVERY_RETRY_MAX_SECONDS = 10.0


def install() -> None:
    import importlib.metadata
    from cron import scheduler
    from cron import jobs
    from tools import cronjob_tools

    version = importlib.metadata.version("hermes-agent")
    if version != SUPPORTED_VERSION:
        raise RuntimeError(
            f"Golem cron delivery requires hermes-agent=={SUPPORTED_VERSION}; found {version}"
        )
    _validate_signature(jobs.create_job, "prompt", "schedule")
    _validate_signature(jobs.update_job, "job_id", "updates")
    _validate_signature(scheduler.run_job, "job")
    _validate_signature(scheduler._deliver_result, "job", "content", "adapters", "loop")
    _validate_signature(cronjob_tools._execute_job_now, "job")
    _patch_job_creation(jobs, cronjob_tools)
    _patch_execute_now(cronjob_tools)
    _patch_run_job(scheduler)
    _patch_deliver_result(scheduler)


def _patch_job_creation(jobs: Any, cronjob_tools: Any) -> None:
    _patch_update_job(jobs)
    _patch_create_job(jobs)
    cronjob_tools.create_job = jobs.create_job
    cronjob_tools.update_job = jobs.update_job
    cronjob_tools.cronjob.__globals__["create_job"] = jobs.create_job
    cronjob_tools.cronjob.__globals__["update_job"] = jobs.update_job


def _patch_create_job(jobs: Any) -> None:
    original = jobs.create_job
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(*args: Any, **kwargs: Any) -> Dict[str, Any]:
        job = original(*args, **kwargs)
        origin = job.get("origin")
        if not _is_bound_origin_delivery(job, origin):
            return job
        profile = _context_profile()
        try:
            _register(job["id"], profile)
            updated = jobs.update_job(job["id"], {_PROFILE_FIELD: profile})
            if updated is None:
                raise CapabilityError("Hermes cron job disappeared during delivery registration")
        except Exception:
            jobs.remove_job(job["id"])
            raise
        return updated

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(wrapped, "__signature__", inspect.signature(original))
    jobs.create_job = wrapped


def _patch_update_job(jobs: Any) -> None:
    original = jobs.update_job
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(job_id: str, updates: Dict[str, Any]):
        value = dict(updates or {})
        current = jobs.get_job(job_id)
        predicted = {**current, **value} if isinstance(current, dict) else None
        if (
            predicted is not None
            and "deliver" in value
            and _is_bound_origin_delivery(predicted, predicted.get("origin"))
            and not str(predicted.get(_PROFILE_FIELD) or "").strip()
        ):
            profile = _context_profile()
            _register(str(predicted["id"]), profile)
            value[_PROFILE_FIELD] = profile
        return original(job_id, value)

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(wrapped, "__signature__", inspect.signature(original))
    jobs.update_job = wrapped


def _patch_execute_now(cronjob_tools: Any) -> None:
    original = cronjob_tools._execute_job_now
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(job: Dict[str, Any]) -> Dict[str, Any]:
        value = dict(job)
        value[_RUN_FIELD] = f"{job['id']}:manual:{secrets.token_hex(16)}"
        return original(value)

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(wrapped, "__signature__", inspect.signature(original))
    cronjob_tools._execute_job_now = wrapped


def _patch_deliver_result(scheduler: Any) -> None:
    original = scheduler._deliver_result
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(job: Dict[str, Any], content: str, adapters=None, loop=None):
        origin = job.get("origin")
        if not _is_bound_origin_delivery(job, origin):
            return original(job, content, adapters=adapters, loop=loop)
        profile = str(job.get(_PROFILE_FIELD) or "").strip()
        if not profile:
            return "Golem cron delivery binding is missing; recreate this cron job"
        delivery_id = _delivery_id(job)
        binding = CronDeliveryBinding(profile, str(job["id"]), delivery_id)
        try:
            if cron_delivery_api.direct_output_count(binding) > 0:
                return None
        except CapabilityError as exc:
            return f"Golem cron direct delivery status failed: {exc}"
        del adapters, loop
        payload = {
            "profile": profile,
            "job_id": str(job["id"]),
            "chat_id": str(origin["chat_id"]),
            "delivery_id": delivery_id,
            "content": content,
        }
        try:
            result = _deliver_until_committed(
                payload,
                stopping=lambda: scheduler._interpreter_shutting_down(),
            )
        except CapabilityError as exc:
            return f"Golem cron delivery failed: {exc}"
        if result.get("disposition") != "delivered":
            return "Golem cron delivery returned an invalid receipt"
        return None

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(wrapped, "__signature__", inspect.signature(original))
    scheduler._deliver_result = wrapped


def _patch_run_job(scheduler: Any) -> None:
    original = scheduler.run_job
    if getattr(original, _PATCH_MARKER, False):
        return

    def wrapped(job: Dict[str, Any], *args: Any, **kwargs: Any):
        binding = _run_binding(job)
        if binding is None:
            return original(job, *args, **kwargs)
        token = current_cron_delivery.set(binding)
        try:
            return original(job, *args, **kwargs)
        finally:
            current_cron_delivery.reset(token)

    setattr(wrapped, _PATCH_MARKER, True)
    setattr(wrapped, "__signature__", inspect.signature(original))
    scheduler.run_job = wrapped


def _run_binding(job: Dict[str, Any]) -> CronDeliveryBinding | None:
    if not _is_bound_origin_delivery(job, job.get("origin")):
        return None
    profile = str(job.get(_PROFILE_FIELD) or "").strip()
    if not profile:
        return None
    return CronDeliveryBinding(profile, str(job["id"]), _delivery_id(job))


def _register(job_id: str, profile: str) -> None:
    context = client.current_context()
    context["profile"] = profile
    result = post_idempotent(
        client.CRON_REGISTER_PATH,
        {"job_id": job_id, "context": context},
    )
    if result.get("registered") is not True:
        raise CapabilityError("Golem did not register cron delivery")


def _context_profile() -> str:
    profile = str(client.current_context().get("profile") or "").strip()
    return profile or active_profile()


def _deliver_until_committed(
    payload: Dict[str, str],
    *,
    stopping=lambda: False,
) -> Dict[str, Any]:
    deadline = time.monotonic() + _DELIVERY_RETRY_WINDOW_SECONDS
    delay = _DELIVERY_RETRY_INITIAL_SECONDS
    while True:
        try:
            return client.post_json(client.CRON_DELIVER_PATH, payload)
        except RetryableCapabilityError:
            if stopping():
                raise CapabilityError("Hermes Gateway is stopping during cron delivery") from None
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise
            time.sleep(min(delay, remaining))
            delay = min(delay * 2.0, _DELIVERY_RETRY_MAX_SECONDS)


def _delivery_id(job: Dict[str, Any]) -> str:
    if job.get(_RUN_FIELD):
        return str(job[_RUN_FIELD])
    scheduled = str(job.get("next_run_at") or job.get("run_claim") or "manual")
    return f"{job['id']}:{scheduled}"


def _is_relay_origin(origin: Any) -> bool:
    return isinstance(origin, dict) and origin.get("platform") == "relay" and bool(
        origin.get("chat_id")
    )


def _is_bound_origin_delivery(job: Dict[str, Any], origin: Any) -> bool:
    return _is_relay_origin(origin) and job.get("deliver") in {"origin", "relay"}


def _validate_signature(function: Any, *names: str) -> None:
    actual = tuple(inspect.signature(function).parameters)
    if actual[: len(names)] != names:
        raise RuntimeError(f"unsupported Hermes method signature: {function.__qualname__}{actual}")
