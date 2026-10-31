CREATE TABLE IF NOT EXISTS v2_web_access_config (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	config_json TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
--;
CREATE TABLE IF NOT EXISTS v2_gateway_registrations (
	gateway_id TEXT PRIMARY KEY,
	cluster_id TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	boot_id TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	capabilities_json TEXT NOT NULL,
	ca_keys_json TEXT NOT NULL,
	registered_ms INTEGER NOT NULL
)
--;
CREATE TABLE IF NOT EXISTS v2_transfer_events (
	transfer_id TEXT PRIMARY KEY,
	gateway_id TEXT NOT NULL,
	cluster_id TEXT NOT NULL,
	user_id INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('sending', 'sent', 'skipped')),
	reason TEXT NOT NULL DEFAULT '',
	lease_until_ms INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)
