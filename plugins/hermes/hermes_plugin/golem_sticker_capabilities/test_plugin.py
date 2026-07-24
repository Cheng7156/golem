"""Self-contained unit tests for the deployable Hermes user plugin."""

from __future__ import annotations

import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import threading
import unittest
from unittest import mock


PLUGIN_DIR = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "golem_sticker_capabilities_under_test",
    PLUGIN_DIR / "__init__.py",
    submodule_search_locations=[str(PLUGIN_DIR)],
)
assert SPEC is not None and SPEC.loader is not None
plugin = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = plugin
SPEC.loader.exec_module(plugin)


SESSION = {
    "HERMES_SESSION_PLATFORM": "relay",
    "HERMES_SESSION_CHAT_ID": "room-1|interactive",
    "HERMES_SESSION_THREAD_ID": "",
    "HERMES_SESSION_USER_ID": "wxid-owner",
    "HERMES_SESSION_KEY": "agent:main:relay:group:room-1|interactive",
    "HERMES_SESSION_ID": "room-1",
    "HERMES_SESSION_MESSAGE_ID": "msg-1",
    "HERMES_SESSION_PROFILE": "default",
}


class _Response:
    def __init__(self, payload, content_type="application/json"):
        self.payload = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
        self.headers = {"Content-Type": content_type}

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False

    def read(self, _limit):
        return self.payload


class PluginTests(unittest.TestCase):
    def setUp(self):
        self.env = mock.patch.dict(
            os.environ,
            {
                "GOLEM_CAPABILITIES_URL": "http://127.0.0.1:8789/",
                "GOLEM_CAPABILITIES_TOKEN": "test-secret-not-a-real-key",
                "GOLEM_STICKER_INSPECT_ENABLED": "true",
                "GOLEM_ASYNC_DELIVERY_ENABLED": "false",
            },
            clear=False,
        )
        self.env.start()
        self.addCleanup(self.env.stop)
        self.session = mock.patch.object(
            plugin._client, "_session_value", side_effect=lambda name: SESSION.get(name, "")
        )
        self.session.start()
        self.addCleanup(self.session.stop)
        self.child_context = mock.patch.object(plugin._async_child_context, "install")
        self.child_context.start()
        self.addCleanup(self.child_context.stop)

    def test_search_posts_context_and_sanitizes_candidates(self):
        response = _Response(
            {
                "candidates": [
                    {
                        "id": "candidate-1",
                        "description": "吃瓜",
                        "format": "gif",
                        "animated": True,
                        "url": "https://must-not-reach-model.invalid/x.gif",
                    }
                ],
                "expires_in": 120,
            }
        )
        with mock.patch.object(plugin._opener, "open", return_value=response) as opened:
            result = json.loads(plugin._handle_search({"query": "吃瓜", "limit": 3}))

        self.assertEqual(result["candidates"][0]["id"], "candidate-1")
        self.assertNotIn("url", result["candidates"][0])
        request = opened.call_args.args[0]
        body = json.loads(request.data.decode())
        self.assertEqual(body["context"]["chat_id"], SESSION["HERMES_SESSION_CHAT_ID"])
        self.assertEqual(body["context"]["session_key"], SESSION["HERMES_SESSION_KEY"])
        self.assertNotIn("chat_id", body.keys() - {"context"})
        self.assertTrue(request.full_url.endswith("/capabilities/v1/stickers/search"))

    def test_image_search_is_metadata_only_and_supports_sender_filters(self):
        response = _Response(
            {
                "candidates": [
                    {
                        "id": "img_opaque",
                        "event_id": "event-7",
                        "message_id": "wx-7",
                        "speaker_id": "wxid-member",
                        "speaker_name": "成员",
                        "kind": "image",
                        "mime_type": "image/png",
                        "occurred_at": "2026-07-24T12:00:00Z",
                        "readable": True,
                        "is_current_sender": True,
                        "is_current_message": False,
                        "url": "https://must-not-reach-model.invalid/x.png",
                    }
                ],
                "expires_in": 120,
            }
        )
        with mock.patch.object(plugin._opener, "open", return_value=response) as opened:
            result = json.loads(
                plugin._image_tools._handle_search(
                    {
                        "speaker_id": "wxid-member",
                        "speaker_name": "成员",
                        "message_id": "wx-7",
                        "limit": 4,
                    }
                )
            )
        self.assertEqual(result["candidates"][0]["id"], "img_opaque")
        self.assertNotIn("url", result["candidates"][0])
        self.assertTrue(result["candidates"][0]["is_current_sender"])
        self.assertFalse(result["candidates"][0]["is_current_message"])
        body = json.loads(opened.call_args.args[0].data.decode())
        self.assertEqual(body["speaker_id"], "wxid-member")
        self.assertEqual(body["speaker_name"], "成员")
        self.assertEqual(body["message_id"], "wx-7")
        self.assertTrue(opened.call_args.args[0].full_url.endswith("/capabilities/v1/images/search"))

    def test_image_read_keeps_native_multimodal_result(self):
        envelope = {
            "_multimodal": True,
            "content": [
                {"type": "text", "text": "Image loaded"},
                {"type": "image_url", "image_url": {"url": "data:image/png;base64,UE5H"}},
            ],
            "text_summary": "attached",
        }
        with mock.patch.object(
            plugin._client, "read_current_image", return_value=(b"\x89PNG\r\n\x1a\n", "image/png")
        ) as read:
            with mock.patch.object(plugin._image_tools, "_vision", new=mock.AsyncMock(return_value=envelope)):
                result = __import__("asyncio").run(
                    plugin._image_tools._handle_read(
                        {"candidate_id": "img_opaque", "question": "这是什么？"},
                        task_id="task-image",
                    )
                )
        self.assertIs(result, envelope)
        read.assert_called_once()
        self.assertEqual(read.call_args.kwargs["question"], "这是什么？")

    def test_image_read_auxiliary_result_does_not_leak_data_url(self):
        with mock.patch.object(
            plugin._client, "read_current_image", return_value=(b"\x89PNG\r\n\x1a\n", "image/png")
        ):
            with mock.patch.object(
                plugin._image_tools,
                "_vision",
                new=mock.AsyncMock(
                    return_value=json.dumps({"success": True, "analysis": "一只猫"})
                ),
            ):
                result = __import__("asyncio").run(
                    plugin._image_tools._handle_read({"candidate_id": "img_opaque"})
                )
        self.assertEqual(json.loads(result), {"candidate_id": "img_opaque", "analysis": "一只猫"})
        self.assertNotIn("data:", result)

    def test_select_uses_only_candidate_and_current_context(self):
        response = _Response(
            {
                "staged": True,
                "effect_only_token": "[[GOLEM_EFFECT_ONLY:abc]]",
                "description": "无语",
            }
        )
        with mock.patch.object(plugin._opener, "open", return_value=response) as opened:
            result = json.loads(
                plugin._handle_select({"candidate_id": "candidate-2"})
            )

        self.assertTrue(result["staged"])
        self.assertEqual(result["effect_only_token"], "[[GOLEM_EFFECT_ONLY:abc]]")
        body = json.loads(opened.call_args.args[0].data.decode())
        self.assertEqual(set(body), {"candidate_id", "context"})
        self.assertEqual(body["candidate_id"], "candidate-2")

    def test_attach_in_main_relay_turn_stages_sticker(self):
        response = _Response(
            {
                "staged": True,
                "effect_only_token": "[[GOLEM_EFFECT_ONLY:abc]]",
                "description": "ok",
            }
        )
        with mock.patch.object(plugin._opener, "open", return_value=response):
            result = json.loads(
                plugin._handle_attach({"candidate_id": "candidate-2"})
            )

        self.assertTrue(result["staged"])
        self.assertEqual(result["effect_only_token"], "[[GOLEM_EFFECT_ONLY:abc]]")

    def test_non_relay_context_fails_closed(self):
        with mock.patch.object(
            plugin._client, "_session_value", side_effect=lambda name: "cli" if name.endswith("PLATFORM") else "x"
        ):
            with self.assertRaisesRegex(plugin.CapabilityError, "only in a Golem Relay"):
                plugin._current_context()

    def test_incomplete_relay_context_fails_closed(self):
        with mock.patch.object(
            plugin._client,
            "_session_value",
            side_effect=lambda name: "" if name.endswith("SESSION_KEY") else SESSION.get(name, ""),
        ):
            with self.assertRaisesRegex(plugin.CapabilityError, "context is unavailable"):
                plugin._current_context()

    def test_plain_http_remote_url_is_rejected(self):
        with mock.patch.dict(
            os.environ, {"GOLEM_CAPABILITIES_URL": "http://192.0.2.10:8789"}
        ):
            with self.assertRaisesRegex(plugin.CapabilityError, "restricted to a loopback"):
                plugin._base_url()

    def test_auth_error_does_not_echo_token_or_body(self):
        error = __import__("urllib.error").error.HTTPError(
            "http://127.0.0.1:8789/capabilities/v1/stickers/search",
            401,
            "secret=test-secret-not-a-real-key",
            {},
            None,
        )
        with mock.patch.object(plugin._opener, "open", side_effect=error):
            with self.assertRaises(plugin.CapabilityError) as caught:
                plugin._post("capabilities/v1/stickers/search", {})
        message = str(caught.exception)
        self.assertNotIn("test-secret", message)
        self.assertNotIn("secret=", message)

    def test_capability_token_survives_child_env_gap(self):
        original = plugin._client._cached_token
        plugin._client._cached_token = ""
        try:
            with mock.patch.dict(
                os.environ, {"GOLEM_CAPABILITIES_TOKEN": "cached-child-token"}
            ):
                plugin._client.cache_token_from_environment()
            with mock.patch.dict(os.environ, {"GOLEM_CAPABILITIES_TOKEN": ""}):
                self.assertTrue(plugin._check_available())
                self.assertEqual(plugin._client._token(), "cached-child-token")
        finally:
            plugin._client._cached_token = original

    def test_http_error_retry_classification(self):
        auth_error = __import__("urllib.error").error.HTTPError(
            "http://127.0.0.1:8789/capabilities/v1/async-delivery/status",
            401, "unauthorized", {}, None,
        )
        unavailable_error = __import__("urllib.error").error.HTTPError(
            "http://127.0.0.1:8789/capabilities/v1/async-delivery/status",
            503, "unavailable", {}, None,
        )
        with mock.patch.object(plugin._opener, "open", side_effect=auth_error):
            with self.assertRaises(plugin.CapabilityError) as caught:
                plugin._post("capabilities/v1/async-delivery/status", {})
        self.assertNotIsInstance(caught.exception, plugin._client.RetryableCapabilityError)
        with mock.patch.object(plugin._opener, "open", side_effect=unavailable_error):
            with self.assertRaises(plugin._client.RetryableCapabilityError):
                plugin._post("capabilities/v1/async-delivery/status", {})

    def test_conflict_includes_bounded_golem_error_detail(self):
        conflict = __import__("urllib.error").error.HTTPError(
            "http://127.0.0.1:8789/capabilities/v1/async-delivery/register",
            409,
            "conflict",
            {"Content-Type": "application/json; charset=utf-8"},
            io.BytesIO(
                json.dumps(
                    {"error": "message context does not match the active run"}
                ).encode()
            ),
        )
        with mock.patch.object(plugin._opener, "open", side_effect=conflict):
            with self.assertRaises(plugin.CapabilityError) as caught:
                plugin._post("capabilities/v1/async-delivery/register", {})
        self.assertIn("HTTP 409", str(caught.exception))
        self.assertIn(
            "message context does not match the active run", str(caught.exception)
        )

    def test_retryable_error_includes_bounded_golem_error_detail(self):
        unavailable = __import__("urllib.error").error.HTTPError(
            "http://127.0.0.1:8789/capabilities/v1/async-delivery/videos/search",
            502,
            "bad gateway",
            {"Content-Type": "application/json; charset=utf-8"},
            io.BytesIO(
                json.dumps(
                    {"error": "all video providers failed: upstream returned HTTP 520"}
                ).encode()
            ),
        )
        with mock.patch.object(plugin._opener, "open", side_effect=unavailable):
            with self.assertRaises(plugin._client.RetryableCapabilityError) as caught:
                plugin._post("capabilities/v1/async-delivery/videos/search", {})
        self.assertIn("HTTP 502", str(caught.exception))
        self.assertIn("upstream returned HTTP 520", str(caught.exception))

    def test_materialize_reads_only_controlled_media(self):
        response = _Response(b"GIF89a", "image/gif; charset=binary")
        with mock.patch.object(plugin._opener, "open", return_value=response) as opened:
            data, mime_type = plugin._materialize("candidate-3", plugin._current_context())

        self.assertEqual(data, b"GIF89a")
        self.assertEqual(mime_type, "image/gif")
        request = opened.call_args.args[0]
        body = json.loads(request.data.decode())
        self.assertEqual(set(body), {"candidate_id", "context"})
        self.assertNotIn("url", body)

    def test_inspect_uses_thread_and_forwards_task_id(self):
        caller_thread = threading.get_ident()

        def materialize(candidate_id, context):
            self.assertNotEqual(threading.get_ident(), caller_thread)
            self.assertEqual(candidate_id, "candidate-4")
            self.assertEqual(context["message_id"], "msg-1")
            return b"PNG", "image/png"

        analyze = mock.AsyncMock(
            return_value=json.dumps({"success": True, "analysis": "一只猫捂脸，表达尴尬"})
        )
        with mock.patch.object(plugin, "_materialize", side_effect=materialize):
            with mock.patch.object(plugin._vision, "_analyze", analyze):
                result = __import__("asyncio").run(
                    plugin._handle_inspect({"candidate_id": "candidate-4"}, task_id="task-9")
                )

        parsed = json.loads(result)
        self.assertEqual(parsed["candidate_id"], "candidate-4")
        self.assertNotIn("data:", result)
        image_url, task_id = analyze.await_args.args
        self.assertTrue(image_url.startswith("data:image/png;base64,"))
        self.assertEqual(task_id, "task-9")

    def test_inspect_exposes_vision_failure(self):
        analyze = mock.AsyncMock(return_value=json.dumps({"success": False, "error": "secret"}))
        with mock.patch.object(plugin, "_materialize", return_value=(b"PNG", "image/png")):
            with mock.patch.object(plugin._vision, "_analyze", analyze):
                with self.assertRaisesRegex(plugin.CapabilityError, "could not analyze"):
                    __import__("asyncio").run(
                        plugin._handle_inspect({"candidate_id": "candidate-5"})
                    )

    def test_register_marks_only_inspect_async(self):
        context = mock.Mock()
        plugin.register(context)
        registrations = {
            call.kwargs["name"]: call.kwargs for call in context.register_tool.call_args_list
        }
        self.assertEqual(
            set(registrations),
            {
                "golem_sticker_search",
                "golem_sticker_attach",
                "golem_sticker_inspect",
                "golem_sticker_select",
                "golem_image_search_current_session",
                "golem_image_read_current_session",
                "golem_video_search",
                "golem_video_attach",
                "golem_video_select",
                "golem_video_fetch",
            },
        )
        self.assertTrue(registrations["golem_sticker_inspect"]["is_async"])
        self.assertTrue(registrations["golem_image_read_current_session"]["is_async"])
        self.assertNotIn("is_async", registrations["golem_sticker_search"])
        self.assertNotIn("is_async", registrations["golem_image_search_current_session"])
        context.register_hook.assert_any_call(
            "pre_tool_call", plugin._async_runtime.pre_tool_call
        )
        context.register_hook.assert_any_call(
            "transform_llm_output", plugin._async_text.normalize_text
        )

    def test_register_can_disable_inspect(self):
        context = mock.Mock()
        with mock.patch.dict(
            os.environ, {"GOLEM_STICKER_INSPECT_ENABLED": "false"}
        ):
            plugin.register(context)

        registrations = {
            call.kwargs["name"]: call.kwargs
            for call in context.register_tool.call_args_list
        }
        self.assertEqual(
            set(registrations),
            {
                "golem_sticker_search",
                "golem_sticker_attach",
                "golem_sticker_select",
                "golem_image_search_current_session",
                "golem_image_read_current_session",
                "golem_video_search",
                "golem_video_attach",
                "golem_video_select",
                "golem_video_fetch",
            },
        )

    def test_register_rejects_invalid_inspect_setting(self):
        context = mock.Mock()
        with mock.patch.dict(
            os.environ, {"GOLEM_STICKER_INSPECT_ENABLED": "sometimes"}
        ):
            with self.assertRaisesRegex(
                plugin.CapabilityError, "must be true or false"
            ):
                plugin.register(context)
        context.register_tool.assert_not_called()

    def test_register_adds_async_attach_tool_when_enabled(self):
        context = mock.Mock()
        with mock.patch.dict(os.environ, {"GOLEM_ASYNC_DELIVERY_ENABLED": "true"}):
            with mock.patch.object(plugin._async_runtime, "install"):
                with mock.patch.object(plugin._cron_delivery, "install"):
                    with mock.patch.object(plugin._async_api, "delivery_profiles", return_value=()):
                        plugin.register(context)

        registrations = {
            call.kwargs["name"]: call.kwargs
            for call in context.register_tool.call_args_list
        }
        self.assertIn("golem_sticker_attach", registrations)
        self.assertEqual(
            registrations["golem_sticker_attach"]["handler"],
            plugin._handle_attach,
        )
        context.register_hook.assert_any_call(
            "on_gateway_startup", plugin._async_runtime.gateway_startup
        )


if __name__ == "__main__":
    unittest.main()
