-- E-mailed one-time codes for resetting a password. Only a keyed HMAC of the code is stored; a code
-- expires after a few minutes and is dropped after too many wrong attempts.
CREATE TABLE IF NOT EXISTS v2_password_resets (
	user_id INTEGER PRIMARY KEY,
	code_hash TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	sent_at TEXT NOT NULL
)
--;
-- v1 kept reversible (RSA-encrypted) passwords in users.password. v2 keeps only one-way hashes
-- (v2_user_credentials), so the reversible copies are removed.
UPDATE users SET password = '' WHERE COALESCE(password, '') <> ''
