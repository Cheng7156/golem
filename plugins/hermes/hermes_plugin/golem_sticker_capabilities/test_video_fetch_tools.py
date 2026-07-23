from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import sys
import unittest
from unittest import mock


PLUGIN_DIR = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "golem_video_fetch_tools_under_test",
    PLUGIN_DIR / "__init__.py",
    submodule_search_locations=[str(PLUGIN_DIR)],
)
assert SPEC is not None and SPEC.loader is not None
plugin = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = plugin
SPEC.loader.exec_module(plugin)
tools = plugin._video_fetch_tools


class Binding:
    pass


class VideoFetchToolsTest(unittest.TestCase):
    def test_single_json_candidate_queues_without_second_model_selection(self) -> None:
        binding = Binding()
        inspection = {
            "kind": "json",
            "document": {"data": {"play_url": "https://cdn.example/video.mp4"}},
            "candidates": [
                {"path": "$.data.play_url", "url": "https://cdn.example/video.mp4", "score": 50}
            ],
        }
        with (
            mock.patch.object(tools._context, "current_binding", return_value=binding),
            mock.patch.object(tools._context, "video_inspect", return_value=inspection) as inspect,
            mock.patch.object(
                tools._context,
                "video_send_url",
                return_value={"job_id": "job-json", "state": "pending"},
            ) as send,
            mock.patch.object(tools._context, "invocation_id", return_value="call-json"),
            mock.patch.object(
                tools._async_video, "_wait_for_delivery", return_value='{"queued":true}'
            ),
        ):
            result = json.loads(tools.handle_fetch({"url": "https://api.example/video"}))
        self.assertTrue(result["queued"])
        inspect.assert_called_once_with(binding, "https://api.example/video")
        send.assert_called_once_with(
            binding, "https://cdn.example/video.mp4", "", "call-json"
        )

    def test_ambiguous_json_candidates_return_one_selection_context(self) -> None:
        binding = Binding()
        inspection = {
            "kind": "json",
            "document": {
                "primary": "https://cdn.example/one.mp4",
                "backup": "https://cdn.example/two.mp4",
            },
            "candidates": [
                {"path": "$.primary", "url": "https://cdn.example/one.mp4", "score": 45},
                {"path": "$.backup", "url": "https://cdn.example/two.mp4", "score": 40},
            ],
        }
        with (
            mock.patch.object(tools._context, "current_binding", return_value=binding),
            mock.patch.object(tools._context, "video_inspect", return_value=inspection),
            mock.patch.object(tools._context, "video_send_url") as send,
        ):
            result = json.loads(tools.handle_fetch({"url": "https://api.example/video"}))
        self.assertEqual(result["status"], "needs_selection")
        self.assertEqual(len(result["candidates"]), 2)
        send.assert_not_called()

    def test_clear_score_winner_queues_automatically(self) -> None:
        candidates = [
            {"path": "$.video", "url": "https://cdn.example/video.mp4", "score": 65},
            {"path": "$.page", "url": "https://example.com/watch", "score": 10},
        ]
        selected = tools._automatic_candidate(candidates)
        self.assertEqual(selected["url"], "https://cdn.example/video.mp4")

    def test_direct_response_queues_video_and_polls(self) -> None:
        binding = Binding()
        with (
            mock.patch.object(tools._context, "current_binding", return_value=binding),
            mock.patch.object(
                tools._context,
                "video_inspect",
                return_value={"kind": "video", "final_url": "https://cdn.example/video.mp4"},
            ),
            mock.patch.object(tools._context, "video_send_url", return_value={"job_id": "job-1", "state": "pending"}) as send,
            mock.patch.object(tools._context, "invocation_id", return_value="call-1"),
            mock.patch.object(tools._async_video, "_wait_for_delivery", return_value='{"queued":true}'),
        ):
            result = json.loads(tools.handle_fetch({"url": "https://api.example/video"}))
        self.assertTrue(result["queued"])
        send.assert_called_once_with(binding, "https://cdn.example/video.mp4", "", "call-1")

    def test_selected_json_url_queues_without_reinspection(self) -> None:
        binding = Binding()
        with (
            mock.patch.object(tools._context, "current_binding", return_value=binding),
            mock.patch.object(tools._context, "video_inspect") as inspect,
            mock.patch.object(
                tools._context,
                "video_send_url",
                return_value={"job_id": "job-2", "state": "pending"},
            ) as send,
            mock.patch.object(tools._context, "invocation_id", return_value="call-2"),
            mock.patch.object(
                tools._async_video,
                "_wait_for_delivery",
                return_value='{"queued":true}',
            ),
        ):
            result = json.loads(
                tools.handle_fetch(
                    {
                        "url": "https://api.example/video",
                        "media_url": "https://cdn.example/video.mp4",
                        "title": "sample",
                    }
                )
            )
        self.assertTrue(result["queued"])
        inspect.assert_not_called()
        send.assert_called_once_with(
            binding, "https://cdn.example/video.mp4", "sample", "call-2"
        )

    def test_parent_turn_enqueues_durable_job_without_delegation(self) -> None:
        with (
            mock.patch.object(tools._context, "current_binding", return_value=None),
            mock.patch.object(tools._context, "invocation_id", return_value="call-inline"),
            mock.patch.object(
                tools._context,
                "inline_video_fetch",
                return_value={
                    "accepted": True,
                    "job_id": "avjob-inline",
                    "state": "pending",
                    "deduplicated": False,
                },
            ) as enqueue,
        ):
            result = json.loads(tools.handle_fetch({"url": "https://api.example/video"}))
        self.assertTrue(result["accepted"])
        self.assertEqual(result["job_id"], "avjob-inline")
        enqueue.assert_called_once_with(
            "https://api.example/video", "", "call-inline"
        )


if __name__ == "__main__":
    unittest.main()
