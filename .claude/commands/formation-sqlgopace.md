---
description: (Re)generate the SqlGoPace Claude Code training material under formation/
argument-hint: "[english | combined-deck | focus: <theme> | …] (optional)"
---

You are (re)generating the **Claude Code training material** for this project, under `formation/`.
This is the project-tailored, on-demand version of the `formation` skill recipe — reuse that skill's
method, but apply the project specifics below. Optional tweaks from the user: **$ARGUMENTS**
(e.g. `english`, `combined-deck`, `focus: trust-but-verify`; if empty, do a full refresh in French).

## Non-negotiable principle

**Everything must come from what actually happened** — never invent examples, numbers, or events.
Show **both faces**: where the agent helped AND where the developer corrected/steered/verified it. The
through-line: *the developer stays in control; the agent proposes and accelerates, the human decides,
verifies, sets the bar.*

## Inputs to mine (in this order)

1. The **current conversation/session**, if it contains real development work — mine its timeline.
2. The **existing `formation/` materials** (`formation.md`, `slides/`, `exercices.md`) — treat them as
   the prior tailored version: refresh and extend, don't discard their accurate specifics.
3. The repo **artifacts** (open them to quote exact paths/snippets):
   - `docs/specs/MAINTENANCE.md` (the maintenance-mode spec: §2 archi decision, §4.0 cheap inventory,
     §5/§5.4 rules+formulas, §15 frictions, §16 golden path),
   - `maintenance_profile.yaml`, `ddl_compatibility.yaml` (data-not-code),
   - `internal/maint/{profile,decide}.go`, `internal/ddl/{resolve,render,manifest}.go`,
     `internal/mssql/analysis.go`, `internal/report/history.go`, `internal/run/reaction.go`,
     `cmd/sqlgopace/plan.go`,
   - the project **memories** under the memory dir + `MEMORY.md`.
4. **Git history** for the arc (but never restate what Git already shows).
5. Get the **real current test count** before quoting it: run `go test ./...` (and note `go vet` /
   `gofmt` clean) — do not hard-code a number that may be stale.

## Deliverables (under `formation/`, default all three)

1. `formation.md` — trainer guide: why this project is a good case study, audience+objectives, a
   **session timeline through-line** table (request, action, skill acquired), one section per module,
   antipatterns, a prompt cheat-sheet, an artifacts annex, a timed agenda.
2. `slides/` — one **Marp** deck per module (`marp: true`, theme, paginate, header/footer; `---`
   separators; lead slides for key messages; ≤ ~6 bullets/slide) + a `slides/README.md` (order + render
   commands).
3. `exercices.md`, one exercise per module (statement, expected walkthrough, solution and
   validation points, variant) plus a self-assessment grid.

## Module set (keep these 8, same order in guide/slides/exercises)

1. **Context and memory**: targeted reading, `@file`, project memories that survive across phases.
2. **Spec-driven workflow**: converge the spec before coding, and **keep the spec honest** by
   correcting it when the best implementation diverges (M5 columns, M7 materialise versus in-memory).
3. **The critical, fallible reviewer** *(the core module)*: trust but verify, against a source (the
   single-partition RESUMABLE bug, checked through the Microsoft Learn MCP); triaging an external
   opinion (Gemini, Mistral).
4. **Steering the model**: decisions rather than wishes; `AskUserQuestion` and asking first; supplying
   a script as the source; interrupting; pacing by phases.
5. **Harness tooling**: parallel calls; surgical `Edit` versus `Write`; dedicated and deferred tools
   (`ToolSearch`, WebFetch, MCP); the build, test, vet and gofmt loop.
6. **Skills and plugins**: `/claude-hud:setup` (the token-consumption HUD); one skill as one quality
   bar; this command itself.
7. **Quality, safety, consistency**: back up and check the BOM before writing a file; golden and
   round-trip tests; **additive** migration (`IF NOT EXISTS`); **extend without breaking** (wrappers);
   secrets through `.env`.
8. **Elegance and idiom** *(capstone)*: sum types, a narrow consumer-side interface, data rather than
   code, testable pure extraction (`reactionEvent`), reusing rather than refactoring (`--auto`),
   **knowing when to stop** (a bounded scope), a clean `gofmt` and `vet`.

Keep a module only if it has a **real** illustration in the inputs; otherwise drop it, or mark it as
not illustrated.

## Conventions

- **Language**: French by default (the code/product stays English — note it once as a footnote). If
  `$ARGUMENTS` asks for `english`, write the material in English instead.
- If `$ARGUMENTS` contains `combined-deck`, also emit a single concatenated deck
  (`slides/00-…`→`08-…` in order, one front-matter block at the top).
- If `$ARGUMENTS` contains `focus: <theme>`, weight that module deeper while keeping the others.
- Concrete snippets and real file paths so the trainer can open them live. **Never** copy real
  secrets/credentials — placeholders only.

## Process

1. Reconstruct the timeline → through-line table. 2. Inventory + read the artifacts. 3. Map each
notable moment to a module; drop unillustrated ones. 4. Write `formation.md` first (source of truth).
5. Derive the slides + `slides/README.md`. 6. Write `exercices.md`. 7. **Consistency pass**: same
modules/order everywhere, every concept has a real example, both faces present, key messages on lead
slides, no secrets, test counts match the real `go test` run. 8. Summarize files written and offer the
remaining options (combined deck, custom Marp theme/logo, English version).
