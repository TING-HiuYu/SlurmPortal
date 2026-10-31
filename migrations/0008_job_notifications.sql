CREATE TABLE IF NOT EXISTS v2_notification_config (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	config_json TEXT NOT NULL,
	smtp_password_sealed TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
)
--;
CREATE TABLE IF NOT EXISTS v2_job_notifications (
	cluster_id TEXT NOT NULL,
	event_id TEXT NOT NULL,
	sluid TEXT NOT NULL,
	job_id INTEGER NOT NULL,
	uid INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('sending', 'sent', 'skipped', 'failed')),
	reason TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (cluster_id, event_id)
)
--;
CREATE INDEX IF NOT EXISTS ix_v2_job_notifications_updated ON v2_job_notifications(updated_at)
