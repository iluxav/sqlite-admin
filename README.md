# SQLite client

A Go admin UI for browsing SQLite, staging edits, running SQL, and managing backups. Embed it as a library or run the standalone CLI. The HTML, HTMX, CSS, and fonts are compiled into the binary.

## Standalone CLI

Build from the repository root with Go 1.26 or newer:

```bash
go build -buildvcs=false -o bin/sqliteadmin ./cmd/sqliteadmin
```

The CLI bundles the SQLite driver and opens an **existing** database. It starts only the admin UI. To try it with the demo database and credentials you already configured:

```bash
# Use 8082 when the demo runner already occupies 8081.
./bin/sqliteadmin serve --db runner/demo.db --env-file runner/.env --addr 127.0.0.1:8082
```

Open **http://127.0.0.1:8082** and stop with Ctrl-C. The default listen address is `127.0.0.1:8081`. Add `--read-only` to disable mutations. Local and S3 backups, restore, and the built-in scheduler work as they do in the embedded UI. Run only one scheduler owner for a database/backup folder; stop the demo runner before using the same database for scheduled CLI backups.

For your own database, copy the root `.env.example` to `.env`, set an existing `SQLITEADMIN_DB` path and your credentials, then run `./bin/sqliteadmin serve`. `serve` is also the default command. Settings are read at startup, so restart after configuration changes.

| Flag | Environment / default |
| --- | --- |
| `--db` | `SQLITEADMIN_DB`; required |
| `--addr` | `SQLITEADMIN_ADDR`; `127.0.0.1:8081` |
| `--read-only` | `SQLITEADMIN_READ_ONLY`; `false` |
| `--backup-dir` | `SQLITEADMIN_BACKUP_DIR`; `<database path>.backups/` |
| `--env-file` | `.env` in the working directory; `--env-file=` disables loading |
| `--state-dir` | Private per-database directory under `$XDG_STATE_HOME/sqliteadmin`, or `~/.local/state/sqliteadmin` |
| `--log-file` | Background mode only; `daemon.log` in the instance directory |

Flags override environment settings; exported variables override dotenv values. A missing default `.env` is allowed, but an explicitly named missing file is an error. Relative paths are relative to the working directory, including paths written inside the env file. Credentials use `SQLITEADMIN_USER` and `SQLITEADMIN_PASSWORD`; they are never passed as command-line flags. The `SQLITEADMIN_S3_*` variables are shared with the library.

### Background mode

On Linux and macOS, use the built binary:

```bash
./bin/sqliteadmin start --db /path/to/app.db --env-file /path/to/app.env
./bin/sqliteadmin status --db /path/to/app.db
./bin/sqliteadmin stop --db /path/to/app.db
```

`start` detaches from the terminal, waits for the UI and control socket to be ready, and prints the address and log path. Startup errors, such as an occupied port, return a failure. `status` prints the live process, address, database, mode, and start time; its exit code is 3 when stopped. `stop` waits for graceful shutdown and is safe to repeat. Shutdown allows 30 seconds for active requests; the scheduler is cancelled and database connections are closed.

Instance directories are private (`0700`); new logs and control sockets use `0600`. A held file lock prevents duplicate CLI instances in the same state directory. `status` and `stop` use the private socket, not a PID file, and also work for a foreground CLI instance. A crashed process releases its lock; the next start replaces the stale socket. If you override `--state-dir`, pass the same directory to management commands. Log files append across restarts; rotate them as needed.

Background mode does not install a service or automatically restart after a crash or reboot. For supervised operation, run `serve` under a service manager. An example [systemd user service](contrib/sqliteadmin.service) is included. It uses `~/.local/bin/sqliteadmin` and `~/.config/sqliteadmin/env`; set absolute database and backup paths in that env file. Install and enable the unit only when you want a persistent service. Manage a supervised instance with `systemctl --user`, so the service manager stays in control of its lifecycle. See the [systemd service documentation](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html).

The root module owns the CLI and its driver dependencies. The `sqliteadmin/` library remains a separate module without a driver dependency. Build/install this source checkout locally; versioned `go install ...@version` distribution requires publishing the nested library version and replacing the local development module replacement.

## Run the demo

```bash
cd runner
# On a fresh checkout, copy .env.example to .env and edit the settings.
cp -n .env.example .env
go run . -app 127.0.0.1:0
```

Open **http://127.0.0.1:8081**. The demo application gets an available port; the admin UI uses 8081. Use `-admin 127.0.0.1:8082` if that port is occupied. Restart the runner after source changes to reload embedded assets.

The runner loads `.env` from its working directory at startup. Edit `runner/.env` to set the admin login, local backup directory, and optional S3 credentials; `.env.example` lists the supported settings. Change the example password before using it. Single-quote secrets containing `$` or `#` to keep them literal. The local `.env` file is ignored by Git.

Existing exported environment variables take precedence over `.env`, including empty values. If you previously exported credentials, run `unset SQLITEADMIN_USER SQLITEADMIN_PASSWORD` to use the file instead. A missing `.env` is allowed when you supply settings through the environment. Restart the runner after changing configuration. The CLI and demo runner load dotenv files; embedded library users configure the library through `Config` or the environment.

## Backups and restore

Open **Backups** in the sidebar. **Back up now** creates a complete SQLite snapshot of committed data, including schema, triggers, indexes, implicit rowids, blobs, and database metadata. Staged edits are included only after you commit them.

Local storage is always available. By default, snapshots and `schedule.json` live beside the database in `<database path>.backups/`. Set a persistent folder in the runner's `.env` or export it before starting the app to override it:

```bash
export SQLITEADMIN_BACKUP_DIR=/path/to/persistent/backups
```

Local files can be downloaded from the snapshots list. Newly created directories use mode `0700`, and snapshot and schedule files use `0600`. Snapshots are not encrypted by this library; protect the backup storage as you protect the original database.

### S3-compatible storage

Set all four required variables in the runner's `.env` or export them to show S3 as an additional destination:

```bash
export SQLITEADMIN_S3_ENDPOINT=https://your-s3-endpoint.example
export SQLITEADMIN_S3_BUCKET=database-backups
export SQLITEADMIN_S3_ACCESS_KEY_ID=your-access-key
export SQLITEADMIN_S3_SECRET_ACCESS_KEY=your-secret-key

# Optional:
export SQLITEADMIN_S3_REGION=us-east-1
export SQLITEADMIN_S3_PREFIX=sqliteadmin/my-app
# export SQLITEADMIN_S3_SESSION_TOKEN=your-temporary-session-token
```

Use the provider's region (`auto` where required by your provider). The default region is `us-east-1`, and the default prefix is `sqliteadmin/<database filename>/`. Give each database its own prefix. The bucket must already exist. Credentials need `s3:ListBucket` for that prefix and `s3:PutObject` / `s3:GetObject` for snapshots. The library uses path-style requests and AWS Signature V4; credentials stay on the server. Use HTTPS for remote endpoints; HTTP is supported for local S3-compatible services.

S3 backups are built and checked locally before upload. Successful uploads remove the temporary local copy. If upload fails, a complete local backup is kept and the UI reports the failure. An incomplete S3 configuration shows a message while local backups remain available. Single-object S3 uploads are limited to 5 GiB; multipart upload is not implemented.

### Periodic backups

Enable **Automatic backups**, choose **Every hour**, **Every day**, **Every week**, or a **Custom interval** in minutes, and click **Save schedule**. When S3 is configured, you can also choose the destination. Scheduling is off until enabled.

The scheduler runs in the host process, including when you serve `Admin.Handler()` yourself. Settings, the next deadline, and the last result persist in the backup folder. After downtime, an overdue schedule runs once and schedules the next interval from that run; it does not replay every missed period. Jobs do not overlap. A failed run is recorded and retries at the next interval. Keep one scheduler owner per database/backup folder. No backups are automatically deleted; manage retention in local storage or with an S3 lifecycle rule.

### Restore

Choose **Restore…** beside a saved snapshot, or upload a SQLite backup using **Restore from a file**. Uploads are checked first and do not change the database. Type the current database filename in the confirmation dialog to apply a restore.

Before restoring:

- Commit or discard staged changes in every active admin session.
- Pause writes in the host application so it does not write new data during recovery.
- Use a self-contained SQLite snapshot, not a copy of a live database with an omitted WAL file.

Restore checks the file's SQLite header and integrity, makes a local `safety-*.sqlite` snapshot of the current database, and then uses SQLite's online backup API to restore into the existing database. It does not rename or overwrite an open database file outside SQLite. Other admin requests wait during restore, and SQLite coordinates the database transaction with host connections. A safety-backup failure cancels the restore. Interrupted/failed SQLite copies roll back unless the copy has already completed. A successful restore reloads the UI because the schema may have changed.

The safety snapshot remains available in **Backups**. The scheduler's configuration is stored separately and is not rolled back with the database. For a destination in WAL mode, the snapshot must have the same SQLite page size. Uploads and S3 restore downloads default to a 1 GiB limit. Pending uploads are session-bound, expire after one hour, and are removed when replaced or on graceful shutdown. Backup creation, scheduling, and restore are disabled in read-only mode; existing backups can still be downloaded.

## Embed in a Go application

```go
admin, err := sqliteadmin.New(sqliteadmin.Config{
    Path: "data/app.db",
    // Credentials default to SQLITEADMIN_USER / SQLITEADMIN_PASSWORD.
    BackupDir: "data/backups", // optional; overrides the environment
})
if err != nil { log.Fatal(err) }
go admin.ListenAndServe()
defer admin.Shutdown(context.Background())
```

The library reuses the SQLite driver registered by the host and does not add a driver dependency. Online backup/restore adapters support modernc's `NewBackup`/`NewRestore` and mattn's `Backup` API. Other drivers can supply `Config.BackupCopy` and `Config.RestoreCopy` callbacks; unsupported copy APIs return an explicit error without changing the database.

Other optional settings:

| Config field | Default / behavior |
| --- | --- |
| `S3 *S3Config` | `nil` reads the S3 environment variables; an empty config disables S3 |
| `BackupTimeout` | 5 minutes per backup/restore operation |
| `MaxRestoreBytes` | 1 GiB per upload or S3 restore/download |
| `ReadOnly` | Disables mutations, including backup scheduling and restore |

Call `Shutdown` to stop the scheduler and release database resources. When serving `Handler()` with your own HTTP server, allow enough read/write timeout for uploads and backups.

## Checks

```bash
go test -race ./...
go vet ./...
cd sqliteadmin
go test -race ./...
go vet ./...
cd ../runner
go test -race ./...
go vet ./...
```

Integration tests use disposable databases, processes, and a local S3 test server. CLI tests cover configuration precedence, readiness, duplicate starts, startup failures, control-socket isolation, graceful shutdown, and crash recovery. Library/runner tests exercise WAL backups, live-connection restore, safety snapshots, uploaded files, scheduler restart, read-only/CSRF guards, S3 failures, and SQLite metadata preservation. Signature tests use [AWS's published test vectors](https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sig-v4-header-based-auth.html); database copying follows the [SQLite online backup API](https://www.sqlite.org/c3ref/backup_finish.html).
