ALTER TABLE namespaces
    ADD COLUMN openlinker_subject TEXT UNIQUE
        CHECK (openlinker_subject IS NULL OR (kind = 'openlinker' AND length(openlinker_subject) BETWEEN 1 AND 200)),
    ADD COLUMN review_trusted BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE skills
    ADD COLUMN review_state TEXT NOT NULL DEFAULT 'approved'
        CHECK (review_state IN ('approved', 'pending', 'rejected')),
    ADD COLUMN review_reason TEXT NOT NULL DEFAULT '' CHECK (length(review_reason) <= 500),
    ADD COLUMN claimed BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN shown_visibility TEXT GENERATED ALWAYS AS
        (CASE WHEN review_state = 'approved' THEN visibility ELSE 'review' END) STORED;

ALTER TABLE mcp_servers
    ADD COLUMN review_state TEXT NOT NULL DEFAULT 'approved'
        CHECK (review_state IN ('approved', 'pending', 'rejected')),
    ADD COLUMN review_reason TEXT NOT NULL DEFAULT '' CHECK (length(review_reason) <= 500),
    ADD COLUMN shown_visibility TEXT GENERATED ALWAYS AS
        (CASE WHEN review_state = 'approved' THEN visibility ELSE 'review' END) STORED;

CREATE INDEX skills_review_pending_idx ON skills (id) WHERE review_state <> 'approved';
CREATE INDEX mcp_servers_review_pending_idx ON mcp_servers (id) WHERE review_state <> 'approved';

CREATE TABLE skill_redirects (
    from_namespace TEXT NOT NULL,
    from_name      TEXT NOT NULL,
    skill_id       BIGINT NOT NULL REFERENCES skills (id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (from_namespace, from_name)
);

CREATE TABLE reports (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('skill', 'mcp')),
    entry_id    BIGINT NOT NULL,
    namespace   TEXT NOT NULL,
    name        TEXT NOT NULL,
    version     TEXT NOT NULL DEFAULT '' CHECK (length(version) <= 255),
    reporter    TEXT NOT NULL CHECK (reporter <> '' AND length(reporter) <= 200),
    category    TEXT NOT NULL CHECK (category IN ('malware', 'credential', 'license', 'spam', 'impersonation', 'other')),
    details     TEXT NOT NULL DEFAULT '' CHECK (length(details) <= 2000),
    state       TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'dismissed', 'actioned')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    resolved_by TEXT NOT NULL DEFAULT '',
    CHECK ((state = 'open') = (resolved_at IS NULL))
);
CREATE UNIQUE INDEX reports_open_reporter_key ON reports (kind, entry_id, reporter) WHERE state = 'open';
CREATE INDEX reports_state_idx ON reports (state, id);
