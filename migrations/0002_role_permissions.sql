CREATE TABLE IF NOT EXISTS v2_role_permissions (
	role TEXT NOT NULL,
	object TEXT NOT NULL,
	action TEXT NOT NULL,
	PRIMARY KEY (role, object, action)
)
