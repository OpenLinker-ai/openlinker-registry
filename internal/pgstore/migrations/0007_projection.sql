ALTER TABLE gateways ADD COLUMN projection BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE namespaces
    ADD COLUMN generated_for TEXT UNIQUE CHECK (generated_for IS NULL OR (kind = 'openlinker' AND length(generated_for) BETWEEN 1 AND 200));

ALTER TABLE skills
    ADD COLUMN managed_by TEXT,
    ADD COLUMN managed_owner TEXT,
    ADD COLUMN managed_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN managed_phase SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN projection_hidden BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN legacy JSONB CHECK (legacy IS NULL OR (jsonb_typeof(legacy) = 'object' AND octet_length(legacy::text) <= 16384)),
    ADD COLUMN display_name TEXT CHECK (display_name IS NULL OR char_length(display_name) BETWEEN 1 AND 120),
    ADD CONSTRAINT skills_managed_complete CHECK ((managed_by IS NULL) = (managed_owner IS NULL));
ALTER TABLE skills DROP COLUMN shown_visibility;
ALTER TABLE skills ADD COLUMN shown_visibility TEXT GENERATED ALWAYS AS
    (CASE WHEN projection_hidden THEN 'hidden' WHEN review_state <> 'approved' THEN 'review' ELSE visibility END) STORED;

ALTER TABLE mcp_servers
    ADD COLUMN managed_by TEXT,
    ADD COLUMN managed_owner TEXT,
    ADD COLUMN managed_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN managed_phase SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN projection_hidden BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN legacy JSONB CHECK (legacy IS NULL OR (jsonb_typeof(legacy) = 'object' AND octet_length(legacy::text) <= 16384)),
    ADD COLUMN metadata JSONB CHECK (metadata IS NULL OR (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 262144)),
    ADD COLUMN metadata_revision BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT mcp_servers_managed_complete CHECK ((managed_by IS NULL) = (managed_owner IS NULL));
ALTER TABLE mcp_servers DROP COLUMN shown_visibility;
ALTER TABLE mcp_servers ADD COLUMN shown_visibility TEXT GENERATED ALWAYS AS
    (CASE WHEN projection_hidden THEN 'hidden' WHEN review_state <> 'approved' THEN 'review' ELSE visibility END) STORED;

ALTER TABLE skill_versions ADD COLUMN projection_hidden BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE projection_ops (
    consumer   TEXT NOT NULL,
    op         UUID NOT NULL,
    result     JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, op)
);
