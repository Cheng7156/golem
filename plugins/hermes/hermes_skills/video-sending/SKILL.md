---
name: video-sending
description: Send a requested video into the current WeChat conversation through Golem. Use when a user supplies a public HTTP(S) URL or asks Hermes to find, send, or post a configured beauty, black-stockings, or white-stockings video. Do not use for creating scheduled video jobs; route those requests to the dedicated Golem video cron skill.
---

# Send A WeChat Video

Use a background subagent because URL inspection, JSON selection, download, preparation, and Outbox delivery may take time.

## Acknowledge The Delegation

Once `delegate_task` accepts the task, end the current turn immediately with one short natural-language acknowledgement such as `活已经派下去了，我找到后直接发到这里。` Do not wait for the background result before acknowledging it, and do not call `send_message` just to acknowledge.

## User-Supplied URL

When the user gives an `http://` or `https://` URL and asks Hermes to send its video, delegate the whole operation. The child must call `golem_video_fetch(url=<exact user URL>)`. If the result is `status="needs_selection"`, choose only an exact `candidates[].url` or an exact URL visible in the redacted `document`, then call `golem_video_fetch` again with the original `url` and that `media_url`. Never use shell/curl, invent a URL, pass a local path, or send a URL directly through another tool. This URL route is complete by itself; do not also run the configured-provider flow below.

## Select The Provider

Choose only a provider configured in the deployed Golem plugin:

| User intent | category | provider_id |
|---|---|---|
| Generic beauty video, 美女视频, 小姐姐, no subtype | `xjj` | `xjj_stream` |
| Black stockings, 黑丝 | `heisi` | `yujn_heisi` |
| White stockings, 白丝 | `baisi` | `yujn_baisi` |

For a generic request, select `xjj_stream` without asking a follow-up question. Use only the three Provider IDs in this table. Never invent Provider IDs, URLs, local paths, or candidate IDs.

## Delegate The Delivery

Call `delegate_task` with a non-empty string `goal`. Include these exact steps in the goal:

1. Call `golem_video_search` once with the selected `category`, `provider_id`, `query=""`, and `limit=1`.
2. Take the real `candidate_id` returned by that call.
3. Call `golem_video_attach(candidate_id=<returned ID>)` exactly once.
4. Report `queued`, `kind`, `outbox_id`, and `sequence`. Preserve the complete capability error on failure.

Use `context` only for supporting constraints. Do not put the task itself exclusively in `context`; `delegate_task` rejects calls without `goal` or `tasks`.

The runtime injects the delegation ticket, chat identity, receiver identity, invocation ID, and tool call ID. Do not pass or fabricate those fields. Do not call `golem_video_select` or `send_message`.

After delegation succeeds, say only that the video was queued for this conversation. Do not claim delivery if `queued` is not `true`.
