"""Non-blocking lifecycle calls for durable async delivery."""

from __future__ import annotations

import asyncio
import logging
from typing import Callable


logger = logging.getLogger(__name__)
_tasks: set[asyncio.Task[int]] = set()


def schedule_revoke(
    hermes_session_id: str,
    profile: str,
    revoke: Callable[[str, str], int],
) -> None:
    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        revoke(hermes_session_id, profile)
        return
    task = loop.create_task(
        asyncio.to_thread(revoke, hermes_session_id, profile),
        name=f"golem-revoke-{hermes_session_id}",
    )
    _tasks.add(task)
    task.add_done_callback(_revoke_done)


def _revoke_done(task: asyncio.Task[int]) -> None:
    _tasks.discard(task)
    try:
        task.result()
    except asyncio.CancelledError:
        logger.error("Hermes async delivery revocation task was cancelled")
    except Exception:
        logger.exception("Hermes async delivery revocation failed")
