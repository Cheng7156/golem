"""Async sticker output tests for Golem's Hermes plugin."""

from __future__ import annotations

import json
import threading
from unittest import mock
import unittest

import test_async_delivery as shared


api = shared.api
binding = shared.binding
plugin = shared.plugin
state = shared.state
runtime = shared.runtime


class AsyncStickerDeliveryTests(unittest.TestCase):
    def test_async_sticker_search_uses_ticket_binding(self):
        state.mark_ready(binding())
        token = state.current_child_delegation_id.set(binding().delegation_id)
        try:
            with mock.patch.object(
                api,
                "sticker_search",
                return_value={
                    "candidates": [
                        {"id": "candidate-1", "description": "ok", "url": "hidden"}
                    ],
                    "expires_in": 30,
                },
            ) as search:
                result = json.loads(plugin._handle_search({"query": "ok", "limit": 1}))
        finally:
            state.current_child_delegation_id.reset(token)
        search.assert_called_once_with(binding(), "ok", 1)
        self.assertEqual(
            result["candidates"],
            [{"id": "candidate-1", "description": "ok", "format": "", "animated": False}],
        )

    def test_async_sticker_attach_queues_direct_output(self):
        state.mark_ready(binding())
        token = state.current_child_delegation_id.set(binding().delegation_id)
        try:
            with mock.patch.object(
                api,
                "sticker_send",
                return_value={
                    "queued": True,
                    "outbox_id": "outbox-direct",
                    "sequence": 7,
                    "direct_output_count": 1,
                },
            ) as send:
                result = json.loads(
                    runtime.tool_execution(
                        tool_name="golem_sticker_attach",
                        args={"candidate_id": "candidate-1"},
                        tool_call_id="tool-call-1",
                        next_call=plugin._handle_attach,
                    )
                )
        finally:
            state.current_child_delegation_id.reset(token)
        send.assert_called_once_with(binding(), "candidate-1", "tool-call-1")
        self.assertEqual(
            result,
            {
                "queued": True,
                "kind": "emoji",
                "outbox_id": "outbox-direct",
                "sequence": 7,
            },
        )
        self.assertIsNone(state.current_tool_invocation_id.get())

    def test_async_sticker_attach_requires_runtime_invocation_id(self):
        state.mark_ready(binding())
        token = state.current_child_delegation_id.set(binding().delegation_id)
        try:
            with self.assertRaisesRegex(
                plugin.CapabilityError, "invocation_id is unavailable"
            ):
                plugin._handle_attach({"candidate_id": "candidate-1"})
        finally:
            state.current_child_delegation_id.reset(token)

    def test_sticker_send_posts_runtime_invocation_id(self):
        receipt = {
            "queued": True,
            "outbox_id": "outbox-direct",
            "sequence": 9,
            "direct_output_count": 1,
        }
        with mock.patch.object(api, "_post_idempotent", return_value=receipt) as post:
            result = api.sticker_send(binding(), "candidate-1", "tool-call-9")
        self.assertEqual(result, receipt)
        path, payload = post.call_args.args
        self.assertEqual(path, api.client.ASYNC_STICKER_SEND_PATH)
        self.assertEqual(payload["invocation_id"], "tool-call-9")
        self.assertEqual(payload["chat_id"], binding().chat_id)

    def test_tool_invocation_context_is_thread_isolated_and_reset(self):
        barrier = threading.Barrier(2)
        seen = []

        def execute(invocation_id):
            def next_call(_args):
                barrier.wait()
                seen.append(state.current_tool_invocation_id.get())
                return "ok"

            runtime.tool_execution(
                tool_name="test_tool", args={}, tool_call_id=invocation_id,
                next_call=next_call,
            )

        threads = [
            threading.Thread(target=execute, args=("call-left",)),
            threading.Thread(target=execute, args=("call-right",)),
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertCountEqual(seen, ["call-left", "call-right"])
        self.assertIsNone(state.current_tool_invocation_id.get())

    def test_tool_invocation_context_resets_after_handler_error(self):
        def fail(_args):
            self.assertEqual(state.current_tool_invocation_id.get(), "call-fail")
            raise RuntimeError("failed")

        with self.assertRaisesRegex(RuntimeError, "failed"):
            runtime.tool_execution(
                tool_name="test_tool", args={}, tool_call_id="call-fail",
                next_call=fail,
            )
        self.assertIsNone(state.current_tool_invocation_id.get())


if __name__ == "__main__":
    unittest.main()
