"""HTTP protocol for direct media delivery from Golem-bound cron runs."""

from __future__ import annotations

from typing import Any, Dict

from . import capability_client as client
from .async_delivery_api import _post_idempotent
from .cron_delivery_state import CronDeliveryBinding
from .errors import CapabilityError
from .video_delivery_types import VideoSearchRequest


def direct_output_count(binding: CronDeliveryBinding) -> int:
    result = _post_idempotent(client.CRON_STATUS_PATH, binding.request_fields())
    count = result.get("direct_output_count")
    if not isinstance(count, int) or isinstance(count, bool) or count < 0:
        raise CapabilityError("Golem returned an invalid cron direct output count")
    return count


def video_search(
    binding: CronDeliveryBinding,
    request: VideoSearchRequest,
) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload.update(
        {
            "category": request.category,
            "query": request.query,
            "provider_id": request.provider_id,
            "limit": request.limit,
        }
    )
    return _post_idempotent(client.CRON_VIDEO_SEARCH_PATH, payload)


def video_send(
    binding: CronDeliveryBinding, candidate_id: str, invocation_id: str
) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["candidate_id"] = candidate_id
    payload["invocation_id"] = invocation_id
    return _post_idempotent(client.CRON_VIDEO_SEND_PATH, payload)


def video_status(binding: CronDeliveryBinding, job_id: str) -> Dict[str, Any]:
    payload: Dict[str, Any] = binding.request_fields()
    payload["video_job_id"] = job_id
    return _post_idempotent(client.CRON_VIDEO_STATUS_PATH, payload)
