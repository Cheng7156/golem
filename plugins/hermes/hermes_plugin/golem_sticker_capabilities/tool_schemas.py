"""Tool schemas for Golem sticker capabilities."""

from __future__ import annotations

SEARCH_SCHEMA = {
    "name": "golem_sticker_search",
    "description": (
        "Optionally search for WeChat sticker candidates when a sticker would "
        "make the current reply more natural, expressive, or aligned with your "
        "personality. Search results are short-lived opaque candidates bound "
        "to the current Golem Relay conversation or async delivery ticket."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "description": "A concise Chinese emotion, reaction, or meme keyword.",
                "minLength": 1,
                "maxLength": 80,
            },
            "limit": {
                "type": "integer",
                "description": "Maximum candidates to return (1-10).",
                "minimum": 1,
                "maximum": 10,
                "default": 5,
            },
        },
        "required": ["query"],
        "additionalProperties": False,
    },
}

SELECT_SCHEMA = {
    "name": "golem_sticker_select",
    "description": (
        "Stage one previously searched sticker for the current WeChat reply. "
        "Use this only in the main Relay turn. After selecting a sticker, "
        "reply normally to add text, or return the effect_only_token for a "
        "sticker-only reply."
    ),
    "parameters": {
        "type": "object",
        "properties": {
            "candidate_id": {
                "type": "string",
                "description": (
                    "Opaque candidate id returned by golem_sticker_search. Do "
                    "not construct, alter, or reuse it in another conversation."
                ),
                "minLength": 1,
                "maxLength": 2048,
            }
        },
        "required": ["candidate_id"],
        "additionalProperties": False,
    },
}

ATTACH_SCHEMA = {
    "name": "golem_sticker_attach",
    "description": (
        "Attach one previously searched sticker to the current Golem reply. "
        "In a normal Relay turn this stages the sticker like golem_sticker_select. "
        "In a background child it queues the sticker directly in Golem's "
        "durable WeChat Outbox; do not write JSON or media data yourself."
    ),
    "parameters": SELECT_SCHEMA["parameters"],
}
