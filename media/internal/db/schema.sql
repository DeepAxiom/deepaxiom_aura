-- The media subsystem's own database. It is deliberately NOT the kernel's:
-- these rows are public-zone bookkeeping, they churn at a completely different
-- rate, and a codec CVE must never be a reason to touch kernel state.

-- Several nodes start at once on a deploy, and each of them runs this file.
-- IF NOT EXISTS makes a statement harmless to repeat; it does not make two
-- sessions running it at the same instant safe, and the loser of that race is
-- a node that refuses to start. The whole file is one transaction, so this
-- lock is held to the end of it and the second node simply waits its turn.
--
-- The lock is per schema: two deployments that keep their tables in different
-- schemas of one database are not each other's business, and neither should
-- wait on the other's start.
SELECT pg_advisory_xact_lock(470691220, hashtext(current_schema()));

CREATE TABLE IF NOT EXISTS media_asset (
    id            text PRIMARY KEY,
    state         text        NOT NULL,
    source_key    text        NOT NULL,
    source_bytes  bigint      NOT NULL DEFAULT 0,
    source_sha256 text        NOT NULL DEFAULT '',
    mime          text        NOT NULL DEFAULT '',
    duration_ms   bigint      NOT NULL DEFAULT 0,
    address       text        NOT NULL DEFAULT '',
    outputs       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    error         text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- One row per unit of work. `idem` is what makes an at-least-once caller safe:
-- the API's Idempotency-Key and the C3 envelope's `idem` both land here, so a
-- retried delivery finds its own job instead of starting a second encode.
CREATE TABLE IF NOT EXISTS media_job (
    id           bigserial PRIMARY KEY,
    asset_id     text        NOT NULL REFERENCES media_asset (id) ON DELETE CASCADE,
    kind         text        NOT NULL,
    state        text        NOT NULL DEFAULT 'queued',
    priority     int         NOT NULL DEFAULT 100,
    attempts     int         NOT NULL DEFAULT 0,
    max_attempts int         NOT NULL DEFAULT 3,
    worker       text        NOT NULL DEFAULT '',
    lease_until  timestamptz,
    run_after    timestamptz NOT NULL DEFAULT now(),
    last_error   text        NOT NULL DEFAULT '',
    idem         text UNIQUE,
    args         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- The claim's index. Partial, because the queued rows are the only ones it is
-- ever asked about and a finished job should not make the hot path slower.
CREATE INDEX IF NOT EXISTS media_job_claimable
    ON media_job (priority, run_after, id)
    WHERE state = 'queued';

-- Expired leases: the reclaimer's scan.
CREATE INDEX IF NOT EXISTS media_job_running_lease
    ON media_job (lease_until)
    WHERE state = 'running';

CREATE INDEX IF NOT EXISTS media_job_asset ON media_job (asset_id);

-- ---------------------------------------------------------------------------
-- 0.2.0. Everything below is additive, and written so that it can run against
-- a database 0.1.0 created and has been filling ever since: no column is
-- renamed or dropped, every new column has a default, and a 0.1.0 binary still
-- pointed at this database keeps working, because it names the columns it
-- reads and has never heard of these.
-- ---------------------------------------------------------------------------

-- Three columns on media_asset:
--
--   progress  How far through the encode a job is, 0..1. Written a few times a
--             minute by the worker holding the job, not once per ffmpeg report.
--   growing   True while `address` is set but the package behind it is still
--             being written: an encode that publishes as it goes, or a live
--             stream. `ready` still means what it always meant, complete, so
--             this is how a consumer tells "playable now" from "finished".
--   kind      'vod' for an uploaded source, 'live' for one that arrived as a
--             stream. A live asset has no source object; the stream was the
--             source.
--
-- Adding a column takes an exclusive lock on the table, however briefly, and
-- ADD COLUMN IF NOT EXISTS takes it even when the column is already there.
-- This file runs on every start. So the ALTER is only reached when a column is
-- actually missing, which is once per database: every later start reads the
-- catalogue and touches nothing. Without that, a node restarting while a
-- backup held the table open would queue behind the backup, and every other
-- query on media_asset would queue behind the node.
--
-- For the one start that does need the lock, it waits ten seconds and then
-- gives up loudly rather than holding that queue for as long as the backup
-- takes. The timeout is for the rest of this transaction only.
SET LOCAL lock_timeout = '10s';

DO $$
BEGIN
    IF (SELECT count(*) FROM information_schema.columns
         WHERE table_schema = current_schema()
           AND table_name = 'media_asset'
           AND column_name IN ('progress', 'growing', 'kind')) < 3 THEN
        ALTER TABLE media_asset
            ADD COLUMN IF NOT EXISTS progress double precision NOT NULL DEFAULT 0,
            ADD COLUMN IF NOT EXISTS growing  boolean          NOT NULL DEFAULT false,
            ADD COLUMN IF NOT EXISTS kind     text             NOT NULL DEFAULT 'vod';
    END IF;
END
$$;

-- One row per live stream. Deliberately not a media_job: a job is work any
-- worker may claim, and a live stream is a connection that arrived at one
-- particular node. (It also means a 0.1.0 worker sharing this database during
-- a rolling deploy never sees one, claims it, and fails it for being a kind it
-- does not know.)
--
-- key_hash is the SHA-256 of the stream key. The key itself is shown once,
-- when the stream is created, and is not stored anywhere.
CREATE TABLE IF NOT EXISTS media_live (
    id          text PRIMARY KEY,
    state       text        NOT NULL DEFAULT 'idle',
    key_hash    text        NOT NULL,
    asset_id    text        NOT NULL,
    node        text        NOT NULL DEFAULT '',
    lease_until timestamptz,
    error       text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    ended_at    timestamptz,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Streams whose node stopped renewing them: the scan that finalises what a
-- crashed node left behind.
CREATE INDEX IF NOT EXISTS media_live_lease
    ON media_live (lease_until)
    WHERE state = 'live';
