"""Security and failure-path tests for durable async delivery."""

from __future__ import annotations

import asyncio
from unittest import mock
import unittest

import test_async_delivery as shared


runtime = shared.runtime
output = shared.output
state = shared.state


class AsyncDeliverySecurityTests(unittest.TestCase):
    def test_reset_and_stop_revoke_only_current_session(self):
        state.mark_ready(shared.binding())
        source = object()
        event = mock.Mock(source=source)
        event.get_command.return_value = "stop"
        entry = mock.Mock(session_id="hermes-session-1", session_key="key-1")
        session_store = mock.Mock()
        session_store.get_or_create_session.return_value = entry
        gateway = mock.Mock()
        gateway._is_user_authorized.return_value = True
        with mock.patch.object(runtime, "revoke_session") as revoke:
            with mock.patch.object(
                runtime, "_interrupt_session_delegations"
            ) as interrupt:
                runtime.pre_gateway_dispatch(
                    event=event, gateway=gateway, session_store=session_store
                )
        revoke.assert_called_once_with("hermes-session-1", "default")
        interrupt.assert_called_once_with("key-1")
        state.mark_ready(shared.binding())
        with mock.patch.object(runtime, "revoke_session") as reset_revoke:
            runtime.session_reset(old_session_id="hermes-session-1")
        reset_revoke.assert_called_once_with("hermes-session-1", "default")

    def test_permanent_delivery_error_stops_without_retry(self):
        binding = shared.binding()
        state.mark_ready(binding)

        class Runner:
            async def _handle_message(self, _event):
                return "result"

        output.patch_message_handler(Runner)
        error = shared.plugin.CapabilityError("binding conflict")
        expected = [{"kind": "text", "payload": {"content": "result"}}]
        with mock.patch.object(output, "deliver_v2", side_effect=error) as deliver:
            with mock.patch.object(output.asyncio, "sleep", mock.AsyncMock()) as sleep:
                token = state.current_delivery.set(binding)
                try:
                    with mock.patch.object(output, "direct_output_count", return_value=0):
                        with self.assertRaisesRegex(type(error), "binding conflict"):
                            asyncio.run(Runner()._handle_message(object()))
                finally:
                    state.current_delivery.reset(token)
        deliver.assert_called_once_with(binding, expected)
        sleep.assert_not_awaited()
        self.assertFalse(state.is_binding_ready(binding))

    def test_permanent_status_error_is_not_requeued(self):
        binding = shared.binding()
        state.mark_ready(binding)

        class Runner:
            async def _inject_watch_notification(self, *_):
                raise AssertionError("permanent status error must not be injected")

        runtime._patch_injection(Runner)
        error = shared.plugin.CapabilityError("not found")
        with mock.patch.object(runtime, "delivery_status", side_effect=error):
            with mock.patch.object(runtime, "_requeue_completion") as requeue:
                asyncio.run(Runner()._inject_watch_notification(
                    "done",
                    {"type": "async_delegation", "delegation_id": binding.delegation_id},
                ))
        requeue.assert_not_called()
        self.assertFalse(state.is_binding_ready(binding))

    def test_orphan_completion_is_dropped_without_requeue_loop(self):
        class Runner:
            async def _inject_watch_notification(self, *_):
                raise AssertionError("orphan completion must not be injected")

        runtime._patch_injection(Runner)
        with mock.patch.object(runtime, "wait_registration", return_value=None):
            with mock.patch.object(runtime, "_requeue_completion") as requeue:
                asyncio.run(Runner()._inject_watch_notification(
                    "done",
                    {"type": "async_delegation", "delegation_id": "deleg_orphan1"},
                ))
        requeue.assert_not_called()

    def test_reset_during_registration_revokes_late_ticket(self):
        result = shared.json.dumps(
            {"status": "dispatched", "delegation_id": "deleg_resetreg"}
        )

        def register(_input):
            runtime._deactivate_session("hermes-session-1")
            value = shared.binding()
            return state.DeliveryBinding(
                **{**value.__dict__, "delegation_id": "deleg_resetreg"}
            )

        with mock.patch.object(runtime.client, "current_context", return_value=shared.CONTEXT):
            with mock.patch.object(runtime, "register_delivery", side_effect=register):
                with mock.patch.object(
                    runtime.async_delivery_lifecycle, "schedule_revoke"
                ) as revoke:
                    returned = runtime.tool_execution(
                        tool_name="delegate_task",
                        args={},
                        session_id="hermes-session-1",
                        next_call=lambda _: result,
                    )
        self.assertEqual(
            shared.json.loads(returned)["status"], "delivery_registration_failed"
        )
        revoke.assert_called_once()
        registered = state.wait_registration("deleg_resetreg", 0)
        self.assertEqual(registered.status, "inactive")

    def test_session_boundary_stops_durable_commit_retry(self):
        binding = shared.binding()
        state.mark_ready(binding)

        async def deactivate(_delay):
            state.mark_session_inactive(binding.hermes_session_id)

        error = runtime.RetryableCapabilityError("host unavailable")
        expected = [{"kind": "text", "payload": {"content": "result"}}]
        with mock.patch.object(output, "deliver_v2", side_effect=error) as deliver:
            with mock.patch.object(output.asyncio, "sleep", side_effect=deactivate):
                with self.assertRaisesRegex(
                    shared.plugin.CapabilityError, "no longer active"
                ):
                    asyncio.run(output.deliver_until_terminal(binding, expected))
        deliver.assert_called_once_with(binding, expected)

    def test_unauthorized_stop_cannot_revoke_or_interrupt(self):
        source = object()
        event = mock.Mock(source=source)
        event.get_command.return_value = "stop"
        gateway = mock.Mock()
        gateway._is_user_authorized.return_value = False
        session_store = mock.Mock()

        with mock.patch.object(runtime, "mark_session_inactive") as inactive:
            with mock.patch.object(runtime, "revoke_session") as revoke:
                with mock.patch.object(
                    runtime, "_interrupt_session_delegations"
                ) as interrupt:
                    runtime.pre_gateway_dispatch(
                        event=event, gateway=gateway, session_store=session_store
                    )

        gateway._is_user_authorized.assert_called_once_with(source)
        session_store.get_or_create_session.assert_not_called()
        inactive.assert_not_called()
        revoke.assert_not_called()
        interrupt.assert_not_called()

    def test_missing_final_text_fails_explicitly(self):
        state.mark_ready(shared.binding())

        class Runner:
            async def _handle_message(self, _event):
                return None

        output.patch_message_handler(Runner)
        token = state.current_delivery.set(shared.binding())
        try:
            with mock.patch.object(output, "direct_output_count", return_value=0):
                with self.assertRaisesRegex(
                    shared.plugin.CapabilityError, "produced no outputs"
                ):
                    asyncio.run(Runner()._handle_message(object()))
        finally:
            state.current_delivery.reset(token)


if __name__ == "__main__":
    unittest.main()
