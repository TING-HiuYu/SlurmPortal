CREATE TABLE IF NOT EXISTS v2_user_credentials (
	user_id INTEGER PRIMARY KEY,
	app_hash TEXT NOT NULL,
	ldap_hash TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
--;
CREATE TABLE IF NOT EXISTS v2_revoked_tokens (
	token_id TEXT PRIMARY KEY,
	expires_at TEXT NOT NULL
)
--;
CREATE INDEX IF NOT EXISTS v2_revoked_tokens_expiry ON v2_revoked_tokens(expires_at)
