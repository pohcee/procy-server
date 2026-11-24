CREATE TABLE `entries_new` (
    `tenant_id` TEXT NOT NULL DEFAULT 'default',
    `id` TEXT NOT NULL,
    `name` TEXT NOT NULL,
    `description` TEXT NOT NULL DEFAULT '',
    `tags` TEXT NOT NULL DEFAULT '[]',
    `os` TEXT NOT NULL DEFAULT '[]',
    `active_version` INTEGER NOT NULL,
    `builtin_key` TEXT NULL,
    `created_at` DATE NOT NULL,
    `created_by` TEXT NOT NULL,
    `updated_at` DATE NOT NULL,
    `updated_by` TEXT NOT NULL,
    `deleted_at` DATE NULL,
    `deleted_by` TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (`tenant_id`, `id`)
);
INSERT INTO `entries_new` (`tenant_id`, `id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`, `deleted_at`, `deleted_by`)
    SELECT 'default', `id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`, `deleted_at`, `deleted_by` FROM `entries`;
DROP TABLE `entries`;
ALTER TABLE `entries_new` RENAME TO `entries`;
CREATE UNIQUE INDEX `entries__unique_live_name` ON `entries` (`tenant_id`, `name`) WHERE `deleted_at` IS NULL;

CREATE TABLE `entry_versions_new` (
    `tenant_id` TEXT NOT NULL DEFAULT 'default',
    `entry_id` TEXT NOT NULL,
    `version` INTEGER NOT NULL,
    `created_at` DATE NOT NULL,
    `created_by` TEXT NOT NULL,
    `message` TEXT NOT NULL DEFAULT '',
    `changed` TEXT NOT NULL DEFAULT '[]',
    `content` TEXT NOT NULL,
    PRIMARY KEY (`tenant_id`, `entry_id`, `version`)
);
INSERT INTO `entry_versions_new` (`tenant_id`, `entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, `content`)
    SELECT 'default', `entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, `content` FROM `entry_versions`;
DROP TABLE `entry_versions`;
ALTER TABLE `entry_versions_new` RENAME TO `entry_versions`;

CREATE TABLE `library_meta_new` (
    `tenant_id` TEXT NOT NULL DEFAULT 'default',
    `key` TEXT NOT NULL,
    `value` TEXT NOT NULL,
    PRIMARY KEY (`tenant_id`, `key`)
);
INSERT INTO `library_meta_new` (`tenant_id`, `key`, `value`)
    SELECT 'default', `key`, `value` FROM `library_meta`;
DROP TABLE `library_meta`;
ALTER TABLE `library_meta_new` RENAME TO `library_meta`;
