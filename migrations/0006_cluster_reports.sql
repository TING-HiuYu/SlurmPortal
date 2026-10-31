CREATE TABLE IF NOT EXISTS v2_cluster_reports (
	kind TEXT NOT NULL,
	cluster_id TEXT NOT NULL,
	attempted_ms INTEGER NOT NULL,
	received_at TEXT NOT NULL,
	health TEXT NOT NULL,
	metadata_json TEXT NOT NULL,
	observed_at TEXT,
	payload_json TEXT,
	PRIMARY KEY (kind, cluster_id)
)
