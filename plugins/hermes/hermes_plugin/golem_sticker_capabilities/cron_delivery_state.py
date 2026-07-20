"""Trusted per-run context for Golem-bound Hermes cron jobs."""

from __future__ import annotations

from contextvars import ContextVar
from dataclasses import dataclass
from typing import Dict, Optional


@dataclass(frozen=True)
class CronDeliveryBinding:
    profile: str
    job_id: str
    delivery_id: str

    def request_fields(self) -> Dict[str, str]:
        return {
            "profile": self.profile,
            "job_id": self.job_id,
            "delivery_id": self.delivery_id,
        }


current_cron_delivery: ContextVar[Optional[CronDeliveryBinding]] = ContextVar(
    "golem_cron_delivery", default=None
)
