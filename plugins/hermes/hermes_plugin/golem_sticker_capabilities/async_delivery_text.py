"""Keep asynchronous completion output on the text-only delivery rail."""

from __future__ import annotations

import os
import re


_MARKDOWN_IMAGE = re.compile(
    r'!\[([^]\n]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)'
)
_HTML_IMAGE = re.compile(r"<img\b[^>]*>", re.IGNORECASE)
_DELIVERY_MARKER = re.compile(
    r"\[\[(?:audio_as_voice|as_document)\]\]", re.IGNORECASE
)
_MEDIA_DIRECTIVE = re.compile(
    r"(?im)^\s*(?:[`\"']?MEDIA:)\s*[^\r\n]+$"
)
_MEDIA_INLINE = re.compile(
    r"(?i)[`\"']?MEDIA:\s*(?:`[^`\r\n]+`|\"[^\"\r\n]+\"|'[^'\r\n]+'|[^\s\r\n]+)"
)
_MEDIA_EXTENSIONS = (
    "png", "jpg", "jpeg", "gif", "webp", "bmp", "tiff", "svg",
    "mp4", "mov", "avi", "mkv", "webm",
    "mp3", "wav", "ogg", "opus", "m4a", "flac",
    "pdf", "docx", "doc", "odt", "rtf", "txt", "md", "epub",
    "xlsx", "xls", "ods", "csv", "tsv", "json", "xml", "yaml", "yml",
    "pptx", "ppt", "odp", "key",
    "zip", "tar", "gz", "tgz", "bz2", "xz", "7z", "rar", "apk", "ipa",
    "html", "htm",
)
_MEDIA_EXTENSION_PATTERN = "|".join(
    sorted((re.escape(value) for value in _MEDIA_EXTENSIONS), key=len, reverse=True)
)
_LOCAL_MEDIA = re.compile(
    r"(?<!\w)(?:[A-Za-z]:[\\/]|/|~/)[^\s`\"'<>]+"
    rf"\.(?:{_MEDIA_EXTENSION_PATTERN})\b",
    re.IGNORECASE,
)


def normalize_text(response_text: str, **_: object) -> str | None:
    from .async_delivery_state import current_delivery

    if current_delivery.get() is None and _session_trigger_kind() != "ambient":
        return None
    return sanitize_text(response_text)


def _session_trigger_kind() -> str:
    try:
        from gateway.session_context import get_session_trigger_kind

        return get_session_trigger_kind()
    except (ImportError, AttributeError):
        return ""


def sanitize_text(response_text: str) -> str:
    value = _MARKDOWN_IMAGE.sub(_replace_markdown_image, response_text)
    value = _HTML_IMAGE.sub("[image omitted: async delivery is text-only]", value)
    value = _DELIVERY_MARKER.sub("", value)
    value = _MEDIA_DIRECTIVE.sub(_replace_media_directive, value)
    value = _MEDIA_INLINE.sub(_replace_media_directive, value)
    value = _LOCAL_MEDIA.sub(_replace_local_media, value)
    return value


def _replace_markdown_image(match: re.Match[str]) -> str:
    label = match.group(1).strip() or "image"
    return f"{label} ({match.group(2).strip()})"


def _replace_media_directive(match: re.Match[str]) -> str:
    raw = match.group(0).strip()
    path = raw.split(":", 1)[-1].strip(" `\"'")
    return f"[attachment omitted: {os.path.basename(path) or 'media'}]"


def _replace_local_media(match: re.Match[str]) -> str:
    return f"[local attachment omitted: {os.path.basename(match.group(0))}]"
