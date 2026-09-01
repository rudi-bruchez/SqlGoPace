# Security policy

## Supported versions

SqlGoPace is pre-1.0. Only the latest release receives fixes; there are no
maintained older branches. The version a run used is recorded in its `.log`
sidecar and in the SQLite history, so a report can name it precisely.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository: open the
Security tab and choose "Report a vulnerability". That keeps the report private
until a fix exists.

If that option is not offered, open an issue saying only that you have a security
report and asking for a private channel, with no details in it. Either way,
please do not put the details of a security problem in a public issue.

Include the SqlGoPace version, the SQL Server version and edition, the manifest
and the relevant part of `config.yaml` with secrets removed, and what you
observed against what you expected. A reproduction against a throwaway database
is worth more than a description.

Expect an acknowledgement within a few days. This is a small project maintained
by one person, so please allow reasonable time before disclosing publicly.

## What this tool is allowed to do

Understanding the trust model matters more here than in most tools, because
SqlGoPace holds real privileges on a production server by design.

It connects with a login that, depending on which operations you run, may hold
`db_ddladmin`, `db_owner`, `ALTER ANY CONNECTION`, or `sysadmin`. See
[`docs/permissions.md`](docs/permissions.md) for the grant each operation needs
and why. Grant the least of them that your manifests actually use: the document
exists to make that possible rather than reflexive.

It can terminate other sessions. `kill_blocking_sessions`,
`kill_amplifying_maintenance` and `allow_abort_blockers` are all off by default
and each needs `ALTER ANY CONNECTION`. Without the grant they are silent no-ops,
and preflight says so.

It can shrink and check database files, which is why `shrink` and `check_db`
require `db_owner`, and `shrink_tempdb` requires `sysadmin`.

## The trust boundary is the queue directory

SqlGoPace does not parse or execute arbitrary `.sql` files. It generates T-SQL
from typed manifest operations, and object identifiers are quoted and escaped.
That is a deliberate design property, not an accident, and it is what makes the
generated statement predictable.

It is not a sandbox, and four fields are the reason.

`set_raw` and `where_raw` on `batch_update` and `batch_delete` are interpolated
into the generated statement verbatim. They are validated for shape (a raw `SET`
must come with a self-limiting predicate, a missing predicate must be confirmed
explicitly) but their SQL text is not inspected or sanitised.

`type` on `add_column` and `alter_column`, and `data_compression` on
`rebuild_index`, `create_index` and `rebuild_heap`, are pasted into the DDL the
same way, and are checked less than that: `type` only for being non-empty,
`data_compression` not at all — nothing restricts it to `NONE`, `ROW` or `PAGE`.
They read like enumerations and are not, which is the likelier way to be
surprised by them.

So a manifest is a trusted input, equivalent in privilege to a script the
connected login could run itself. Write access to the `to_run` directory is
therefore equivalent to that login's rights on the server. Treat it accordingly:
restrict who can write there, and review manifests the way you would review a
script you are about to run in production. `--dry-run --explain` renders the
exact statements without executing or taking a lock, which is the intended way
to review one.

A manifest that does damage is not a vulnerability in SqlGoPace. A path by which
something other than an authorised manifest causes SqlGoPace to execute
statements it did not generate is.

## Secrets

Credentials never belong in `config.yaml`. The file carries `${VAR}` references
that are expanded from the environment and from an optional `.env` file, which
is gitignored and must stay that way. A real environment variable always wins
over the file, so an operator can override one key for one command without
editing anything.

The connection string reaches the driver and nothing else. It is not part of any
report structure, and the failure paths were checked too: a host that does not
resolve and a malformed connection string both produce an error naming the
problem and not the credentials. If you find a connection string or a password in
a `.log` sidecar, in the SQLite history, in a webhook payload or in an email
notification, that is a bug worth reporting privately.
