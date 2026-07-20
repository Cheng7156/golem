from __future__ import annotations

import unittest
from unittest import mock

import test_async_delivery as shared


class AsyncDeliveryGuardTests(unittest.TestCase):
    def test_async_turn_blocks_nested_and_sticker_tools(self):
        token = shared.state.current_delivery.set(shared.binding())
        try:
            for tool_name in ("delegate_task", "golem_sticker_select"):
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
