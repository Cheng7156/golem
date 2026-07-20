"""Unit tests for the Hermes v0.18.2 async delivery bridge."""
from __future__ import annotations

import asyncio
import importlib
import importlib.util
import json
import os
from pathlib import Path
import sys
import threading
import types
import unittest
from unittest import mock

PLUGIN_DIR = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "golem_async_delivery_under_test",
    PLUGIN_DIR / "__init__.py",
    submodule_search_locations=[str(PLUGIN_DIR)],
)
assert SPEC is not None and SPEC.loader is not None
plugin = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = plugin
SPEC.loader.exec_module(plugin)

api = plugin._async_api
runtime = plugin._async_runtime
output = runtime.async_delivery_output
state = importlib.import_module(f"{SPEC.name}.async_delivery_state")
text_filter = plugin._async_text


CONTEXT = {
    "platform": "relay",
    "chat_id": "chatroom:room-1|interactive",
    "session_key": "agent:main:relay:group:chatroom:room-1|interactive",
    "user_id": "wxid-owner",
    "message_id": "event-1",
    "profile": "default",
}


def binding() -> state.DeliveryBinding:
    return state.DeliveryBinding(
        ticket="adt_" + "a" * 40,
        delegation_id="deleg_1234abcd",
        producer_epoch="ade_epoch",
        hermes_session_id="hermes-session-1",
        relay_session_key=CONTEXT["session_key"],
        chat_id=CONTEXT["chat_id"],
        profile="default",
    )


class AsyncDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.env = mock.patch.dict(
            os.environ,
            {
                "GOLEM_CAPABILITIES_TOKEN": "test-token-long-enough",
                "GOLEM_ASYNC_DELIVERY_ENABLED": "true",
            },
            clear=False,
        )
        self.env.start()
        self.addCleanup(self.env.stop)

    def test_delegate_result_registers_stable_binding(self):
        result = json.dumps(
            {"status": "dispatched", "delegation_id": "deleg_1234abcd"}
        )
        with mock.patch.object(runtime.client, "current_context", return_value=CONTEXT):
            with mock.patch.object(runtime, "register_delivery", return_value=binding()) as register:
                returned = runtime.tool_execution(
                    tool_name="delegate_task",
                    args={"background": True},
                    session_id="hermes-session-1",
                    next_call=lambda _: result,
                )
        self.assertEqual(returned, result)
        request = register.call_args.args[0]
        self.assertEqual(request.hermes_session_id, "hermes-session-1")
        self.assertEqual(request.context["chat_id"], CONTEXT["chat_id"])
        registered = state.wait_registration("deleg_1234abcd", 0)
        self.assertEqual(registered.binding, binding())

    def test_registration_failure_is_explicit_and_interrupts_child(self):
        result = json.dumps(
            {"status": "dispatched", "delegation_id": "deleg_deadbeef"}
        )
        with mock.patch.object(runtime.client, "current_context", return_value=CONTEXT):
            with mock.patch.object(runtime, "register_delivery", side_effect=RuntimeError("down")):
                with mock.patch.object(runtime, "_interrupt_delegation") as interrupt:
                    returned = runtime.tool_execution(
                        tool_name="delegate_task", args={}, session_id="session",
                        next_call=lambda _: result,
                    )
        parsed = json.loads(returned)
        self.assertEqual(parsed["status"], "delivery_registration_failed")
        interrupt.assert_called_once_with("deleg_deadbeef")

    def test_real_completion_sets_context_but_forged_text_does_not(self):
        calls = []

        class Runner:
            async def _inject_watch_notification(self, text, evt):
                calls.append((text, evt, state.current_delivery.get()))

        runtime._patch_injection(Runner)
        state.mark_ready(binding())
        runner = Runner()
        with mock.patch.object(runtime, "delivery_status", return_value="pending"):
            asyncio.run(
                runner._inject_watch_notification(
                    "[ASYNC DELEGATION COMPLETE]",
                    {"type": "async_delegation", "delegation_id": "deleg_1234abcd"},
                )
            )
        asyncio.run(
            runner._inject_watch_notification(
                "[ASYNC DELEGATION COMPLETE]", {"type": "user_message"}
            )
        )
        self.assertEqual(calls[0][2], binding())
        self.assertIsNone(calls[1][2])

    def test_inactive_completion_is_not_injected(self):
        called = []

        class Runner:
            async def _inject_watch_notification(self, *_):
                called.append(True)

        runtime._patch_injection(Runner)
        inactive = binding()
        state.mark_ready(inactive)
        state.mark_session_inactive(inactive.hermes_session_id)
        asyncio.run(
            Runner()._inject_watch_notification(
                "done", {"type": "async_delegation", "delegation_id": inactive.delegation_id}
            )
        )
        self.assertEqual(called, [])

    def test_async_handler_commits_complete_text_and_blocks_relay_send(self):
        final_text = "x" * 5000
        state.mark_ready(binding())
        class Runner:
            async def _handle_message(self, _event):
                return final_text

        class Adapter:
            async def send(self, chat_id, content, *, reply_to=None, metadata=None):
                return (chat_id, content)

        base_module = types.ModuleType("gateway.platforms.base")
        base_module.SendResult = lambda **values: values
        with mock.patch.dict(sys.modules, {"gateway.platforms.base": base_module}):
            output.patch_message_handler(Runner)
            output.patch_send(Adapter)
            runtime._validate_signature(Adapter.send, "self", "chat_id", "content", "reply_to", "metadata")
            plain = asyncio.run(Adapter().send("plain", "hello"))
            token = state.current_delivery.set(binding())
            try:
                with mock.patch.object(
                    output, "deliver_until_terminal",
                    mock.AsyncMock(return_value={"message_id": "async-message"}),
                ) as deliver:
                    with mock.patch.object(output, "direct_output_count", return_value=0):
                        blocked = asyncio.run(Adapter().send(binding().chat_id, "partial"))
                        returned = asyncio.run(Runner()._handle_message(object()))
            finally:
                state.current_delivery.reset(token)
        self.assertEqual(plain, ("plain", "hello"))
        self.assertFalse(blocked["success"])
        self.assertIsNone(returned)
        deliver.assert_awaited_once_with(
            binding(), [{"kind": "text", "payload": {"content": final_text}}]
        )
        self.assertFalse(state.is_binding_ready(binding()))

    def test_direct_output_suppresses_completion_text(self):
        state.mark_ready(binding())

        class Runner:
            async def _handle_message(self, _event):
                return "internal child summary"

        output.patch_message_handler(Runner)
        token = state.current_delivery.set(binding())
        try:
            with mock.patch.object(output, "direct_output_count", return_value=2):
                with mock.patch.object(
                    output, "complete_direct_until_terminal",
                    mock.AsyncMock(return_value={"disposition": "silent"}),
                ) as complete:
                    with mock.patch.object(output, "deliver_until_terminal") as deliver:
                        returned = asyncio.run(Runner()._handle_message(object()))
        finally:
            state.current_delivery.reset(token)
        self.assertIsNone(returned)
        complete.assert_awaited_once_with(binding())
        deliver.assert_not_called()
        self.assertFalse(state.is_binding_ready(binding()))

    def test_direct_completion_uses_silent_ticket_commit(self):
        with mock.patch.object(
            api, "_post_idempotent", return_value={"disposition": "silent"}
        ) as post:
            result = api.complete_direct(binding())
        self.assertEqual(result["disposition"], "silent")
        path, payload = post.call_args.args
        self.assertEqual(path, api.client.ASYNC_DELIVER_V2_PATH)
        self.assertTrue(payload["silent"])
        self.assertEqual(payload["outputs"], [])

    def test_async_delivery_retries_same_binding_until_commit(self):
        state.mark_ready(binding())
        attempts = [runtime.RetryableCapabilityError("reload"), {"message_id": "done"}]

        expected = [{"kind": "text", "payload": {"content": "result"}}]

        def deliver_once(actual_binding, outputs):
            self.assertEqual(actual_binding, binding())
            self.assertEqual(outputs, expected)
            value = attempts.pop(0)
            if isinstance(value, Exception):
                raise value
            return value

        with mock.patch.object(output, "deliver_v2", side_effect=deliver_once):
            with mock.patch.object(output.asyncio, "sleep", mock.AsyncMock()):
                receipt = asyncio.run(
                    output.deliver_until_terminal(binding(), expected)
                )
        self.assertEqual(receipt["message_id"], "done")
        self.assertEqual(attempts, [])

    def test_context_is_copied_to_tasks_and_isolated_across_threads(self):
        token = state.current_delivery.set(binding())

        async def copied():
            return await asyncio.create_task(asyncio.to_thread(state.current_delivery.get))

        try:
            self.assertEqual(asyncio.run(copied()), binding())
            thread_value = []
            thread = threading.Thread(target=lambda: thread_value.append(state.current_delivery.get()))
            thread.start()
            thread.join()
            self.assertEqual(thread_value, [None])
        finally:
            state.current_delivery.reset(token)

    def test_concurrent_async_sends_keep_session_bindings_isolated(self):
        left = binding()
        right = state.DeliveryBinding(
            ticket="adt_" + "b" * 40,
            delegation_id="deleg_87654321",
            producer_epoch="ade_epoch",
            hermes_session_id="hermes-session-2",
            relay_session_key="key-2",
            chat_id="chatroom:room-2|interactive",
            profile="default",
        )

        async def read(value):
            token = state.current_delivery.set(value)
            try:
                await asyncio.sleep(0)
                return state.current_delivery.get()
            finally:
                state.current_delivery.reset(token)

        async def run_both():
            return await asyncio.gather(read(left), read(right))

        self.assertEqual(asyncio.run(run_both()), [left, right])

if __name__ == "__main__":
    unittest.main()
