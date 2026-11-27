-- Drop leftover go-ldap-admin v1 sync_revision triggers that are unused in v2.
-- Leaving them active adds extra D1 row writes and inflates meta.changes when tables are mutated.
DROP TRIGGER IF EXISTS sync_revision_apis_insert
--;
DROP TRIGGER IF EXISTS sync_revision_apis_update
--;
DROP TRIGGER IF EXISTS sync_revision_apis_delete
--;
DROP TRIGGER IF EXISTS sync_revision_casbin_rule_insert
--;
DROP TRIGGER IF EXISTS sync_revision_casbin_rule_update
--;
DROP TRIGGER IF EXISTS sync_revision_casbin_rule_delete
--;
DROP TRIGGER IF EXISTS sync_revision_certificate_authorities_insert
--;
DROP TRIGGER IF EXISTS sync_revision_certificate_authorities_update
--;
DROP TRIGGER IF EXISTS sync_revision_certificate_authorities_delete
--;
DROP TRIGGER IF EXISTS sync_revision_field_relations_insert
--;
DROP TRIGGER IF EXISTS sync_revision_field_relations_update
--;
DROP TRIGGER IF EXISTS sync_revision_field_relations_delete
--;
DROP TRIGGER IF EXISTS sync_revision_group_users_insert
--;
DROP TRIGGER IF EXISTS sync_revision_group_users_update
--;
DROP TRIGGER IF EXISTS sync_revision_group_users_delete
--;
DROP TRIGGER IF EXISTS sync_revision_groups_insert
--;
DROP TRIGGER IF EXISTS sync_revision_groups_update
--;
DROP TRIGGER IF EXISTS sync_revision_groups_delete
--;
DROP TRIGGER IF EXISTS sync_revision_menus_insert
--;
DROP TRIGGER IF EXISTS sync_revision_menus_update
--;
DROP TRIGGER IF EXISTS sync_revision_menus_delete
--;
DROP TRIGGER IF EXISTS sync_revision_oauth_connectors_insert
--;
DROP TRIGGER IF EXISTS sync_revision_oauth_connectors_update
--;
DROP TRIGGER IF EXISTS sync_revision_oauth_connectors_delete
--;
DROP TRIGGER IF EXISTS sync_revision_revoked_certificates_insert
--;
DROP TRIGGER IF EXISTS sync_revision_revoked_certificates_update
--;
DROP TRIGGER IF EXISTS sync_revision_revoked_certificates_delete
--;
DROP TRIGGER IF EXISTS sync_revision_role_menus_insert
--;
DROP TRIGGER IF EXISTS sync_revision_role_menus_update
--;
DROP TRIGGER IF EXISTS sync_revision_role_menus_delete
--;
DROP TRIGGER IF EXISTS sync_revision_roles_insert
--;
DROP TRIGGER IF EXISTS sync_revision_roles_update
--;
DROP TRIGGER IF EXISTS sync_revision_roles_delete
--;
DROP TRIGGER IF EXISTS sync_revision_user_certificates_insert
--;
DROP TRIGGER IF EXISTS sync_revision_user_certificates_update
--;
DROP TRIGGER IF EXISTS sync_revision_user_certificates_delete
--;
DROP TRIGGER IF EXISTS sync_revision_user_roles_insert
--;
DROP TRIGGER IF EXISTS sync_revision_user_roles_update
--;
DROP TRIGGER IF EXISTS sync_revision_user_roles_delete
--;
DROP TRIGGER IF EXISTS sync_revision_users_insert
--;
DROP TRIGGER IF EXISTS sync_revision_users_update
--;
DROP TRIGGER IF EXISTS sync_revision_users_delete
