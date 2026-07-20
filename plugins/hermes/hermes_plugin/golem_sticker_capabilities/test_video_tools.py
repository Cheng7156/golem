from __future__ import annotations

import json
import importlib.util
import os
from pathlib import Path
import sys
import unittest
from unittest import mock


PLUGIN_DIR = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "golem_video_tools_under_test",
    PLUGIN_DIR / "__init__.py",
    submodule_search_locations=[str(PLUGIN_DIR)],
)
assert SPEC is not None and SPEC.loader is not None
plugin = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = plugin
SPEC.loader.exec_module(plugin)
video_tools = plugin._video_tools
CapabilityError = plugin.CapabilityError


class VideoToolsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.original_post = video_tools._client.post_json
        self.original_context = video_tools._client.current_context
        self.original_sleep = video_tools.time.sleep
        video_tools._client.current_context = lambda: {"platform": "relay"}
        video_tools.time.sleep = lambda _: None

    def tearDown(self) -> None:
        video_tools._client.post_json = self.original_post
        video_tools._client.current_context = self.original_context
        video_tools.time.sleep = self.original_sleep

    def test_search_cleans_candidates_and_preserves_failures(self) -> None:
        video_tools._client.post_json = lambda path, payload: {
            "candidates": [
                {
                    "id": "vid_1",
                    "provider_id": "funny_api",
                    "title": "Funny",
                    "duration_seconds": 3,
                }
            ],
            "failures": [{"provider_id": "down", "message": "quota"}],
            "expires_in": 600,
        }
        result = json.loads(video_tools.handle_search({"category": "funny"}))
        self.assertEqual(result["candidates"][0]["id"], "vid_1")
        self.assertEqual(result["failures"][0]["provider_id"], "down")

    def test_select_polls_until_staged(self) -> None:
        responses = iter(
            [
                {"job_id": "vjob_1", "state": "pending"},
                {"job_id": "vjob_1", "state": "pending"},
                {
                    "job_id": "vjob_1",
                    "state": "completed",
                    "staged": True,
                    "fallback": False,
                    "effect_only_token": "[[TOKEN]]",
                },
            ]
        )
        video_tools._client.post_json = lambda path, payload: next(responses)
        result = json.loads(video_tools.handle_select({"candidate_id": "vid_1"}))
        self.assertTrue(result["staged"])
        self.assertEqual(result["effect_only_token"], "[[TOKEN]]")

    def test_failed_job_exposes_reason_and_link(self) -> None:
        responses = iter(
            [
                {"job_id": "vjob_1", "state": "pending"},
                {
                    "job_id": "vjob_1",
                    "state": "failed",
                    "failure_reason": "ffmpeg failed",
                    "fallback_url": "https://example.com/video",
                },
            ]
        )
        video_tools._client.post_json = lambda path, payload: next(responses)
        with self.assertRaisesRegex(CapabilityError, "ffmpeg failed.*example.com"):
            video_tools.handle_select({"candidate_id": "vid_1"})

    def test_status_poll_retries_temporary_capability_error(self) -> None:
        responses = iter(
            [
                {"job_id": "vjob_1", "state": "pending"},
                video_tools.RetryableCapabilityError("reload"),
                {
                    "state": "completed",
                    "staged": True,
                    "effect_only_token": "[[TOKEN]]",
                },
            ]
        )

        def post(path, payload):
            value = next(responses)
            if isinstance(value, Exception):
                raise value
            return value

        video_tools._client.post_json = post
        result = json.loads(video_tools.handle_select({"candidate_id": "vid_1"}))
        self.assertTrue(result["staged"])

    def test_video_tools_are_hidden_until_explicitly_enabled(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertFalse(video_tools._enabled())
        with mock.patch.dict(os.environ, {"GOLEM_VIDEO_ENABLED": "true"}, clear=True):
            self.assertTrue(video_tools._enabled())


if __name__ == "__main__":
    unittest.main()
