# Golem Sticker Capabilities for Hermes

This is a Hermes Agent v0.18.2 user plugin. It provides optional sticker and
video tools plus a durable completion rail for `delegate_task(background=true)`.
Async results still enter Golem's Transactional Outbox and are sent by
`message.send`.

The plugin never accepts a chat id, wxId, receiver, URL, or local path from the
model. Sticker and video tools accept only a search query or opaque candidate
id. Video candidates come from configured Golem Provider APIs; Golem validates
and downloads them inside the active run. The plugin injects Hermes' task-local
Gateway context, and Golem validates it against the active run before accepting
a selection.

## Install

Copy this complete directory to the Hermes user-plugin directory:

```bash
mkdir -p /root/.hermes/plugins
cp -a golem_sticker_capabilities \
  /root/.hermes/plugins/golem_sticker_capabilities
```

Generate a dedicated secret. Do not reuse the model API key or commit the
secret to Git:

```bash
openssl rand -hex 32
```

Put the token and provider credentials in the file referenced by Golem's
`capabilities.environment_file`. It is read by `/pm load hermes` without
restarting the Golem Host and its contents are not shown by `/pm info hermes`.
Put the same capability token in `/root/.hermes/.env` for Hermes:

```dotenv
GOLEM_CAPABILITIES_URL=http://127.0.0.1:8789
GOLEM_CAPABILITIES_TOKEN=replace-with-the-generated-secret
GOLEM_CAPABILITIES_TIMEOUT_SECONDS=10
GOLEM_STICKER_INSPECT_ENABLED=false
GOLEM_ASYNC_DELIVERY_ENABLED=true
GOLEM_VIDEO_ENABLED=true
GOLEM_VIDEO_PREPARE_TIMEOUT_SECONDS=210
```

`GOLEM_STICKER_INSPECT_ENABLED` defaults to `true`. Set it to `false` to omit
`golem_sticker_inspect` entirely and use only provider descriptions for blind
search and selection. The existing `auxiliary.vision` configuration may remain;
it is not called while the inspection tool is disabled.

`GOLEM_CAPABILITIES_URL` may include or omit a trailing slash. Plain HTTP is
accepted only for a loopback host. A remote deployment must use HTTPS so the
bearer token is not sent in clear text. The client deliberately ignores
`HTTP_PROXY` and `HTTPS_PROXY` for this control-plane request so the dedicated
token cannot leak through an unrelated network proxy.

Enable the plugin and expose only its toolset to Relay in
`/root/.hermes/config.yaml`:

```yaml
plugins:
  enabled:
    - golem-sticker-capabilities

auxiliary:
  vision:
    provider: auto
    model: "<vision-model-id>"
    base_url: "<optional-openai-compatible-url>"
    timeout: 120
    download_timeout: 30

platform_toolsets:
  relay:
    - delegation
    - cronjob
    - web
    - golem_stickers
    - no_mcp
```

Restart Hermes Gateway after installing or changing environment variables.
The main conversation model does not need native vision support. Configure the
visual model and credentials through Hermes; Golem never receives model keys.

## Agent behavior

The tools are optional by design. Their descriptions tell Hermes to decide
whether a sticker is natural for the current reply. The Agent's system prompt
or personality can therefore tune frequency without changing this plugin, for
example:

```text
Use stickers occasionally when they convey the reaction better than text.
Prefer plain text for serious, technical, or sensitive discussions, and avoid
using a sticker mechanically on every turn.
```

The flow is:

1. `golem_sticker_search(query, limit)` returns short-lived opaque candidates.
2. When enabled, `golem_sticker_inspect(candidate_id)` sends Golem-validated
   candidate bytes to Hermes `auxiliary.vision` and returns a short textual
   analysis. When disabled, Hermes selects from the search descriptions alone.
3. `golem_sticker_select(candidate_id)` stages the exact cached candidate bytes.
4. For a sticker-only reply, Hermes returns the exact `effect_only_token` from
   the selection result. For text plus sticker, it returns ordinary final text.

Provider API keys, provider response parsing, downloads, media validation, and
WeChat delivery remain in Golem. Replacing the remote sticker search service
does not require changing this Hermes plugin.

## Video replies

Video tools are hidden unless `GOLEM_VIDEO_ENABLED=true`. They are strictly
run-bound and their descriptions require an explicit user request for video.

1. `golem_video_search(category, query?, limit?)` searches configured API
   providers without exposing provider credentials or media locators.
2. `golem_video_select(candidate_id)` starts a Golem preparation job and polls
   it without holding one long HTTP request. Call it repeatedly for multiple
   ordered videos.
3. `golem_video_attach(candidate_id)` uses the same candidate in a background
   child, waits for Golem preparation, and returns only after a durable video
   Outbox has been queued.
4. `golem_video_fetch(url, media_url?)` accepts a normal addressed/private turn
   and immediately creates a durable background job; that path does not need a
   child delegation. A background child may still use it for an existing
   durable delivery ticket. Direct video, text-URL, and unambiguous JSON
   responses are selected automatically; only ambiguous JSON returns a
   redacted bounded document and ranked candidates for one exact `media_url`
   selection.

Golem performs public-IP/redirect checks, download limits, ffprobe validation, optional ffmpeg conversion,
thumbnail generation, immutable local storage, Transactional Outbox reference
tracking, and `message.TypeVideo` delivery. See `VIDEO_CAPABILITY.md` in the
Golem Hermes plugin source for Provider TOML examples and deployment checks.
Configured-provider URLs and media bytes never appear in the normal search tools.
The URL fetch tool returns only the user-supplied source URL and the bounded
JSON values needed for the child Agent to choose a media URL; credentials in
JSON fields are redacted.

### Trigger-scoped authority

The model-visible toolset remains stable for prompt caching, but execution is
authorized from the connector-bound trigger class. Private, explicitly
addressed, quoted, and control turns retain normal sticker, video, and
delegation capabilities. An unaddressed ambient `respond` turn may produce text
and use read-only web tools, but `delegate_task` and every `golem_sticker_*` or
`golem_video_*` call are blocked. Attachment syntax in its final text is also
removed. The trigger class is task-local and has no environment-variable or
model-argument fallback; Golem independently enforces the same ambient media
denial against the active Run before provider access or durable job creation.

## Background delegation

Enable `hermes.config.agent.async_delivery_enabled = true` in Golem and
`GOLEM_ASYNC_DELIVERY_ENABLED=true` in Hermes. The plugin registers a ticket
while the parent Golem Run is active. A real Hermes async completion later
commits a synthetic audit chain and text Outbox item through the capability API.
Only the complete final response can consume the ticket. Streaming fragments,
tool progress, and ordinary Relay sends from the synthetic turn are rejected.
When the same session is busy, completion events wait in a separate FIFO until
ordinary pending user turns drain; their text and delivery binding are never
merged into a user event.
Messages arriving while a completion turn runs are likewise deferred and start
only after that turn commits, with no delivery binding in their context.
`/stop`, `/new`, and `/reset` revoke the old session's tickets. The bridge is
version locked: any Hermes version other than `0.18.2` fails plugin loading
instead of silently using an unverified private API.

### Direct child sticker delivery

Background children use `golem_sticker_search` followed by
`golem_sticker_attach`. The attach handler submits the selected candidate to
Golem with the ticket binding and the captured Hermes tool invocation ID. Golem
selects the media, creates a durable Outbox row, and returns `queued=true`.
Repeated execution of the same invocation returns the same Outbox row; separate
tool calls receive separate session sequence numbers.

The plugin captures Hermes' stable runtime `tool_call_id` from the
`tool_execution` middleware and exposes it to the handler through a private
`ContextVar`. It never comes from model arguments. Missing IDs fail explicitly.
When at least one direct output was queued,
the background completion closes the ticket silently so its internal summary
is not sent as another WeChat message. Batch children currently share one
delivery ticket and therefore have partial-success semantics; their independent
invocation IDs preserve idempotency and database-assigned output ordering.

Already queued Outbox rows remain durable after ticket revocation. Revocation
blocks new tool sends but does not retract a send that the tool already reported
as queued.

## Cron delivery

The same `async_delivery_enabled` and `GOLEM_ASYNC_DELIVERY_ENABLED` settings
also enable durable cron delivery for jobs created from a Golem Relay chat.
The Relay platform toolset must include `cronjob` so the Agent can create and
manage those jobs from WeChat.
If Hermes uses platform toolset allowlists, the `cron` platform must include
`golem_stickers` so scheduled agents can see the video tools:

```yaml
platform_toolsets:
  cron:
    - golem_stickers
```

Golem-bound cron runs can call `golem_video_search` followed by
`golem_video_attach`. The cron runtime injects `profile`, `job_id`, and the
per-fire `delivery_id`; tools never accept a model-supplied WeChat target.
Creation registers the job id against the current active Run, so Golem stores
the real WeChat session and receiver binding. Each scheduled result is posted
directly to the authenticated capability API and committed to the transactional
Outbox with a stable per-fire id. Capability outages retry the same id; duplicate
commits return the existing receipt instead of creating another WeChat message.
Temporary capability failures retry the same id for a bounded window and then
become Hermes `last_delivery_error`; Gateway shutdown interrupts that retry.

Private chats and groups both use `deliver="origin"`. Do not encode a group as
`relay:chatroom:...`; that selects Hermes' live Relay adapter, which has no active
Run when a later cron fire occurs. Golem Hermes plugin `0.7.4` accepts both valid
Hermes group session modes (shared group context and per-user group isolation)
while still matching the active chat, sender, triggering message, and Run before
persisting the receiver binding.

The durable Golem path currently applies to delivery mode `origin` or `relay`.
After changing a local job to either mode from a Relay turn, the bridge registers
the missing binding. Combined targets such as `origin,all` continue through
Hermes' native delivery path and are not claimed by this bridge.

Cron jobs created before plugin version `1.4.0` have no registered Golem binding
and fail with an explicit `recreate this cron job` delivery error. Delete and
recreate those jobs from the intended WeChat conversation after deployment.

## Security notes

- The current target comes from Hermes `ContextVar` session state, not model
  arguments or process-global `os.environ` session fields.
- The capability API independently verifies `platform`, `chat_id`,
  `session_key`, `user_id`, and `message_id` against an active run; the client
  context is not authority.
- Capability control-plane redirects are rejected so the shared bearer token
  cannot be forwarded. Golem's video data plane handles only validated HTTPS
  redirects and strips sensitive Provider headers on cross-origin redirects.
- HTTP error bodies are never returned to the model and authentication errors
  never include the configured token.
- Candidate bytes are disclosed only to the visual Provider explicitly
  configured in Hermes. The Agent never receives the data URL or source URL.
- Image text is treated as untrusted content by a fixed inspection prompt; the
  model cannot supply or override that prompt.
