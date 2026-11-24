# Database migrations

Each rportd SQLite database has one migration set. The SQL files are embedded
into the binary with `//go:embed` and exposed as an `fs.FS`:

| Database           | Package                                        | Files                                                |
|--------------------|------------------------------------------------|------------------------------------------------------|
| `<name>.db`        | `db/migration/<name>` (`<name>.FS`)            | `db/migration/<name>/sql/`                           |
| `notifications.db` | `server/notifications/repository/sqlite` (`sqlite.Migrations`) | `server/notifications/repository/sqlite/migrations/` |

`migration.All()` (`db/migration/sets.go`) lists every set. `db/migration/dummy`
is used by tests only.

Migrations are run by [golang-migrate](https://github.com/golang-migrate/migrate)
through `db/sqlite`:

- `sqlite.New(path, fs, opts)` opens a file and migrates it to the latest version.
- `sqlite.Open(path, opts)` opens without migrating; `sqlite.Migrate(db, fs)` migrates;
  `sqlite.CheckVersion(db, fs)` fails if the database is dirty or behind.

No code generation is needed.

## Adding a migration

1. Add a pair of files to the set's directory, numbered after the last one:
   `NNN_description.up.sql` and `NNN_description.down.sql` (the down file may be empty).
2. Run `go test ./db/migration/`. `TestMigrationManifest` fails and prints the
   expected manifest; copy it into `db/migration/testdata/manifest.txt`.
3. Run the tests of the package that uses the database.

Never edit, rename or renumber a migration that has been released: databases in
the field have already applied it. The manifest test pins the name and SHA-256
of every file to catch this; change an existing line only for a migration that
has never been released.

## Adding a database

Create `db/migration/<name>/sql/` with the SQL files and a `migrations.go` like
the existing ones, add the set to `All()` in `sets.go`, and update the manifest.
