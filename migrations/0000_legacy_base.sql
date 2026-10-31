-- Legacy (v1) schema, idempotent so existing databases are untouched and fresh installs work.
CREATE TABLE IF NOT EXISTS `users` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`username` varchar(50) NOT NULL UNIQUE,`password` text NOT NULL,`nickname` varchar(50),`given_name` varchar(50),`mail` varchar(100),`job_number` varchar(20),`mobile` varchar(15) NOT NULL,`uid_number` integer DEFAULT 0,`gid_number` integer DEFAULT 0,`home_directory` varchar(255),`login_shell` varchar(64),`avatar` varchar(255),`postal_address` varchar(255),`departments` varchar(512),`position` varchar(128),`introduction` varchar(255),`status` tinyint(1) DEFAULT 1,`creator` varchar(20),`source` varchar(50),`department_id` varchar(100) NOT NULL,`source_user_id` varchar(100) NOT NULL,`source_union_id` varchar(100) NOT NULL,`user_dn` varchar(255) NOT NULL,`sync_state` tinyint(1) DEFAULT 1,PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_users_deleted_at` ON `users`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `roles` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`name` varchar(20) NOT NULL UNIQUE,`keyword` varchar(20) NOT NULL UNIQUE,`remark` varchar(100),`status` tinyint(1) DEFAULT 1,`sort` int(3) DEFAULT 999,`creator` varchar(20),PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_roles_deleted_at` ON `roles`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `user_roles` (`role_id` integer,`user_id` integer,PRIMARY KEY (`role_id`,`user_id`))
--;
CREATE TABLE IF NOT EXISTS `menus` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`name` varchar(50),`title` varchar(50),`icon` varchar(50),`path` varchar(100),`redirect` varchar(100),`component` varchar(100),`sort` int(3) DEFAULT 999,`status` tinyint(1) DEFAULT 1,`hidden` tinyint(1) DEFAULT 2,`no_cache` tinyint(1) DEFAULT 2,`always_show` tinyint(1) DEFAULT 2,`breadcrumb` tinyint(1) DEFAULT 1,`active_menu` varchar(100),`parent_id` integer DEFAULT 0,`creator` varchar(20),PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_menus_deleted_at` ON `menus`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `role_menus` (`menu_id` integer,`role_id` integer,PRIMARY KEY (`menu_id`,`role_id`))
--;
CREATE TABLE IF NOT EXISTS `groups` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`group_name` varchar(128),`remark` varchar(128),`creator` varchar(20),`group_type` varchar(20),`group_class` varchar(64),`gid_number` integer DEFAULT 0,`parent_id` integer DEFAULT 0,`source_dept_id` varchar(100),`source` varchar(20),`source_dept_parent_id` varchar(100),`source_user_num` integer DEFAULT 0,`group_dn` varchar(255) NOT NULL,`sync_state` tinyint(1) DEFAULT 1,PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_groups_deleted_at` ON `groups`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `group_users` (`group_id` integer,`user_id` integer,PRIMARY KEY (`group_id`,`user_id`))
--;
CREATE TABLE IF NOT EXISTS `apis` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`method` varchar(20),`path` varchar(100),`category` varchar(50),`remark` varchar(100),`creator` varchar(20),PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_apis_deleted_at` ON `apis`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `operation_logs` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`username` varchar(20),`ip` varchar(20),`ip_location` varchar(20),`method` varchar(20),`path` varchar(100),`remark` varchar(100),`status` int(4),`start_time` varchar(2048),`time_cost` int(6),`user_agent` varchar(2048),PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_operation_logs_deleted_at` ON `operation_logs`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `field_relations` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`flag` text,`attributes` JSON,PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_field_relations_deleted_at` ON `field_relations`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `certificate_authorities` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`name` text NOT NULL,`kind` text NOT NULL DEFAULT "x509",`certificate_pem` text NOT NULL,`private_key_ciphertext` text,`crl_pem` text,`crl_number` integer NOT NULL DEFAULT 0,`enabled` numeric NOT NULL DEFAULT false,PRIMARY KEY (`id`))
--;
CREATE UNIQUE INDEX IF NOT EXISTS `idx_certificate_authorities_name` ON `certificate_authorities`(`name`)
--;
CREATE INDEX IF NOT EXISTS `idx_certificate_authorities_deleted_at` ON `certificate_authorities`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `revoked_certificates` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`certificate_authority_id` integer NOT NULL,`serial_hex` text NOT NULL,`revoked_at` datetime NOT NULL,`reason` integer NOT NULL DEFAULT 0,PRIMARY KEY (`id`))
--;
CREATE UNIQUE INDEX IF NOT EXISTS `revoked_serial` ON `revoked_certificates`(`certificate_authority_id`,`serial_hex`)
--;
CREATE INDEX IF NOT EXISTS `idx_revoked_certificates_deleted_at` ON `revoked_certificates`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `user_certificates` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`user_id` integer NOT NULL,`certificate_authority_id` integer NOT NULL,`serial_hex` text NOT NULL,`certificate_pem` text NOT NULL,PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_user_certificates_deleted_at` ON `user_certificates`(`deleted_at`)
--;
CREATE UNIQUE INDEX IF NOT EXISTS `idx_user_certificates_user_id` ON `user_certificates`(`user_id`)
--;
CREATE TABLE IF NOT EXISTS `oauth_connectors` (`id` integer,`created_at` datetime,`updated_at` datetime,`deleted_at` datetime,`name` varchar(100) NOT NULL,`provider` varchar(50) NOT NULL,`certificate_authority_id` integer NOT NULL,`enabled` numeric DEFAULT true,`default_role_ids` JSON,`default_status` tinyint(1) DEFAULT 1,`department_id` integer NOT NULL,`default_login_shell` varchar(64),`config` JSON,`config_hash` text NOT NULL,`description` varchar(255),`creator` varchar(50),PRIMARY KEY (`id`))
--;
CREATE INDEX IF NOT EXISTS `idx_oauth_connectors_deleted_at` ON `oauth_connectors`(`deleted_at`)
--;
CREATE TABLE IF NOT EXISTS `oauth_states` (`state_hash` text,`connector_id` integer,`expires_at` datetime,`consumed_at` datetime,PRIMARY KEY (`state_hash`))
--;
CREATE UNIQUE INDEX IF NOT EXISTS ux_users_mail_normalized ON users(lower(trim(mail))) WHERE trim(mail) <> ''
--;
CREATE UNIQUE INDEX IF NOT EXISTS ux_users_uid_number ON users(uid_number) WHERE uid_number > 0
--;
CREATE UNIQUE INDEX IF NOT EXISTS ux_groups_gid_number ON groups(gid_number) WHERE gid_number > 0
--;
