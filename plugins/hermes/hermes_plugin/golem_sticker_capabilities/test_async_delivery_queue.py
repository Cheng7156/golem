"""Busy-session ordering tests for async delivery."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
import unittest
from unittest import mock

import test_async_delivery as shared


queue_runtime = shared.plugin._async_runtime.async_delivery_queue
state = shared.state


@dataclass
class Event:
    text: str
    source: object
    session_key: str = shared.CONTEXT["session_key"]


class Runner:
    _BUSY_QUEUE_MAX_PENDING = 32

    def __init__(self, adapter):
        self.adapter = adapter

    async def _handle_active_session_busy_message(self, _event, _session_key):
        return False

    def _adapter_for_source(self, _source):
        return self.adapter

    def _is_user_authorized(self, _source):
        return True


class Adapter:
    def __init__(self, runner):
        self.runner = runner
        self._active_sessions = {}
        self._session_tasks = {}
        self._background_tasks = set()
        self._pending_messages = {}
        self.processed = []
        self.first_turn_release = asyncio.Event()

    async def handle_message(self, event):
        session_key = event.session_key
        if session_key in self._active_sessions:
            handled = await self.runner._handle_active_session_busy_message(
                event, session_key
            )
            if handled:
                return
            pending = self._pending_messages.get(session_key)
            if pending is None:
                self._pending_messages[session_key] = event
            else:
                pending.text = f"{pending.text}\n{event.text}"
            return
        self._start_session_processing(event, session_key)

    def _start_session_processing(self, event, session_key):
        if event.text == "start failure":
            raise RuntimeError("start failure")
        guard = asyncio.Event()
        self._active_sessions[session_key] = guard
        task = asyncio.create_task(self._process_message_background(event, session_key))
        self._session_tasks[session_key] = task
        self._background_tasks.add(task)
        return True

    async def _process_message_background(self, event, session_key):
        guard = self._active_sessions[session_key]
        self.processed.append((event.text, state.current_delivery.get()))
        if event.text == "active":
            await self.first_turn_release.wait()
        pending = self._pending_messages.pop(session_key, None)
        if pending is not None:
            task = asyncio.create_task(
                self._process_message_background(pending, session_key)
            )
            self._session_tasks[session_key] = task
            self._background_tasks.add(task)
            return
        self._cleanup_finished_session_task(session_key, guard)

    def _cleanup_finished_session_task(self, session_key, interrupt_event):
        if self._active_sessions.get(session_key) is not interrupt_event:
            return
        self._active_sessions.pop(session_key)
        self._session_tasks.pop(session_key, None)

    async def cancel_background_tasks(self):
        tasks = [task for task in self._background_tasks if not task.done()]
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        self._background_tasks.clear()
        self._session_tasks.clear()
        self._pending_messages.clear()
        self._active_sessions.clear()


class AsyncDeliveryQueueTests(unittest.IsolatedAsyncioTestCase):
    async def test_busy_completion_stays_separate_and_runs_after_user_pending(self):
        runner = Runner(None)
        adapter = Adapter(runner)
        runner.adapter = adapter
        queue_runtime.install(
            Adapter,
            Runner,
            delivery_context=state.current_delivery,
            completion_event_context=state.current_completion_event,
            binding_is_ready=state.is_binding_ready,
        )
        state.mark_ready(shared.binding())

        await adapter.handle_message(Event("active", object()))
        await asyncio.sleep(0)
        await adapter.handle_message(Event("user pending", object()))
        token = state.current_delivery.set(shared.binding())
        event_token = state.current_completion_event.set(completion_payload())
        try:
            await adapter.handle_message(Event("async complete", object()))
        finally:
            state.current_completion_event.reset(event_token)
            state.current_delivery.reset(token)

        self.assertEqual(adapter._pending_messages[shared.CONTEXT["session_key"]].text, "user pending")
        adapter.first_turn_release.set()
        await self._wait_for_processed(adapter, 3)
        await asyncio.gather(*tuple(adapter._background_tasks))

        self.assertEqual(
            [item[0] for item in adapter.processed],
            ["active", "user pending", "async complete"],
        )
        self.assertEqual(adapter.processed[0][1], None)
        self.assertEqual(adapter.processed[1][1], None)
        self.assertEqual(adapter.processed[2][1], shared.binding())

    async def test_inactive_queued_completion_never_starts(self):
        runner = Runner(None)
        adapter = Adapter(runner)
        runner.adapter = adapter
        queue_runtime.install(
            Adapter,
            Runner,
            delivery_context=state.current_delivery,
            completion_event_context=state.current_completion_event,
            binding_is_ready=state.is_binding_ready,
        )
        state.mark_ready(shared.binding())

        await adapter.handle_message(Event("active", object()))
        await asyncio.sleep(0)
        active_task = adapter._session_tasks[shared.CONTEXT["session_key"]]
        token = state.current_delivery.set(shared.binding())
        event_token = state.current_completion_event.set(completion_payload())
        try:
            await adapter.handle_message(Event("revoked completion", object()))
        finally:
            state.current_completion_event.reset(event_token)
            state.current_delivery.reset(token)
        state.mark_session_inactive(shared.binding().hermes_session_id)

        adapter.first_turn_release.set()
        await active_task
        await asyncio.sleep(0)

        self.assertEqual([item[0] for item in adapter.processed], ["active"])
        self.assertNotIn(shared.CONTEXT["session_key"], adapter._active_sessions)

    async def test_user_arriving_during_completion_gets_clean_context(self):
        runner = Runner(None)
        adapter = Adapter(runner)
        runner.adapter = adapter
        queue_runtime.install(
            Adapter,
            Runner,
            delivery_context=state.current_delivery,
            completion_event_context=state.current_completion_event,
            binding_is_ready=state.is_binding_ready,
        )
        state.mark_ready(shared.binding())

        token = state.current_delivery.set(shared.binding())
        event_token = state.current_completion_event.set(completion_payload())
        try:
            await adapter.handle_message(Event("active", object()))
        finally:
            state.current_completion_event.reset(event_token)
            state.current_delivery.reset(token)
        await asyncio.sleep(0)
        await adapter.handle_message(Event("user during completion", object()))

        self.assertNotIn(shared.CONTEXT["session_key"], adapter._pending_messages)
        adapter.first_turn_release.set()
        await self._wait_for_processed(adapter, 2)
        await asyncio.gather(*tuple(adapter._background_tasks))

        self.assertEqual(
            [item[0] for item in adapter.processed],
            ["active", "user during completion"],
        )
        self.assertEqual(adapter.processed[0][1], shared.binding())
        self.assertEqual(adapter.processed[1][1], None)

    async def test_shutdown_discards_queued_completion_before_cancelling(self):
        runner = Runner(None)
        adapter = Adapter(runner)
        runner.adapter = adapter
        queue_runtime.install(
            Adapter,
            Runner,
            delivery_context=state.current_delivery,
            completion_event_context=state.current_completion_event,
            binding_is_ready=state.is_binding_ready,
        )
        state.mark_ready(shared.binding())
        await adapter.handle_message(Event("active", object()))
        await asyncio.sleep(0)
        token = state.current_delivery.set(shared.binding())
        event_token = state.current_completion_event.set(completion_payload())
        try:
            await adapter.handle_message(Event("queued completion", object()))
        finally:
            state.current_completion_event.reset(event_token)
            state.current_delivery.reset(token)

        lifecycle = queue_runtime.async_delivery_queue_lifecycle
        with mock.patch.object(lifecycle, "requeue_completion") as requeue:
            await adapter.cancel_background_tasks()
        completion_queues = getattr(
            adapter, "_golem_async_completion_queues_v1", {}
        )
        self.assertEqual(completion_queues, {})
        self.assertEqual([item[0] for item in adapter.processed], ["active"])
        requeue.assert_called_once_with(completion_payload())

    async def test_shutdown_requeues_active_completion(self):
        runner = Runner(None)
        adapter = Adapter(runner)
        runner.adapter = adapter
        queue_runtime.install(
            Adapter,
            Runner,
            delivery_context=state.current_delivery,
            completion_event_context=state.current_completion_event,
            binding_is_ready=state.is_binding_ready,
        )
        state.mark_ready(shared.binding())
        delivery_token = state.current_delivery.set(shared.binding())
        event_token = state.current_completion_event.set(completion_payload())
        try:
            await adapter.handle_message(Event("active", object()))
        finally:
            state.current_completion_event.reset(event_token)
            state.current_delivery.reset(delivery_token)
        await asyncio.sleep(0)

        lifecycle = queue_runtime.async_delivery_queue_lifecycle
        with mock.patch.object(lifecycle, "requeue_completion") as requeue:
            await adapter.cancel_background_tasks()
        requeue.assert_called_once_with(completion_payload())

    async def _wait_for_processed(self, adapter, expected):
        for _ in range(20):
            if len(adapter.processed) >= expected:
                return
            await asyncio.sleep(0)
        self.fail(f"only processed {len(adapter.processed)} events")


def completion_payload():
    return {
        "type": "async_delegation",
        "delegation_id": shared.binding().delegation_id,
        "session_key": shared.binding().relay_session_key,
        "result": "done",
    }


if __name__ == "__main__":
    unittest.main()
