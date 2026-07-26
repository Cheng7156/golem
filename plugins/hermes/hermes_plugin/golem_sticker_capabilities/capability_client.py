"""Restricted HTTP client for Golem's Relay capability endpoints."""

from __future__ import annotations

import ipaddress
import json
import os
import socket
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, NoReturn, Tuple

from .errors import CapabilityError, RetryableCapabilityError


DEFAULT_BASE_URL = "http://127.0.0.1:8789"
SEARCH_PATH = "capabilities/v1/stickers/search"
MATERIALIZE_PATH = "capabilities/v1/stickers/materialize"
SELECT_PATH = "capabilities/v1/stickers/select"
SELECT_MANY_PATH = "capabilities/v1/stickers/select-many"
STICKER_LIBRARY_SEARCH_PATH = "capabilities/v1/stickers/library/search"
STICKER_LIBRARY_INVENTORY_PATH = "capabilities/v1/stickers/library/inventory"
STICKER_LIBRARY_COLLECT_PATH = "capabilities/v1/stickers/library/collect"
IMAGE_SEARCH_PATH = "capabilities/v1/images/search"
IMAGE_READ_PATH = "capabilities/v1/images/read"
VIDEO_SEARCH_PATH = "capabilities/v1/videos/search"
VIDEO_SELECT_PATH = "capabilities/v1/videos/select"
VIDEO_STATUS_PATH = "capabilities/v1/videos/status"
VIDEO_FETCH_PATH = "capabilities/v1/videos/fetch"
ASYNC_REGISTER_PATH = "capabilities/v1/async-delivery/register"
ASYNC_STATUS_PATH = "capabilities/v1/async-delivery/status"
ASYNC_DELIVER_PATH = "capabilities/v1/async-delivery/deliver"
ASYNC_DELIVER_V2_PATH = "capabilities/v1/async-delivery/deliver-v2"
ASYNC_STICKER_SEARCH_PATH = "capabilities/v1/async-delivery/stickers/search"
ASYNC_STICKER_SELECT_PATH = "capabilities/v1/async-delivery/stickers/select"
ASYNC_STICKER_SEND_PATH = "capabilities/v1/async-delivery/stickers/send"
ASYNC_VIDEO_SEARCH_PATH = "capabilities/v1/async-delivery/videos/search"
ASYNC_VIDEO_INSPECT_PATH = "capabilities/v1/async-delivery/videos/inspect"
ASYNC_VIDEO_SEND_PATH = "capabilities/v1/async-delivery/videos/send"
ASYNC_VIDEO_SEND_URL_PATH = "capabilities/v1/async-delivery/videos/send-url"
ASYNC_VIDEO_STATUS_PATH = "capabilities/v1/async-delivery/videos/status"
ASYNC_REVOKE_PATH = "capabilities/v1/async-delivery/revoke"
ASYNC_RECONCILE_PATH = "capabilities/v1/async-delivery/reconcile"
CRON_REGISTER_PATH = "capabilities/v1/cron-delivery/register"
CRON_DELIVER_PATH = "capabilities/v1/cron-delivery/deliver"
CRON_STATUS_PATH = "capabilities/v1/cron-delivery/status"
CRON_VIDEO_SEARCH_PATH = "capabilities/v1/cron-delivery/videos/search"
CRON_VIDEO_SEND_PATH = "capabilities/v1/cron-delivery/videos/send"
CRON_VIDEO_STATUS_PATH = "capabilities/v1/cron-delivery/videos/status"
MAX_RESPONSE_BYTES = 1 << 20
MAX_DELIVERY_RESPONSE_BYTES = 12 << 20
MAX_MEDIA_BYTES = 8 << 20
MAX_ERROR_RESPONSE_BYTES = 16 << 10
SUPPORTED_MEDIA_TYPES = {
    "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp",
}

_CONTEXT_ENV = {
    "platform": "HERMES_SESSION_PLATFORM",
    "chat_id": "HERMES_SESSION_CHAT_ID",
    "thread_id": "HERMES_SESSION_THREAD_ID",
    "user_id": "HERMES_SESSION_USER_ID",
    "session_key": "HERMES_SESSION_KEY",
    "session_id": "HERMES_SESSION_ID",
    "message_id": "HERMES_SESSION_MESSAGE_ID",
    "profile": "HERMES_SESSION_PROFILE",
}


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRedirect())
_cached_token = ""


def _session_value(name: str) -> str:
    from gateway.session_context import get_session_env  # pyright: ignore[reportMissingImports]

    return str(get_session_env(name, "") or "").strip()


def current_context() -> Dict[str, str]:
    context = {key: _session_value(env_name) for key, env_name in _CONTEXT_ENV.items()}
    if context["platform"].lower() != "relay":
        raise CapabilityError("Golem capability tools are available only in a Golem Relay turn")
    required = ("chat_id", "session_key", "user_id", "message_id")
    if any(not context[key] for key in required):
        raise CapabilityError("Current Golem Relay chat context is unavailable")
    context["platform"] = "relay"
    return context


def _is_loopback(hostname: str) -> bool:
    if hostname.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(hostname).is_loopback
    except ValueError:
        return False


def base_url() -> str:
    raw = os.getenv("GOLEM_CAPABILITIES_URL", DEFAULT_BASE_URL).strip()
    raw = raw or DEFAULT_BASE_URL
    parsed = urllib.parse.urlsplit(raw)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise CapabilityError("GOLEM_CAPABILITIES_URL must be an http(s) URL")
    try:
        parsed.port
    except ValueError as exc:
        raise CapabilityError("GOLEM_CAPABILITIES_URL contains an invalid port") from exc
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise CapabilityError(
            "GOLEM_CAPABILITIES_URL must not contain credentials, a query, or a fragment"
        )
    if parsed.scheme == "http" and not _is_loopback(parsed.hostname):
        raise CapabilityError(
            "Plain HTTP capability access is restricted to a loopback address; use HTTPS remotely"
        )
    return raw.rstrip("/")


def _token() -> str:
    global _cached_token
    value = os.getenv("GOLEM_CAPABILITIES_TOKEN", "").strip() or _cached_token
    if not value:
        raise CapabilityError("GOLEM_CAPABILITIES_TOKEN is not configured")
    if "\r" in value or "\n" in value:
        raise CapabilityError("GOLEM_CAPABILITIES_TOKEN is invalid")
    _cached_token = value
    return value


def cache_token_from_environment() -> None:
    global _cached_token
    value = os.getenv("GOLEM_CAPABILITIES_TOKEN", "").strip()
    if not value:
        return
    if "\r" in value or "\n" in value:
        raise CapabilityError("GOLEM_CAPABILITIES_TOKEN is invalid")
    _cached_token = value


def token_configured() -> bool:
    return bool(os.getenv("GOLEM_CAPABILITIES_TOKEN", "").strip() or _cached_token)


def _timeout_seconds() -> float:
    raw = os.getenv("GOLEM_CAPABILITIES_TIMEOUT_SECONDS", "10").strip()
    try:
        value = float(raw)
    except ValueError as exc:
        raise CapabilityError("GOLEM_CAPABILITIES_TIMEOUT_SECONDS is invalid") from exc
    if value <= 0 or value > 60:
        raise CapabilityError("GOLEM_CAPABILITIES_TIMEOUT_SECONDS must be between 0 and 60")
    return value


def _request(
    path: str,
    payload: Dict[str, Any],
    *,
    accept: str,
    maximum: int,
) -> Tuple[bytes, str]:
    body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(
        f"{base_url()}/{path.lstrip('/')}",
        data=body,
        method="POST",
        headers={
            "Accept": accept,
            "Authorization": f"Bearer {_token()}",
            "Content-Type": "application/json; charset=utf-8",
            "User-Agent": "golem-hermes-sticker-capabilities/1.1",
        },
    )
    raw, content_type = _open(request, maximum)
    return raw, content_type.partition(";")[0].strip().lower()


def _open(request: urllib.request.Request, maximum: int) -> Tuple[bytes, str]:
    try:
        with _opener.open(request, timeout=_timeout_seconds()) as response:
            raw = response.read(maximum + 1)
            content_type = response.headers.get("Content-Type", "")
    except urllib.error.HTTPError as exc:
        detail = _read_http_error_detail(exc)
        exc.close()
        _raise_http_error(exc, detail)
    except (urllib.error.URLError, TimeoutError, socket.timeout, OSError):
        raise RetryableCapabilityError(
            "Golem capability API is unavailable or timed out"
        ) from None
    if len(raw) > maximum:
        raise CapabilityError("Golem capability API response is too large")
    return raw, content_type


def _read_http_error_detail(error: urllib.error.HTTPError) -> str:
    content_type = str(error.headers.get("Content-Type", "") or "")
    if content_type.partition(";")[0].strip().lower() != "application/json":
        return ""
    try:
        raw = error.read(MAX_ERROR_RESPONSE_BYTES + 1)
    except (OSError, ValueError):
        return ""
    if len(raw) > MAX_ERROR_RESPONSE_BYTES:
        return ""
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return ""
    detail = payload.get("error") if isinstance(payload, dict) else None
    if not isinstance(detail, str):
        return ""
    detail = " ".join(detail.split())
    return detail if 0 < len(detail) <= 512 else ""


def _raise_http_error(error: urllib.error.HTTPError, detail: str = "") -> NoReturn:
    if error.code == 507:
        raise CapabilityError(
            "Golem sticker library storage is full; increase its configured budget"
        ) from None
    if error.code in {408, 425, 429} or 500 <= error.code < 600:
        suffix = f": {detail}" if detail else ""
        raise RetryableCapabilityError(
            f"Golem capability API is temporarily unavailable (HTTP {error.code}){suffix}"
        ) from None
    if error.code == 401:
        raise CapabilityError(
            "Golem capability authentication failed; check GOLEM_CAPABILITIES_TOKEN"
        ) from None
    if error.code == 403:
        suffix = f": {detail}" if detail else ""
        raise CapabilityError(
            f"Golem capability request is forbidden{suffix}"
        ) from None
    if 300 <= error.code < 400:
        raise CapabilityError("Golem capability API redirects are not allowed") from None
    suffix = f": {detail}" if detail else ""
    raise CapabilityError(
        f"Golem capability API rejected the request (HTTP {error.code}){suffix}"
    ) from None


def post_json(path: str, payload: Dict[str, Any]) -> Dict[str, Any]:
    return post_json_limited(path, payload, MAX_RESPONSE_BYTES)


def post_json_limited(path: str, payload: Dict[str, Any], maximum: int) -> Dict[str, Any]:
    raw, _ = _request(
        path, payload, accept="application/json", maximum=maximum,
    )
    try:
        result = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise CapabilityError("Golem capability API returned invalid JSON") from None
    if not isinstance(result, dict):
        raise CapabilityError("Golem capability API returned an invalid response object")
    return result


def materialize(candidate_id: str, context: Dict[str, str]) -> Tuple[bytes, str]:
    raw, content_type = _request(
        MATERIALIZE_PATH,
        {"candidate_id": candidate_id, "context": context},
        accept="image/jpeg,image/png,image/gif,image/webp,image/bmp",
        maximum=MAX_MEDIA_BYTES,
    )
    if content_type not in SUPPORTED_MEDIA_TYPES:
        raise CapabilityError("Golem capability API returned unsupported sticker media")
    if not raw:
        raise CapabilityError("Golem capability API returned empty sticker media")
    return raw, content_type


def select_stickers(
    candidate_ids: list[str],
    context: Dict[str, str],
) -> Dict[str, Any]:
    return post_json_limited(
        SELECT_MANY_PATH,
        {"candidate_ids": candidate_ids, "context": context},
        MAX_RESPONSE_BYTES,
    )


def search_current_images(
    context: Dict[str, str],
    *,
    speaker_id: str = "",
    speaker_name: str = "",
    message_id: str = "",
    limit: int = 8,
) -> Dict[str, Any]:
    """Return metadata candidates without downloading any image bytes."""
    payload: Dict[str, Any] = {"limit": limit, "context": context}
    if speaker_id:
        payload["speaker_id"] = speaker_id
    if speaker_name:
        payload["speaker_name"] = speaker_name
    if message_id:
        payload["message_id"] = message_id
    return post_json_limited(IMAGE_SEARCH_PATH, payload, MAX_RESPONSE_BYTES)


def search_sticker_library(
    query: str,
    limit: int,
    context: Dict[str, str],
) -> Dict[str, Any]:
    return post_json_limited(
        STICKER_LIBRARY_SEARCH_PATH,
        {"query": query, "limit": limit, "context": context},
        MAX_RESPONSE_BYTES,
    )


def sticker_library_inventory(
    limit: int,
    offset: int,
    context: Dict[str, str],
) -> Dict[str, Any]:
    return post_json_limited(
        STICKER_LIBRARY_INVENTORY_PATH,
        {"limit": limit, "offset": offset, "context": context},
        MAX_RESPONSE_BYTES,
    )


def collect_current_sticker(
    candidate_id: str,
    description: str,
    context: Dict[str, str],
) -> Dict[str, Any]:
    return post_json_limited(
        STICKER_LIBRARY_COLLECT_PATH,
        {
            "candidate_id": candidate_id,
            "description": description,
            "context": context,
        },
        MAX_RESPONSE_BYTES,
    )


def read_current_image(
    candidate_id: str,
    context: Dict[str, str],
    *,
    question: str = "",
) -> Tuple[bytes, str]:
    """Materialize one run-scoped image candidate on explicit request."""
    payload: Dict[str, Any] = {
        "candidate_id": candidate_id,
        "context": context,
    }
    if question:
        payload["question"] = question
    raw, content_type = _request(
        IMAGE_READ_PATH,
        payload,
        accept="image/jpeg,image/png,image/gif,image/webp,image/bmp",
        maximum=MAX_MEDIA_BYTES,
    )
    if content_type not in SUPPORTED_MEDIA_TYPES:
        raise CapabilityError("Golem capability API returned unsupported image media")
    if not raw:
        raise CapabilityError("Golem capability API returned empty image media")
    return raw, content_type
