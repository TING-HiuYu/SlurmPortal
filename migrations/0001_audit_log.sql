CREATE TABLE IF NOT EXISTS v2_audit_log (
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
	hash TEXT NOT NULL UNIQUE
)
--;
CREATE TRIGGER IF NOT EXISTS v2_audit_log_no_update BEFORE UPDATE ON v2_audit_log
BEGIN
	SELECT RAISE(ABORT, 'audit log is append-only');
END
--;
CREATE TRIGGER IF NOT EXISTS v2_audit_log_no_delete BEFORE DELETE ON v2_audit_log
BEGIN
	SELECT RAISE(ABORT, 'audit log is append-only');
END
