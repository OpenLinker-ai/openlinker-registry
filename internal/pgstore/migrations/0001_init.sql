-- openlinker-registry R1 schema.

CREATE TABLE namespaces (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE
                 CHECK (length(name) <= 128
                        AND name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$'),
    kind         TEXT NOT NULL CHECK (kind IN ('github', 'openlinker', 'internal', 'domain')),
    -- io.github namespaces are bound to the numeric GitHub account id, never the login.
    github_id    BIGINT CHECK (github_id IS NULL OR github_id > 0),
    display_name TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'github') = (github_id IS NOT NULL))
);
CREATE UNIQUE INDEX namespaces_github_id_key ON namespaces (github_id) WHERE github_id IS NOT NULL;

-- Subjects are "token:<id>" in R1; later phases add "github:<id>" etc.
CREATE TABLE namespace_roles (
    namespace_id BIGINT NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,
    subject      TEXT NOT NULL CHECK (subject <> ''),
    role         TEXT NOT NULL CHECK (role IN ('owner', 'publisher', 'reader')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, subject)
);
CREATE INDEX namespace_roles_subject_idx ON namespace_roles (subject);

CREATE TABLE api_tokens (
    id          TEXT PRIMARY KEY CHECK (id ~ '^[a-z0-9]{16,64}$'),
    secret_hash BYTEA NOT NULL CHECK (length(secret_hash) = 32),
    name        TEXT NOT NULL,
    scopes      TEXT[] NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

CREATE TABLE skills (
    id            BIGSERIAL PRIMARY KEY,
    namespace_id  BIGINT NOT NULL REFERENCES namespaces (id),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64 AND name ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    description   TEXT NOT NULL DEFAULT '',
    visibility    TEXT NOT NULL CHECK (visibility IN ('public', 'internal', 'private')),
    listed        BOOLEAN NOT NULL DEFAULT true,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deprecated', 'yanked')),
    status_reason TEXT NOT NULL DEFAULT '',
    mode          TEXT NOT NULL CHECK (mode IN ('indexed', 'hosted')),
    source_key    TEXT,
    global_alias  TEXT UNIQUE CHECK (global_alias IS NULL OR (length(global_alias) BETWEEN 1 AND 64
                                     AND global_alias ~ '^[a-z0-9]+(-[a-z0-9]+)*$')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace_id, name),
    CHECK ((mode = 'indexed') = (source_key IS NOT NULL))
);
-- One indexed entry per normalized external source on this instance.
CREATE UNIQUE INDEX skills_source_key_key ON skills (source_key) WHERE source_key IS NOT NULL;

CREATE TABLE skill_versions (
    id                      BIGSERIAL PRIMARY KEY,
    skill_id                BIGINT NOT NULL REFERENCES skills (id),
    version                 TEXT NOT NULL CHECK (version ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$'),
    mode                    TEXT NOT NULL CHECK (mode IN ('indexed', 'hosted')),
    frontmatter_name        TEXT NOT NULL,
    frontmatter_description TEXT NOT NULL,
    license_kind            TEXT NOT NULL CHECK (license_kind IN ('spdx', 'custom', 'unknown')),
    license_id              TEXT NOT NULL DEFAULT '',
    license_text            TEXT NOT NULL DEFAULT '',
    license_approved        BOOLEAN NOT NULL DEFAULT false,
    core_compatible         BOOLEAN NOT NULL,
    files                   JSONB NOT NULL CHECK (jsonb_typeof(files) = 'array'),
    artifact_digest         TEXT NOT NULL CHECK (artifact_digest ~ '^sha256:[0-9a-f]{64}$'),
    artifact_type           TEXT NOT NULL CHECK (artifact_type IN ('skill-md', 'tar', 'openlinker-bundle-v1')),
    artifact_size           BIGINT NOT NULL CHECK (artifact_size >= 0),
    archive_digest          TEXT CHECK (archive_digest IS NULL OR archive_digest ~ '^sha256:[0-9a-f]{64}$'),
    source                  JSONB,
    locator                 JSONB,
    indexed_by              TEXT NOT NULL DEFAULT '',
    check_result            TEXT CHECK (check_result IS NULL OR check_result IN ('ok', 'source_changed', 'source_unavailable')),
    checked_at              TIMESTAMPTZ,
    yanked_at               TIMESTAMPTZ,
    yank_reason             TEXT NOT NULL DEFAULT '',
    created_by              TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (skill_id, version),
    CHECK ((mode = 'hosted') = (source IS NOT NULL AND locator IS NULL)),
    CHECK ((mode = 'indexed') = (locator IS NOT NULL AND source IS NULL))
);

-- Versions are immutable: only review, check and yank columns may change, and
-- rows may never be deleted.
CREATE FUNCTION skill_versions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'skill_versions rows are immutable and cannot be deleted';
    END IF;
    IF (NEW.id, NEW.skill_id, NEW.version, NEW.mode, NEW.frontmatter_name, NEW.frontmatter_description,
        NEW.license_kind, NEW.license_id, NEW.license_text, NEW.core_compatible, NEW.files,
        NEW.artifact_digest, NEW.artifact_type, NEW.artifact_size, NEW.archive_digest,
        NEW.source, NEW.locator, NEW.indexed_by, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.skill_id, OLD.version, OLD.mode, OLD.frontmatter_name, OLD.frontmatter_description,
        OLD.license_kind, OLD.license_id, OLD.license_text, OLD.core_compatible, OLD.files,
        OLD.artifact_digest, OLD.artifact_type, OLD.artifact_size, OLD.archive_digest,
        OLD.source, OLD.locator, OLD.indexed_by, OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'skill_versions content columns are immutable';
    END IF;
    IF OLD.yanked_at IS NOT NULL AND NEW.yanked_at IS DISTINCT FROM OLD.yanked_at THEN
        RAISE EXCEPTION 'a yanked version cannot be restored';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER skill_versions_immutable_trg
    BEFORE UPDATE OR DELETE ON skill_versions
    FOR EACH ROW EXECUTE FUNCTION skill_versions_immutable();

CREATE TABLE audit_events (
    id        BIGSERIAL PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor     TEXT NOT NULL,
    action    TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT '',
    target    TEXT NOT NULL DEFAULT '',
    details   JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;
CREATE TRIGGER audit_events_no_update
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();
CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
