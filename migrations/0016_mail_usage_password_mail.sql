-- Every delivery through a leaf provider (SMTP, REST) is counted per UTC day, provider and purpose;
-- mail-pool providers are not counted themselves, the member that delivered is. WITHOUT ROWID keeps
-- one row write per count (no separate primary-key index).
CREATE TABLE IF NOT EXISTS v2_mail_usage (
	day TEXT NOT NULL,
	provider TEXT NOT NULL,
	purpose TEXT NOT NULL,
	sent INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, provider, purpose)
) WITHOUT ROWID
--;
-- Settings and template of the password e-mail code (one row).
CREATE TABLE IF NOT EXISTS v2_password_mail_config (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	config_json TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
