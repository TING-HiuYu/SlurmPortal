CREATE TABLE IF NOT EXISTS v2_mail_samples (
	kind TEXT PRIMARY KEY,
	context_json TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
