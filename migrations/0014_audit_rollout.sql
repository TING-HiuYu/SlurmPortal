-- The audit log becomes a bounded ring: once audit.max-records is reached each new record removes the
-- oldest. Rows can only leave from the old end, so the remaining chain stays verifiable from its first
-- row. The UNIQUE index on hash is dropped: every index costs an extra D1 row write per insert, and
-- seq (the rowid) already orders and identifies records.
CREATE TABLE v2_audit_log_next (
	seq INTEGER PRIMARY KEY,
	time TEXT NOT NULL,
	actor_id INTEGER NOT NULL DEFAULT 0,
	actor TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL,
	status INTEGER NOT NULL DEFAULT 0,
	request_id TEXT NOT NULL DEFAULT '',
	trace_id TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	prev_hash TEXT NOT NULL,
	hash TEXT NOT NULL
)
--;
INSERT INTO v2_audit_log_next (seq, time, actor_id, actor, action, target, outcome, status, request_id, trace_id, detail, prev_hash, hash)
	SELECT seq, time, actor_id, actor, action, target, outcome, status, request_id, trace_id, detail, prev_hash, hash FROM v2_audit_log
--;
DROP TABLE v2_audit_log
--;
ALTER TABLE v2_audit_log_next RENAME TO v2_audit_log
--;
CREATE TRIGGER IF NOT EXISTS v2_audit_log_no_update BEFORE UPDATE ON v2_audit_log
BEGIN
	SELECT RAISE(ABORT, 'audit log is append-only');
END
--;
CREATE TRIGGER IF NOT EXISTS v2_audit_log_oldest_only BEFORE DELETE ON v2_audit_log
WHEN OLD.seq <> (SELECT MIN(seq) FROM v2_audit_log)
BEGIN
	SELECT RAISE(ABORT, 'audit log only drops its oldest records');
END
