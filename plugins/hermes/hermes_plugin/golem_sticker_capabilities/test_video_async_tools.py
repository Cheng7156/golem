from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import sys
import unittest
from unittest import mock


PLUGIN_DIR = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "golem_video_async_tools_under_test",
    PLUGIN_DIR / "__init__.py",
    submodule_search_locations=[str(PLUGIN_DIR)],
)
assert SPEC is not None and SPEC.loader is not None
plugin = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = plugin
SPEC.loader.exec_module(plugin)
tools = plugin._video_async_tools


class Binding:
    pass


class VideoAsyncToolsTest(unittest.TestCase):
    def test_attach_polls_and_returns_durable_receipt(self) -> None:
        binding = Binding()
        responses = iter(
            [
                {"job_id": "avjob_1", "state": "pending"},
                {
                    "job_id": "avjob_1",
                    "state": "completed",
                    "queued": True,
                    "outbox_id": "outbox-1",
                    "sequence": 9,
                    "direct_output_count": 1,
                },
            ]
        )
        with (
            mock.patch.object(tools._context, "current_binding", return_value=binding),
            mock.patch.object(tools._context, "invocation_id", return_value="call-1"),
            mock.patch.object(tools._context, "video_send", return_value=next(responses)),
            mock.patch.object(tools._context, "video_status", side_effect=lambda *_: next(responses)),
            mock.patch.object(tools._video, "_prepare_timeout", return_value=1),
            mock.patch.object(tools.time, "sleep"),
        ):
            result = json.loads(tools.handle_attach({"candidate_id": "vid-1"}))
        self.assertEqual(result["kind"], "video")
        self.assertEqual(result["outbox_id"], "outbox-1")
        self.assertEqual(result["sequence"], 9)

    def test_child_search_uses_delivery_binding(self) -> None:
        binding = Binding()
        with (
            mock.patch.object(plugin._video_tools._async_context, "current_binding", return_value=binding),
            mock.patch.object(
                plugin._video_tools._async_context,
                "video_search",
                return_value={"candidates": [{"id": "vid-1"}], "expires_in": 600},
            ) as search,
        ):
            result = json.loads(plugin._video_tools.handle_search({"category": "funny"}))
        self.assertEqual(result["candidates"][0]["id"], "vid-1")
        self.assertIs(search.call_args.args[0], binding)
        request = search.call_args.args[1]
        self.assertEqual(request.category, "funny")
        self.assertEqual(request.limit, 3)

    def test_attach_rejects_unverified_success(self) -> None:
        binding = Binding()
        with (
            mock.patch.object(tools._context, "current_binding", return_value=binding),
            mock.patch.object(tools._context, "invocation_id", return_value="call-1"),
            mock.patch.object(tools._context, "video_send", return_value={"job_id": "avjob-1", "state": "pending"}),
            mock.patch.object(tools._context, "video_status", return_value={"state": "completed", "queued": False}),
            mock.patch.object(tools._video, "_prepare_timeout", return_value=1),
        ):
            with self.assertRaises(plugin.CapabilityError):
                tools.handle_attach({"candidate_id": "vid-1"})

    def test_cron_attach_uses_cron_video_protocol(self) -> None:
        binding = tools._context.CronDeliveryBinding(
            "default", "job-1", "job-1:scheduled"
        )
        responses = iter(
            [
                {"job_id": "cvjob_1", "state": "pending"},
                {
                    "state": "completed",
                    "queued": True,
                    "outbox_id": "outbox-cron-1",
                    "sequence": 12,
                    "direct_output_count": 1,
                },
            ]
        )
        with (
            mock.patch.object(tools._context, "current_binding", return_value=binding),
            mock.patch.object(tools._context, "invocation_id", return_value="call-cron"),
            mock.patch.object(tools._context, "video_send", return_value=next(responses)) as send,
            mock.patch.object(tools._context, "video_status", side_effect=lambda *_: next(responses)),
            mock.patch.object(tools._video, "_prepare_timeout", return_value=1),
            mock.patch.object(tools.time, "sleep"),
        ):
            result = json.loads(tools.handle_attach({"candidate_id": "vid-cron"}))
        self.assertEqual(result["outbox_id"], "outbox-cron-1")
        send.assert_called_once_with(binding, "vid-cron", "call-cron")


if __name__ == "__main__":
    unittest.main()
