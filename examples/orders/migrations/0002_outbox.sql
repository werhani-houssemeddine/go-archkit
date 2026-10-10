CREATE TABLE IF NOT EXISTS outbox_events (
  id                uuid PRIMARY KEY,
  type              text        NOT NULL,
  aggregate_type    text,
  aggregate_id      text,
  aggregate_version integer,
  payload           jsonb       NOT NULL,
  metadata          jsonb       NOT NULL DEFAULT '{}'::jsonb,
  occurred_at       timestamptz NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  available_at      timestamptz NOT NULL DEFAULT now(),
  processed_at      timestamptz,
  failed_at         timestamptz,
  attempts          integer     NOT NULL DEFAULT 0,
  last_error        text
);
CREATE INDEX IF NOT EXISTS outbox_pending_idx ON outbox_events (available_at) WHERE processed_at IS NULL AND failed_at IS NULL;
