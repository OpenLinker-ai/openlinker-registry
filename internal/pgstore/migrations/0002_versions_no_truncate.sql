-- Versions are immutable: TRUNCATE (including TRUNCATE ... CASCADE from
-- skills) must not bypass the row-level DELETE guard.
CREATE FUNCTION skill_versions_no_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'skill_versions rows are immutable and cannot be truncated';
END;
$$;
CREATE TRIGGER skill_versions_no_truncate_trg
    BEFORE TRUNCATE ON skill_versions
    FOR EACH STATEMENT EXECUTE FUNCTION skill_versions_no_truncate();
