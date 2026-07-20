"""Immutable request values shared by async and cron video delivery."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class VideoSearchRequest:
    category: str
    query: str
    provider_id: str
    limit: int
