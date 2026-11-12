-- Upstream registries (forwarding and mirroring), upstream-managed
-- namespaces and MCP server entries.

CREATE TABLE upstreams (
    id                    BIGSERIAL PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE
                          CHECK (length(name) BETWEEN 1 AND 40 AND name ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    kind                  TEXT NOT NULL CHECK (kind IN ('openlinker-registry', 'mcp-registry', 'agent-skills')),
    base_url              TEXT NOT NULL CHECK (length(base_url) <= 2048 AND base_url ~ '^https?://[^[:space:]]+$'),
    enabled               BOOLEAN NOT NULL DEFAULT true,
    priority              INT NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 100000),
    -- Visibility given to mirrored entries. Mirrored content is never private:
    -- it has no local members.
    visibility            TEXT NOT NULL CHECK (visibility IN ('public', 'internal')),
    namespace_allow       TEXT[] NOT NULL DEFAULT '{}',
    namespace_deny        TEXT[] NOT NULL DEFAULT '{}',
    target_namespace      TEXT NOT NULL DEFAULT '',
    sync_interval_seconds INT NOT NULL DEFAULT 0 CHECK (sync_interval_seconds >= 0),
    proxy_on_miss         BOOLEAN NOT NULL DEFAULT false,
    latest_only           BOOLEAN NOT NULL DEFAULT false,
    cursor                TEXT NOT NULL DEFAULT '',
    -- Counts changes to what is selected and how it is shown; a sync that
    -- started under an older version does not store its cursor.
    config_version        BIGINT NOT NULL DEFAULT 0,
    sync_state            TEXT NOT NULL DEFAULT 'idle' CHECK (sync_state IN ('idle', 'running')),
    sync_started_at       TIMESTAMPTZ,
    last_sync_at          TIMESTAMPTZ,
    last_sync_status      TEXT NOT NULL DEFAULT '' CHECK (last_sync_status IN ('', 'ok', 'failed')),
    last_sync_error       TEXT NOT NULL DEFAULT '',
    last_sync_stats       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- agent-skills indexes carry no namespaces, so they need a local target.
    CHECK ((kind = 'agent-skills') = (target_namespace <> ''))
);

-- A namespace is either local or managed by exactly one upstream.
ALTER TABLE namespaces ADD COLUMN upstream_id BIGINT REFERENCES upstreams (id);
ALTER TABLE namespaces DROP CONSTRAINT namespaces_kind_check;
ALTER TABLE namespaces ADD CONSTRAINT namespaces_kind_check
    CHECK (kind IN ('github', 'openlinker', 'internal', 'domain', 'upstream'));
ALTER TABLE namespaces ADD CONSTRAINT namespaces_upstream_kind_check
    CHECK ((kind = 'upstream') = (upstream_id IS NOT NULL));
CREATE INDEX namespaces_upstream_id_idx ON namespaces (upstream_id) WHERE upstream_id IS NOT NULL;

-- Ownership of a namespace never changes hands: a local namespace cannot be
-- given to an upstream, nor moved between upstreams.
CREATE FUNCTION namespaces_owner_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.upstream_id IS DISTINCT FROM OLD.upstream_id OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.name IS DISTINCT FROM OLD.name THEN
        RAISE EXCEPTION 'namespace name, kind and upstream are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER namespaces_owner_immutable_trg
    BEFORE UPDATE ON namespaces
    FOR EACH ROW EXECUTE FUNCTION namespaces_owner_immutable();

-- MCP servers. name is the full server.json name ("<namespace>/<server>"),
-- stored exactly as published; the owning namespace row is its lowercase
-- namespace part.
CREATE TABLE mcp_servers (
    id           BIGSERIAL PRIMARY KEY,
    namespace_id BIGINT NOT NULL REFERENCES namespaces (id),
    name         TEXT NOT NULL
                 CHECK (length(name) BETWEEN 3 AND 200 AND name ~ '^[a-zA-Z0-9.-]+/[a-zA-Z0-9._-]+$'),
    mode         TEXT NOT NULL CHECK (mode IN ('registered', 'indexed', 'mirrored')),
    visibility   TEXT NOT NULL CHECK (visibility IN ('public', 'internal', 'private')),
    listed       BOOLEAN NOT NULL DEFAULT true,
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Names that differ only by letter case would be confusable.
CREATE UNIQUE INDEX mcp_servers_name_lower_key ON mcp_servers (lower(name));
CREATE INDEX mcp_servers_name_c_idx ON mcp_servers (name COLLATE "C");
CREATE INDEX mcp_servers_namespace_idx ON mcp_servers (namespace_id);

CREATE TABLE mcp_server_versions (
    id                BIGSERIAL PRIMARY KEY,
    server_id         BIGINT NOT NULL REFERENCES mcp_servers (id),
    version           TEXT NOT NULL CHECK (length(version) BETWEEN 1 AND 255 AND version <> 'latest'),
    -- The canonical document exactly as digested. It is text, not JSONB: JSONB
    -- would rewrite numbers ("1e2" becomes "100") and the stored bytes would
    -- no longer match the digest.
    server_json       TEXT NOT NULL CHECK (octet_length(server_json) <= 1048576
                                           AND jsonb_typeof(server_json::jsonb) = 'object'),
    -- Search text taken from the document when it is stored.
    title             TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    digest            TEXT NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deprecated', 'deleted')),
    status_message    TEXT NOT NULL DEFAULT '' CHECK (length(status_message) <= 500),
    status_changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    is_latest         BOOLEAN NOT NULL DEFAULT false,
    created_by        TEXT NOT NULL,
    UNIQUE (server_id, version),
    CHECK (status <> 'active' OR status_message = ''),
    -- A deleted version is never the latest one.
    CHECK (NOT (is_latest AND status = 'deleted'))
);
CREATE UNIQUE INDEX mcp_server_versions_latest_key ON mcp_server_versions (server_id) WHERE is_latest;
CREATE INDEX mcp_server_versions_updated_idx ON mcp_server_versions (updated_at);

-- Published versions are immutable: only status fields, updated_at and
-- is_latest may change, and rows may never be deleted or truncated.
CREATE FUNCTION mcp_server_versions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'mcp_server_versions rows are immutable and cannot be deleted';
    END IF;
    IF (NEW.id, NEW.server_id, NEW.version, NEW.server_json, NEW.title, NEW.description, NEW.digest,
        NEW.published_at, NEW.created_by)
       IS DISTINCT FROM
       (OLD.id, OLD.server_id, OLD.version, OLD.server_json, OLD.title, OLD.description, OLD.digest,
        OLD.published_at, OLD.created_by) THEN
        RAISE EXCEPTION 'mcp_server_versions content columns are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER mcp_server_versions_immutable_trg
    BEFORE UPDATE OR DELETE ON mcp_server_versions
    FOR EACH ROW EXECUTE FUNCTION mcp_server_versions_immutable();

CREATE FUNCTION mcp_server_versions_no_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'mcp_server_versions rows are immutable and cannot be truncated';
END;
$$;
CREATE TRIGGER mcp_server_versions_no_truncate_trg
    BEFORE TRUNCATE ON mcp_server_versions
    FOR EACH STATEMENT EXECUTE FUNCTION mcp_server_versions_no_truncate();

-- For a skill version mirrored from an upstream registry: whether that
-- registry serves the version's content. It is deliberately not among the
-- immutable columns: the upstream may start serving it later (a license is
-- approved there).
ALTER TABLE skill_versions ADD COLUMN mirror_served BOOLEAN;
