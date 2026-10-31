CREATE TABLE IF NOT EXISTS v2_mail_providers (
	name TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 0,
	is_default INTEGER NOT NULL DEFAULT 0,
	config_json TEXT NOT NULL,
	secret_sealed TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
--;
INSERT OR IGNORE INTO v2_mail_providers (name, kind, enabled, is_default, config_json, secret_sealed, created_at, updated_at)
SELECT 'default', 'smtp',
	CASE WHEN smtp_password_sealed <> '' THEN 1 ELSE 0 END, 1,
	json_extract(config_json, '$.smtp'), smtp_password_sealed, updated_at, updated_at
FROM v2_notification_config
WHERE id = 1 AND COALESCE(json_extract(config_json, '$.smtp.host'), '') <> ''
