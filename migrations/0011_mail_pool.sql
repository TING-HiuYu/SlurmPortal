-- free-mailing was renamed mail-pool the day it shipped; carry over any pool providers.
UPDATE v2_mail_providers SET kind = 'mail-pool' WHERE kind = 'free-mailing'
--;
DROP TABLE IF EXISTS v2_free_mailing_usage
--;
CREATE TABLE IF NOT EXISTS v2_mail_pool_usage (
	pool TEXT NOT NULL,
	member TEXT NOT NULL,
	period TEXT NOT NULL,
	sent INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (pool, member, period)
)
