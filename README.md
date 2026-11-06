# DBKid

A modern, local alternative to phpMyAdmin for everyday MySQL and MariaDB work.

DBKid is a single Go executable with its browser interface built in. Run it on your computer and connect to your database: no PHP installation or separate web server required. It doesn't aim to replicate every phpMyAdmin feature.

## What you can do

- Browse databases and tables, filter and sort rows, and edit data in tables with a primary key.
- Run SQL, inspect table structure, and manage databases, tables, columns and indexes.
- Import SQL scripts and CSV files; export tables as SQL or CSV.
- Browse stored routines, triggers and events, and create or edit supported definitions.

## Get started

Download the archive for your operating system and architecture from [GitHub Releases](https://github.com/justinkadima/dbkid/releases), then extract it. The release includes SHA-256 checksums; the binaries are not code-signed.

On macOS or Linux, run:

```sh
./dbkid
```

On Windows, run `.\dbkid.exe` in PowerShell. Open the URL printed by the program (by default, `http://127.0.0.1:8080`) and enter your database connection details. You need a reachable MySQL or MariaDB server; DBKid doesn't include one. Press Ctrl+C to stop it.

### Build from source

With a current, fully patched Go toolchain installed, run this from the project directory:

```sh
CGO_ENABLED=0 go build -trimpath -o dbkid .
./dbkid
```

The first build may need network access to download Go modules.

## Safety and limits

DBKid is a local, single-user tool, not an authenticated service. It binds only to loopback; don't expose it through a proxy or share it with untrusted users. Use a trusted computer and a least-privilege database account. Remote database connections use verified TLS by default.

SQL and other operations can change or delete data. Check the server, database and operation before confirming. Row editing requires a primary key. Separate SQL requests don't share transactions or session state. Editing a stored procedure, trigger or event replaces it with DROP then CREATE, which is not atomic; save the original definition first.

For more detail, see [InternalDoc.md](InternalDoc.md).
