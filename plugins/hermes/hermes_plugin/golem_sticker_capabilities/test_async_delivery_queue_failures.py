"""Failure-path tests for async completion queue state."""

from __future__ import annotations

import unittest

import test_async_delivery as shared
from test_async_delivery_queue import Adapter, Event, Runner, completion_payload


queue_runtime = shared.plugin._async_runtime.async_delivery_queue
state = shared.state


class AsyncDeliveryQueueFailureTests(unittest.IsolatedAsyncioTestCase):
    async def test_start_failure_clears_active_completion_state(self):
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
            with self.assertRaisesRegex(RuntimeError, "start failure"):
                await adapter.handle_message(Event("start failure", object()))
        finally:
            state.current_completion_event.reset(event_token)
            state.current_delivery.reset(delivery_token)
        self.assertEqual(queue_runtime._active_bindings(adapter), {})
        self.assertEqual(queue_runtime._active_events(adapter), {})


if __name__ == "__main__":
    unittest.main()
