CREATE TABLE IF NOT EXISTS posix_id_counters (kind TEXT PRIMARY KEY NOT NULL CHECK(kind IN ('uid','gid')), last_id INTEGER NOT NULL CHECK(last_id BETWEEN 9999 AND 2147483647))
--;
INSERT INTO posix_id_counters(kind,last_id) SELECT 'uid',MAX(9999,COALESCE(MAX(uid_number),0)) FROM users WHERE 1 ON CONFLICT(kind) DO UPDATE SET last_id=MAX(posix_id_counters.last_id,excluded.last_id)
--;
INSERT INTO posix_id_counters(kind,last_id) SELECT 'gid',MAX(9999,COALESCE((SELECT MAX(gid_number) FROM groups),0),COALESCE((SELECT MAX(gid_number) FROM users),0)) WHERE 1 ON CONFLICT(kind) DO UPDATE SET last_id=MAX(posix_id_counters.last_id,excluded.last_id)
--;
CREATE TRIGGER IF NOT EXISTS posix_users_insert AFTER INSERT ON users BEGIN UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(NEW.uid_number,0)) WHERE kind='uid'; UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(NEW.gid_number,0)) WHERE kind='gid'; END
--;
CREATE TRIGGER IF NOT EXISTS posix_users_update AFTER UPDATE OF uid_number,gid_number ON users BEGIN UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(OLD.uid_number,0),COALESCE(NEW.uid_number,0)) WHERE kind='uid'; UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(OLD.gid_number,0),COALESCE(NEW.gid_number,0)) WHERE kind='gid'; END
--;
CREATE TRIGGER IF NOT EXISTS posix_users_delete BEFORE DELETE ON users BEGIN UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(OLD.uid_number,0)) WHERE kind='uid'; UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(OLD.gid_number,0)) WHERE kind='gid'; END
--;
CREATE TRIGGER IF NOT EXISTS posix_groups_insert AFTER INSERT ON groups BEGIN UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(NEW.gid_number,0)) WHERE kind='gid'; END
--;
CREATE TRIGGER IF NOT EXISTS posix_groups_update AFTER UPDATE OF gid_number ON groups BEGIN UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(OLD.gid_number,0),COALESCE(NEW.gid_number,0)) WHERE kind='gid'; END
--;
CREATE TRIGGER IF NOT EXISTS posix_groups_delete BEFORE DELETE ON groups BEGIN UPDATE posix_id_counters SET last_id=MAX(last_id,COALESCE(OLD.gid_number,0)) WHERE kind='gid'; END
--;
INSERT OR IGNORE INTO roles(name, keyword, remark, status, sort, creator, created_at, updated_at) VALUES ('管理员', 'admin', 'super administrator', 1, 1, 'system', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
--;
INSERT OR IGNORE INTO roles(name, keyword, remark, status, sort, creator, created_at, updated_at) VALUES ('普通用户', 'user', 'self-service user', 1, 3, 'system', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
--;
INSERT OR IGNORE INTO v2_role_permissions(role, object, action) VALUES ('admin', '*', '*')
