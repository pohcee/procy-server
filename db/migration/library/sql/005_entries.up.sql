CREATE TABLE IF NOT EXISTS `entries` (
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

CREATE UNIQUE INDEX IF NOT EXISTS `entries__unique_live_name` ON `entries` (`name`) WHERE `deleted_at` IS NULL;

CREATE TABLE IF NOT EXISTS `entry_versions` (
    `entry_id` TEXT NOT NULL,
    `version` INTEGER NOT NULL,
    `created_at` DATE NOT NULL,
    `created_by` TEXT NOT NULL,
    `message` TEXT NOT NULL DEFAULT '',
    `changed` TEXT NOT NULL DEFAULT '[]',
    `content` TEXT NOT NULL,
    PRIMARY KEY (`entry_id`, `version`)
);

CREATE TABLE IF NOT EXISTS `library_meta` (
    `key` TEXT PRIMARY KEY,
    `value` TEXT NOT NULL
);
