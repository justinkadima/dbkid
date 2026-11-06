# DBKid

DBKid is a local, browser-based MySQL/MariaDB console written in Go. It is a compact starting point inspired by phpMyAdmin, not a replacement for every phpMyAdmin feature. The web interface is embedded in the executable; running it does not require PHP, Node.js, or a separate web server.

## Run

From the project directory:

```sh
./dbkid
```

Open the local URL printed by the program. The default is `http://127.0.0.1:8080`. To use another loopback port:

```sh
./dbkid -addr 127.0.0.1:8090
```

Stop it with Ctrl+C. The address must be loopback; DBKid refuses to bind to public interfaces.

Enter the MySQL/MariaDB host, port, username and password in the connection form. Local TCP connections default to plaintext. For a non-loopback database host, verified TLS is selected by default; the server certificate must be trusted by the operating system and match the hostname. If you explicitly disable TLS for a remote server, the app asks for confirmation because credentials and database traffic will be unencrypted.

## Build

The source requires Go 1.20 or later, but use a current, fully patched Go toolchain for security (Go 1.27.3 or newer is recommended). Check `go version` before building: an older Go installation may appear first in your PATH. The MySQL driver is a Go module dependency; building for the first time may need network access to download modules.

```sh
CGO_ENABLED=0 go build -trimpath -o dbkid .
```

The resulting executable contains the HTML, CSS and JavaScript and can be run from another directory. A reachable MySQL or MariaDB server is still required; the database server is not included in the executable.

## Included

- Connect to a MySQL/MariaDB server over TCP, with optional verified TLS; recent connections are remembered in memory (host, port, user only — never passwords).
- List databases and their tables/views; inspect columns and indexes; create, rename and drop databases and tables; add, modify and drop columns and indexes.
- Build new tables through a column-builder form: database, table name, storage engine, and per column a name, whitelisted type, length, decimals, NULL allowance, auto increment, primary key, unique and default — validated in the browser before the confirmed request.
- List, add and drop foreign keys in the structure tab (single-column form; composite keys via SQL).
- List database accounts, view SHOW GRANTS, create accounts, set passwords, grant and revoke a curated set of privileges — executed as the connected account, so MySQL enforces what it may do.
- Browse rows with column sorting and up to three filter conditions, edit, copy, insert and delete rows through a row editor (primary-key-addressed), with NULL checkboxes, hex input for binary columns and read-only truncated cells.
- Search the current database across all tables by substring or LIKE.
- Browse and filter stored routines, triggers and events; create and edit procedures, triggers and events using SQL definitions. View server variables, status counters and the process list (kill a process after confirmation).
- Run one SQL statement at a time after an explicit confirmation, with in-session history (up to 100 entries) and an EXPLAIN shortcut. Display up to 500 rows from a query result and report affected rows for statements without a result set.
- The SQL editor highlights keywords, strings, quoted identifiers, numbers and comments as you type. The highlighting is built in — no runtime or CDN dependency.
- Import SQL scripts (with DELIMITER support) and CSV files; export tables as CSV or SQL dumps (structure and/or data).
- Preview images stored in binary columns and download any binary value; downloads are served through single-use, short-lived tokens issued by a confirmed request.
- Distinguish SQL NULL from an empty string, handle quoted identifiers, and render database-supplied text as text rather than HTML.

## Safety and limitations

- DBKid is a local single-user tool, not an authenticated multi-user web service. Do not expose it through a proxy or share it with untrusted users. Use a trusted computer and a least-privilege database account. Local applications under the same operating-system account may have access to process memory.
- Database credentials are held in process memory for the session, not written by DBKid to a config file or placed in the URL. The password field is cleared after a connection attempt. Your database server may have its own logging and audit configuration.
- SQL is powerful. A statement can change or delete data, affect other databases, invoke routines or operate on files if the database account has those privileges. Check the selected server, database and SQL before confirming. A network failure or timeout after execution can leave the outcome unknown; inspect the database before retrying.
- Displayed cells are limited to 4 KiB and marked when shortened. Result pages and database responses also have size limits; an oversized response is rejected rather than silently skipping rows.
- Each SQL request uses a fresh database connection. Transactions, temporary tables and session variables cannot be carried across separate requests. MySQL DDL may also commit implicitly. Multi-statement SQL requests and multiple result sets are not supported; a stored procedure may have performed work before a multiple-result error is reported.
- Editing a procedure, trigger or event replaces it with DROP followed by CREATE on one connection; this is not atomic. Save the original definition before confirming. CREATE can fail after DROP, concurrent DDL can race, routine-specific grants may be lost, and trigger order or event schedules may change. Inspect the server after uncertain outcomes; DBKid does not retry or restore automatically. Replacement is limited to supported CREATE syntax and matching creation context; unsupported definitions are rejected rather than guessed. DBKid requires the readback to exactly match the submitted SQL, so server normalization can produce an unverified outcome even when CREATE succeeded. Text containing `/*!` or `/*M!` is currently rejected even inside a string literal. SQL bodies remain unsandboxed.
- SQL requests carry a unique per-session ID to prevent a repeated HTTP request from executing the same statement twice. If the session reaches its 4,096-request safety limit, disconnect and reconnect.
- Every mutation — row edits, schema changes, imports, process kills — goes through the same explicit-confirmation gate with a per-request replay-protection ID as raw SQL. Dropping tables, databases and accounts also requires typing the exact name; stored object drops require confirmation.
- Account and privilege changes go through the same gate. Passwords are sent once to the database server, never stored, logged, echoed in a response, or recorded in the SQL history; the confirm dialog names only the account.
- If the connected account cannot list database users, the Users tab says so and shows only the account's own grants.
- Browsing uses offset pagination without a guaranteed stable ordering. Concurrent changes, or tables without a unique ordering, can cause rows to move between pages.
- Row actions require a primary key; tables without one are read-only in the grid (the interface says so and points to the SQL editor).
- Not included: the visual schema designer and PDF schema export, themes and multi-language UI. Composite foreign keys and rename-user are SQL-editor territory.
- Connections use TCP only. Refreshing the browser starts a fresh session and requires reconnecting; use one DBKid tab per origin at a time. Use Disconnect before closing or refreshing: an abandoned session's credentials can remain in process memory until session cleanup or the program exits.

## Tests

The standard Go tests do not require a database:

```sh
go test ./...
go vet ./...
```

The optional frontend tests use Node.js with built-in WebSocket support and Chrome/Chromium:

```sh
node --test web/app.test.js
```

A separate opt-in integration test can be run against a disposable MySQL or MariaDB instance only. Do not point test fixtures at an existing or production database. The object editor has been exercised against MySQL 9.6.0 and MariaDB 11.8.9 in isolated fixtures. See `integration_test.go`, `tests/test_integration.py`, `tests/test_v2_mutations.py`, `tests/test_v2_reads.py`, `tests/test_v3_features.py` and `tests/test_objects_editor.py` for their explicit environment requirements; `tests/mysql-fixture.sql` builds the disposable fixture (least-privilege read/write, DDL and admin accounts, sample data, routines and binary values). The admin account exists for privilege-testing on a throwaway loopback instance only.
