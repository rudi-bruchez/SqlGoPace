# The compatibility matrix

`ddl_compatibility.yaml` is what lets one manifest run correctly on SQL Server 2016
Standard and on SQL Server 2022 Enterprise without being edited. It declares, per
operation, which options are eligible, keyed by minimum major version and edition tier,
with `requires` dependencies between them.

```yaml
rebuild_index:
  online:               { min_major: 9,  editions: [enterprise, azure] }
  wait_at_low_priority: { min_major: 12, editions: [enterprise, azure], requires: [online] }
  resumable:            { min_major: 14, editions: [enterprise, azure], requires: [online] }
  sort_in_tempdb:       { min_major: 9,  editions: [enterprise, standard, azure] }
  maxdop:               { min_major: 9,  editions: [enterprise, standard, azure] }
```

It encodes the real rules rather than an approximation: `ONLINE` index builds from 2005 on
Enterprise, `RESUMABLE` rebuild from 2017 and create from 2019, `WAIT_AT_LOW_PRIORITY` on
index operations from 2014, `ONLINE ALTER COLUMN` from 2016, and the fact that
`WAIT_AT_LOW_PRIORITY` is not supported with an online `ALTER COLUMN` on any version.

## Edition tiers

| Tier | Covers |
|---|---|
| `enterprise` | Enterprise, Developer, Evaluation |
| `standard` | Standard, Web, Business Intelligence |
| `express` | Express in all its forms |
| `azure` | Azure SQL Database and Azure SQL Managed Instance |

Azure is treated as an evergreen pseudo-version: it always has the newest feature set, so a
minimum major version is meaningless there.

## What the matrix does not decide

Three kinds of restriction are not a function of version and edition, so they live in the
resolver rather than in this file. They apply on top of whatever the matrix allowed:

- **Database-scoped.** `RESUMABLE` is refused in `tempdb` (Msg 11439) on every version and
  edition, while `ONLINE` alone is accepted there.
- **Combination-scoped.** `SORT_IN_TEMPDB` cannot be combined with `RESUMABLE` (Msg 11438,
  raised at compile time, so the batch fails before doing any work). The resolver keeps
  `RESUMABLE`, which is a rung of the reaction hierarchy, and drops the sort.
- **Statement-shape-scoped.** `RESUMABLE` is not allowed with `ALTER INDEX ALL`, nor when
  rebuilding a single partition; columnstore, XML and spatial indexes reject the whole
  `ONLINE` family.

Each of these produces a decision line explaining itself, which `--explain` prints and every
run writes into its `.log`:

```
--     resumable = OFF  (omitted: RESUMABLE is not supported in tempdb (Msg 11439))
--     sort_in_tempdb = OFF  (omitted: SORT_IN_TEMPDB cannot be combined with RESUMABLE (Msg 11438))
```

## Overriding it

Precedence runs from most to least specific: a per-operation `options:` value, then the
global `options_override` in `config.yaml`, then the matrix, then the safety default. The
matrix decides what is *possible*; the other two decide what is *wanted*.

You can point at a different matrix file with `--matrix <path>`, or `matrix_file` in the
config.
