CREATE TABLE `entries_old` (
    `id` TEXT PRIMARY KEY,
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
    `deleted_by` TEXT NOT NULL DEFAULT ''
);
INSERT INTO `entries_old` (`id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`, `deleted_at`, `deleted_by`)
    SELECT `id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`, `deleted_at`, `deleted_by` FROM `entries` WHERE `tenant_id` = 'default';
DROP TABLE `entries`;
ALTER TABLE `entries_old` RENAME TO `entries`;
CREATE UNIQUE INDEX `entries__unique_live_name` ON `entries` (`name`) WHERE `deleted_at` IS NULL;

CREATE TABLE `entry_versions_old` (
    `entry_id` TEXT NOT NULL,
    `version` INTEGER NOT NULL,
    `created_at` DATE NOT NULL,
    `created_by` TEXT NOT NULL,
    `message` TEXT NOT NULL DEFAULT '',
    `changed` TEXT NOT NULL DEFAULT '[]',
    `content` TEXT NOT NULL,
    PRIMARY KEY (`entry_id`, `version`)
);
INSERT INTO `entry_versions_old` (`entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, `content`)
    SELECT `entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, `content` FROM `entry_versions` WHERE `tenant_id` = 'default';
DROP TABLE `entry_versions`;
ALTER TABLE `entry_versions_old` RENAME TO `entry_versions`;

CREATE TABLE `library_meta_old` (
    `key` TEXT PRIMARY KEY,
    `value` TEXT NOT NULL
);
INSERT INTO `library_meta_old` (`key`, `value`)
    SELECT `key`, `value` FROM `library_meta` WHERE `tenant_id` = 'default';
DROP TABLE `library_meta`;
ALTER TABLE `library_meta_old` RENAME TO `library_meta`;
