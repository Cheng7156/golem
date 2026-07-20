from __future__ import annotations

import asyncio
import importlib
import json
import unittest
from unittest import mock

import test_async_delivery as shared


runtime = shared.runtime
state = shared.state
cron_delivery = shared.plugin._cron_delivery
video_context = importlib.import_module(
    f"{shared.SPEC.name}.video_async_context"
)


class CronAsyncDelegationTests(unittest.TestCase):
    def test_cron_delegate_uses_inherited_binding_without_relay_registration(self):
        result = json.dumps(
            {"status": "dispatched", "delegation_id": "deleg_cron1234"}
        )
        token = cron_delivery.current_cron_delivery.set(
            cron_delivery.CronDeliveryBinding(
                "default", "job-1", "job-1:scheduled"
            )
        )
        try:
            with mock.patch.object(runtime, "register_delivery") as register, mock.patch.object(
                runtime, "_interrupt_delegation"
            ) as interrupt:
                returned = runtime.tool_execution(
                    tool_name="delegate_task",
                    args={"background": True},
                    session_id="cron-session",
                    next_call=lambda _: result,
                )
        finally:
            cron_delivery.current_cron_delivery.reset(token)
        self.assertEqual(returned, result)
        register.assert_not_called()
        interrupt.assert_not_called()
        registered = state.wait_registration("deleg_cron1234", 0)
        self.assertEqual(registered.status, "cron")
        state.forget_registration("deleg_cron1234")

    def test_cron_child_resolves_inherited_media_binding(self):
        binding = cron_delivery.CronDeliveryBinding(
            "default", "job-2", "job-2:scheduled"
        )
        state.mark_cron("deleg_cron_media", "cron-session")
        child_token = state.current_child_delegation_id.set("deleg_cron_media")
        cron_token = cron_delivery.current_cron_delivery.set(binding)
        try:
            resolved = video_context.current_binding()
        finally:
            cron_delivery.current_cron_delivery.reset(cron_token)
            state.current_child_delegation_id.reset(child_token)
            state.forget_registration("deleg_cron_media")
        self.assertEqual(resolved, binding)

    def test_cron_completion_is_internal_and_releases_registration(self):
        calls = []

        class Runner:
            async def _inject_watch_notification(self, text, event):
                calls.append((text, event))

        runtime._patch_injection(Runner)
        state.mark_cron("deleg_cron_done", "cron-session")
        asyncio.run(
            Runner()._inject_watch_notification(
                "done",
                {"type": "async_delegation", "delegation_id": "deleg_cron_done"},
            )
        )
        self.assertEqual(calls, [])
        self.assertIsNone(state.wait_registration("deleg_cron_done", 0))


if __name__ == "__main__":
    unittest.main()
