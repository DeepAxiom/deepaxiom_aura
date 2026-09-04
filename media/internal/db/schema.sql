-- The media subsystem's own database. It is deliberately NOT the kernel's:
-- these rows are public-zone bookkeeping, they churn at a completely different
-- rate, and a codec CVE must never be a reason to touch kernel state.

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
