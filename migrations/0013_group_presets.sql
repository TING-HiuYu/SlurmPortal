-- A POSIX group carries the presets of accounts whose main group it is: the home root (home is
-- <home_root>/<username>) and the login shell. There is no global default home any more.
ALTER TABLE groups ADD COLUMN home_root TEXT NOT NULL DEFAULT ''
--;
ALTER TABLE groups ADD COLUMN login_shell TEXT NOT NULL DEFAULT ''
--;
-- Backfill from the accounts already in each group: the most common parent of <parent>/<username>
-- homes and the most common shell among accounts with this main GID.
UPDATE groups SET home_root = COALESCE((
	SELECT substr(u.home_directory, 1, length(u.home_directory) - length(u.username) - 1) AS root
	FROM users u
	WHERE u.deleted_at IS NULL AND u.gid_number = groups.gid_number AND u.username <> ''
		AND length(u.home_directory) > length(u.username) + 1
		AND substr(u.home_directory, length(u.home_directory) - length(u.username)) = '/' || u.username
	GROUP BY root ORDER BY COUNT(*) DESC, root LIMIT 1), '')
WHERE group_class = 'posixGroup' AND COALESCE(gid_number, 0) > 0 AND deleted_at IS NULL
--;
UPDATE groups SET login_shell = COALESCE((
	SELECT u.login_shell FROM users u
	WHERE u.deleted_at IS NULL AND u.gid_number = groups.gid_number AND COALESCE(u.login_shell, '') <> ''
	GROUP BY u.login_shell ORDER BY COUNT(*) DESC, u.login_shell LIMIT 1), '')
WHERE group_class = 'posixGroup' AND COALESCE(gid_number, 0) > 0 AND deleted_at IS NULL
