package activitystore

// SealedSchemaSQL owns the activity objects in agent-reel-sealed/1.
const SealedSchemaSQL = `
CREATE TABLE activity(
  seq                INTEGER PRIMARY KEY,
  id                 TEXT NOT NULL UNIQUE,
  fingerprint        BLOB NOT NULL,
  marker             INTEGER NOT NULL,
  received_ns        INTEGER NOT NULL,
  envelope           TEXT NOT NULL,
  registration       TEXT,
  segment            INTEGER NOT NULL,
  precision          TEXT NOT NULL CHECK(precision IN ('estimated','unknown')),
  basis_line         INTEGER NOT NULL,
  basis_at_ns        INTEGER NOT NULL,
  basis_known        INTEGER NOT NULL,
  basis_receipt_tick INTEGER NOT NULL,
  receipt_tick       INTEGER NOT NULL,
  estimated_ns       INTEGER,
  chain              BLOB NOT NULL
) STRICT;
CREATE INDEX activity_placed ON activity(segment, basis_line, seq);

CREATE TABLE activity_progress(
  id            INTEGER PRIMARY KEY CHECK(id = 1),
  seq           INTEGER NOT NULL,
  chain         BLOB NOT NULL,
  logical_bytes INTEGER NOT NULL
) STRICT;
`
