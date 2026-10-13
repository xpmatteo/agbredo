CREATE FUNCTION protect_approval_payload() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id IS DISTINCT FROM OLD.id OR NEW.run_id IS DISTINCT FROM OLD.run_id
 OR NEW.document->'proposal' IS DISTINCT FROM OLD.document->'proposal'
 OR NEW.document->'payload_hash' IS DISTINCT FROM OLD.document->'payload_hash'
 OR NEW.document->'expires_at' IS DISTINCT FROM OLD.document->'expires_at'
 OR NEW.document->'created_at' IS DISTINCT FROM OLD.document->'created_at' THEN
  RAISE EXCEPTION 'approval payload and binding are immutable';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_approval BEFORE UPDATE ON approvals FOR EACH ROW EXECUTE FUNCTION protect_approval_payload();

CREATE FUNCTION protect_run_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id IS DISTINCT FROM OLD.id OR NEW.agent_id IS DISTINCT FROM OLD.agent_id
 OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
 OR NEW.document->'agent' IS DISTINCT FROM OLD.document->'agent'
 OR NEW.document->'budget_micros' IS DISTINCT FROM OLD.document->'budget_micros'
 OR NEW.document->'created_at' IS DISTINCT FROM OLD.document->'created_at' THEN
  RAISE EXCEPTION 'run configuration snapshot is immutable';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER immutable_run BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION protect_run_snapshot();
