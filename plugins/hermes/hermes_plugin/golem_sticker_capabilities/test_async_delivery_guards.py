from __future__ import annotations

import unittest
from unittest import mock

import test_async_delivery as shared


class AsyncDeliveryGuardTests(unittest.TestCase):
    def test_async_turn_blocks_nested_and_sticker_tools(self):
        token = shared.state.current_delivery.set(shared.binding())
        try:
            for tool_name in (
                "delegate_task",
                "golem_sticker_collect_current_session",
                "golem_sticker_library_manage_recent",
                "golem_silence_rule_add",
                "golem_sticker_library_inventory",
                "golem_sticker_library_pick",
                "golem_sticker_library_preview",
                "golem_sticker_library_search",
                "golem_sticker_select",
                "golem_sticker_select_many",
            ):
                directive = shared.runtime.pre_tool_call(tool_name=tool_name)
                self.assertEqual(directive["action"], "block")
            self.assertIsNone(
                shared.runtime.pre_tool_call(tool_name="golem_sticker_search")
            )
            self.assertIsNone(
                shared.runtime.pre_tool_call(tool_name="golem_sticker_attach")
            )
        finally:
            shared.state.current_delivery.reset(token)
        self.assertIsNone(shared.runtime.pre_tool_call(tool_name="delegate_task"))

    def test_ambient_turn_blocks_delegation_and_all_golem_media_tools(self):
        with mock.patch.object(shared.runtime, "_session_trigger_kind", return_value="ambient"):
            for tool_name in (
                "delegate_task",
                "golem_sticker_collect_current_session",
                "golem_sticker_library_manage_recent",
                "golem_silence_rule_add",
                "golem_sticker_library_inventory",
                "golem_sticker_library_pick",
                "golem_sticker_library_preview",
                "golem_sticker_library_search",
                "golem_sticker_search",
                "golem_sticker_inspect",
                "golem_sticker_attach",
                "golem_sticker_select",
                "golem_sticker_select_many",
                "golem_video_search",
                "golem_video_select",
                "golem_video_attach",
                "golem_video_fetch",
            ):
                directive = shared.runtime.pre_tool_call(tool_name=tool_name)
                self.assertEqual(directive["action"], "block", tool_name)
            self.assertIsNone(shared.runtime.pre_tool_call(tool_name="web_search"))
            self.assertIsNone(shared.runtime.pre_tool_call(tool_name="web_extract"))

    def test_explicit_turn_keeps_full_interactive_capabilities(self):
        with mock.patch.object(shared.runtime, "_session_trigger_kind", return_value="explicit"):
            for tool_name in (
                "delegate_task",
                "golem_sticker_collect_current_session",
                "golem_sticker_library_manage_recent",
                "golem_silence_rule_add",
                "golem_sticker_library_inventory",
                "golem_sticker_library_pick",
                "golem_sticker_library_preview",
                "golem_sticker_library_search",
                "golem_sticker_select",
                "golem_sticker_select_many",
                "golem_video_fetch",
            ):
                self.assertIsNone(shared.runtime.pre_tool_call(tool_name=tool_name))

    def test_ambient_output_filter_removes_attachment_syntax(self):
        with mock.patch.object(shared.text_filter, "_session_trigger_kind", return_value="ambient"):
            value = shared.text_filter.normalize_text(
                "文字回复\nMEDIA:/tmp/video.mp4\n![图](https://example.com/a.png)"
            )
        self.assertNotIn("MEDIA:", value)
        self.assertNotIn("![", value)
        self.assertIn("文字回复", value)

    def test_text_filter_removes_media_delivery_syntax(self):
        token = shared.state.current_delivery.set(shared.binding())
        try:
            value = shared.text_filter.normalize_text(
                "![image](https://example.com/a.png)\nMEDIA:/tmp/Caddyfile\n"
                "/tmp/report.docx /tmp/data.xlsx /tmp/archive.zip /tmp/page.html\n"
                "[[as_document]] [[audio_as_voice]]"
            )
        finally:
            shared.state.current_delivery.reset(token)
        self.assertNotIn("![", value)
        self.assertNotIn("MEDIA:", value)
        forbidden = (
            "/tmp/report.docx", "/tmp/data.xlsx", "/tmp/archive.zip",
            "/tmp/page.html", "[[as_document]]", "[[audio_as_voice]]",
        )
        for item in forbidden:
            self.assertNotIn(item, value)

    def test_version_mismatch_fails_plugin_install(self):
        with mock.patch.object(
            shared.runtime.importlib.metadata, "version", return_value="0.18.3"
        ):
            with self.assertRaisesRegex(RuntimeError, "requires hermes-agent==0.18.2"):
                shared.runtime._validate_version()


if __name__ == "__main__":
    unittest.main()
