CREATE TABLE installation_owner (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    principal_id TEXT NOT NULL UNIQUE REFERENCES principals(id)
);

-- Legacy bootstrap created the owner and the audit event in one transaction.
-- An older database with no retained audit event can identify a sole login.
WITH audited_owner AS (
    SELECT DISTINCT p.id
    FROM events e
    JOIN principals p ON p.id = json_extract(e.payload_json, '$.principal_id')
    WHERE e.type = 'auth' AND e.audit = 1
      AND json_extract(e.payload_json, '$.action') = 'owner_created'
), candidates AS (
    SELECT id FROM audited_owner
    UNION ALL
    SELECT id FROM principals
    WHERE password_hash IS NOT NULL AND NOT EXISTS (SELECT 1 FROM audited_owner)
)
INSERT INTO installation_owner(singleton, principal_id)
SELECT 1, id FROM candidates WHERE (SELECT count(*) FROM candidates) = 1;
