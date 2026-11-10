-- A yanked skill is terminal: no statement may move it to another status.
-- This backs the conditional UPDATE in the store with a schema rule.
CREATE FUNCTION skills_yank_terminal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'yanked' AND NEW.status IS DISTINCT FROM 'yanked' THEN
        RAISE EXCEPTION 'a yanked skill cannot change status';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER skills_yank_terminal_trg
    BEFORE UPDATE OF status ON skills
    FOR EACH ROW EXECUTE FUNCTION skills_yank_terminal();
