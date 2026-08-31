# SqlGoPace documentation

Start at [Getting started](getting-started.md) if you have never run it. Otherwise pick the
page that matches the question you arrived with.

## Using it

| Page | Answers |
|---|---|
| [Getting started](getting-started.md) | Install, create the login, configure, write a first manifest, run it. |
| [Manifest format](manifests.md) | The structure of a manifest: top-level fields, `intent`, `window`, per-operation options, the sidecar files a run writes. |
| [Operations](operations.md) | Every `operation:` value with its fields, from `rebuild_index` to `batch_delete`. |
| [Running](running.md) | Modes and flags, the queue lifecycle, the incident console, stopping a run, and what a re-run repeats. |
| [Configuration](configuration.md) | Every `config.yaml` section and key, with defaults. |
| [Permissions](permissions.md) | The grants each operation needs and why, with T-SQL templates in [`permissions/`](permissions/). |

## Getting the hard parts right

| Page | Answers |
|---|---|
| [Blocking, yielding and kills](blocking-and-kills.md) | The reaction hierarchy, which of the three session-policy features you actually need, and everything about the two killers. |
| [Shrinking files](shrink.md) | `shrink` and `shrink_tempdb`: what they do beyond a bare `DBCC`, what they record, and what they will not do. |
| [The maintenance planner](maintenance-planner.md) | Letting SqlGoPace decide the work: `plan`, `--auto`, the profile, and the shrink feedback loop. |
| [The compatibility matrix](compatibility-matrix.md) | How one manifest runs correctly on 2016 Standard and 2022 Enterprise, and the restrictions the matrix cannot express. |

## Working on it

| Page | Answers |
|---|---|
| [Building](build.md) | Build, versioning, cross-compilation. |
| [Testing](testing.md) | The unit suite, and the integration and end-to-end tests against a container. |
| [`../CONTRIBUTING.md`](../CONTRIBUTING.md) | Conventions, how to earn a claim about SQL Server behaviour, running CI locally. |
| [`../SECURITY.md`](../SECURITY.md) | What privileges the tool holds and where its trust boundary sits. |

## For an AI assistant

| Page | Answers |
|---|---|
| [LLM operator guide](llm-operator-guide.md) | A self-contained brief for a chat or agent that writes manifests on an operator's behalf. Paste it into a conversation or index it for retrieval. |

The repository also ships a Claude Code skill in [`../.claude/skills/`](../.claude/skills/)
that loads the same knowledge automatically.

## Deeper material

[`specs/`](specs/) holds the design documents, which are the source of truth for intended
behaviour, and the implementation plans behind each feature. Consult the relevant one before
changing engine, planner or reaction semantics.

[`reference/`](reference/) holds raw material that is neither product documentation nor
design: research behind an article, a note on `REORGANIZE` locking, a diagnostic query, and
Microsoft's own shrink driver sample kept as the reference this driver was designed against.
