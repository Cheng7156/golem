package sqlite

const schemaV1 = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS inbox_events (
  accept_seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  dedupe_key TEXT NOT NULL UNIQUE,
  message_id INTEGER NOT NULL DEFAULT 0,
  topic TEXT NOT NULL,
  session_id TEXT NOT NULL,
  occurred_at INTEGER NOT NULL,
  accepted_at INTEGER NOT NULL,
  binding_json BLOB NOT NULL,
  payload_json BLOB NOT NULL,
  status TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_status_seq
  ON inbox_events(status, accept_seq);
CREATE INDEX IF NOT EXISTS idx_inbox_session_order
  ON inbox_events(session_id, occurred_at, message_id, accept_seq);

CREATE TABLE IF NOT EXISTS turns (
  id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL UNIQUE REFERENCES inbox_events(id),
  session_id TEXT NOT NULL,
  state TEXT NOT NULL,
  route TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 0,
  base_session_version INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_turns_session_created
  ON turns(session_id, created_at);

CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  turn_id TEXT NOT NULL REFERENCES turns(id),
  session_id TEXT NOT NULL,
  lane TEXT NOT NULL,
  state TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0,
  attempt INTEGER NOT NULL DEFAULT 0,
  lease_token TEXT NOT NULL DEFAULT '',
  lease_until INTEGER NOT NULL DEFAULT 0,
  deadline INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  checkpoint_json BLOB,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runs_sched
  ON runs(state, lane, next_attempt_at, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_turn
  ON runs(turn_id, revision);

CREATE TABLE IF NOT EXISTS outbox (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(id),
  session_id TEXT NOT NULL,
  receiver_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  payload_json BLOB NOT NULL,
  sequence INTEGER NOT NULL,
  state TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 0,
  lease_token TEXT NOT NULL DEFAULT '',
  lease_until INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  receipt_id INTEGER NOT NULL DEFAULT 0,
  receipt_time INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(session_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_outbox_sched
  ON outbox(state, next_attempt_at, created_at);

CREATE TABLE IF NOT EXISTS delivery_attempts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  outbox_id TEXT NOT NULL REFERENCES outbox(id),
  attempt INTEGER NOT NULL,
  outcome TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  receipt_id INTEGER NOT NULL DEFAULT 0,
  receipt_time INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_delivery_attempts_outbox
  ON delivery_attempts(outbox_id, id);
`

const schemaV2 = `
CREATE TABLE IF NOT EXISTS async_delivery_tickets (
  id TEXT PRIMARY KEY,
  ticket_hash TEXT NOT NULL UNIQUE,
  profile TEXT NOT NULL,
  producer_epoch TEXT NOT NULL,
  delegation_id TEXT NOT NULL,
  hermes_session_id TEXT NOT NULL,
  relay_session_key TEXT NOT NULL,
  chat_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  receiver_id TEXT NOT NULL,
  binding_json BLOB NOT NULL,
  parent_run_id TEXT NOT NULL REFERENCES runs(id),
  state TEXT NOT NULL,
  result_message_id TEXT NOT NULL DEFAULT '',
  outbox_id TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(profile, producer_epoch, delegation_id)
);
CREATE INDEX IF NOT EXISTS idx_async_delivery_session
  ON async_delivery_tickets(profile, producer_epoch, hermes_session_id, state);
`

const schemaV3 = `
CREATE TABLE IF NOT EXISTS cron_delivery_bindings (
  id TEXT PRIMARY KEY,
  profile TEXT NOT NULL,
  job_id TEXT NOT NULL,
  chat_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  receiver_id TEXT NOT NULL,
  binding_json BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(profile, job_id)
);

CREATE TABLE IF NOT EXISTS cron_delivery_commits (
  id TEXT PRIMARY KEY,
  binding_id TEXT NOT NULL REFERENCES cron_delivery_bindings(id),
  delivery_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  message_id TEXT NOT NULL,
  outbox_id TEXT NOT NULL REFERENCES outbox(id),
  created_at INTEGER NOT NULL,
  UNIQUE(binding_id, delivery_id)
);
`

const schemaV4 = `
CREATE TABLE IF NOT EXISTS async_delivery_direct_outputs (
  id TEXT PRIMARY KEY,
  ticket_id TEXT NOT NULL REFERENCES async_delivery_tickets(id),
  invocation_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  outbox_id TEXT NOT NULL REFERENCES outbox(id),
  created_at INTEGER NOT NULL,
  UNIQUE(ticket_id, invocation_id)
);
CREATE INDEX IF NOT EXISTS idx_async_direct_ticket
  ON async_delivery_direct_outputs(ticket_id, created_at);
`

const schemaV5 = `
CREATE TABLE IF NOT EXISTS media_objects (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  mime_type TEXT NOT NULL,
  path TEXT NOT NULL UNIQUE,
  size INTEGER NOT NULL CHECK(size > 0),
  sha256 TEXT NOT NULL UNIQUE,
  retained_until INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  last_access_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_media_objects_collect
  ON media_objects(retained_until, last_access_at);

CREATE TABLE IF NOT EXISTS outbox_media_refs (
  outbox_id TEXT NOT NULL REFERENCES outbox(id) ON DELETE CASCADE,
  object_id TEXT NOT NULL REFERENCES media_objects(id),
  PRIMARY KEY(outbox_id, object_id)
);
CREATE INDEX IF NOT EXISTS idx_outbox_media_object
  ON outbox_media_refs(object_id);

CREATE TRIGGER IF NOT EXISTS trg_outbox_video_media_insert
AFTER INSERT ON outbox
WHEN NEW.kind = 'video'
BEGIN
  INSERT INTO outbox_media_refs(outbox_id, object_id)
  VALUES(NEW.id, json_extract(NEW.payload_json, '$.object_id'));
  INSERT INTO outbox_media_refs(outbox_id, object_id)
  VALUES(NEW.id, json_extract(NEW.payload_json, '$.thumb_object_id'));
END;

CREATE TRIGGER IF NOT EXISTS trg_outbox_video_media_release
AFTER UPDATE OF state ON outbox
WHEN NEW.state IN ('sent', 'dead_letter')
BEGIN
  DELETE FROM outbox_media_refs WHERE outbox_id = NEW.id;
END;
`

const schemaV6 = `
CREATE TABLE IF NOT EXISTS cron_delivery_direct_outputs (
  id TEXT PRIMARY KEY,
  binding_id TEXT NOT NULL REFERENCES cron_delivery_bindings(id),
  delivery_id TEXT NOT NULL,
  invocation_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  outbox_id TEXT NOT NULL REFERENCES outbox(id),
  created_at INTEGER NOT NULL,
  UNIQUE(binding_id, delivery_id, invocation_id)
);
CREATE INDEX IF NOT EXISTS idx_cron_direct_delivery
  ON cron_delivery_direct_outputs(binding_id, delivery_id, created_at);
`

const schemaV7 = `
CREATE TABLE IF NOT EXISTS context_outbox (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL,
  accept_seq INTEGER NOT NULL,
  conversation_seq INTEGER NOT NULL,
  event_id TEXT NOT NULL UNIQUE REFERENCES inbox_events(id),
  payload_hash TEXT NOT NULL,
  observation_json BLOB NOT NULL,
  state TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 0,
  lease_token TEXT NOT NULL DEFAULT '',
  lease_until INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(conversation_id, conversation_seq)
);
CREATE INDEX IF NOT EXISTS idx_context_outbox_sched
  ON context_outbox(state, next_attempt_at, conversation_id, conversation_seq);
CREATE INDEX IF NOT EXISTS idx_context_outbox_barrier
  ON context_outbox(conversation_id, conversation_seq, state);

CREATE TABLE IF NOT EXISTS relay_run_results (
  proposal_id TEXT PRIMARY KEY,
  invocation_id TEXT NOT NULL,
  run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
  result_kind TEXT NOT NULL,
  result_hash TEXT NOT NULL,
  outbox_ids_json BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_relay_result_invocation_proposal
  ON relay_run_results(invocation_id, proposal_id);
`
