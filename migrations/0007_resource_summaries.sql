CREATE TABLE IF NOT EXISTS v2_resource_summaries (
	cluster_id TEXT NOT NULL,
	sluid TEXT NOT NULL,
	event_id TEXT NOT NULL,
	job_id INTEGER NOT NULL,
	uid INTEGER NOT NULL,
	state TEXT NOT NULL,
	ended_at TEXT NOT NULL,
	ended_day TEXT NOT NULL,
	active_seconds INTEGER NOT NULL,
	cpus INTEGER NOT NULL,
	gpus INTEGER NOT NULL,
	memory_mib REAL NOT NULL,
	allocated_gpu_seconds REAL NOT NULL,
	observed_gpu_seconds REAL NOT NULL,
	gpu_utilization_percent_seconds REAL NOT NULL,
	gpu_memory_utilization_percent_seconds REAL NOT NULL,
	complete INTEGER NOT NULL,
	payload_hash TEXT NOT NULL,
	PRIMARY KEY (cluster_id, sluid)
)
--;
CREATE UNIQUE INDEX IF NOT EXISTS ux_v2_resource_summaries_event ON v2_resource_summaries(cluster_id, event_id)
--;
CREATE INDEX IF NOT EXISTS ix_v2_resource_summaries_day ON v2_resource_summaries(ended_day, state, uid)
--;
CREATE INDEX IF NOT EXISTS ix_v2_resource_summaries_uid_day ON v2_resource_summaries(uid, ended_day)
