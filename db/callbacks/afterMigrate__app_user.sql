-- Least-privilege database user for the service (local/test environments).
--
-- Enforces the task constraint at the database level: the service may read
-- everything but only write to sync_queue (mark processed) and
-- partner_contacts (store the Acme contact id). Runs after every migrate and
-- is idempotent. Production grants are managed by the schema owner.

CREATE USER IF NOT EXISTS '${app_user}'@'%' IDENTIFIED BY '${app_password}';
ALTER USER '${app_user}'@'%' IDENTIFIED BY '${app_password}';

GRANT SELECT ON `${flyway:database}`.* TO '${app_user}'@'%';
GRANT INSERT, UPDATE ON `${flyway:database}`.`partner_contacts` TO '${app_user}'@'%';
GRANT UPDATE ON `${flyway:database}`.`sync_queue` TO '${app_user}'@'%';
