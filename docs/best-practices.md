# Best practices

Canonical, human-facing product/process policy for Konoha. This is **NOT** build
instruction — it does not gate implementation and must not be enforced as an
engineering requirement (see `AGENTS.md` → `## References`).

## Milestones

Name milestones `<version>-<theme>` so closing a milestone maps 1:1 to a release
tag and the theme signals its intent. Examples in this repo: `v1.0-wire`,
`v2.0-ship`, `v3.0-operate`, `v3.1-govern`. Semver `vX.Y` matches the release
tags from the release-automation workflow; `<theme>` covers one delivery wave.
The same pattern transfers to Jira fixVersions/versions: one version per
release, suffixed by theme when a release spans multiple workstreams.

## Ticket format

Write every ticket so it can be implemented and ACCEPTED without the author's
memory. A ticket must always carry these sections, in this order:

1. **Priority** — `[P0|P1|P2|P3]` (P0 = foundation nailed early; P1 = blocks
   others; P2 = after dependencies; P3 = governance/widen). Keep in the summary
   line so the board reads it without opening the ticket.
2. **Summary** — one paragraph: what, why. Reference the design section
   (`docs/new-design-v2.md §N`) it implements.
3. **Full description** — the actual plan (workflow/activity/config changes),
   so implementation never re-invents it. Include the design note when a ticket
   EXTENDS the design doc beyond the current spec (e.g. adding merge/deploy).
4. **Acceptance criteria** — checkbox list, each independently verifiable:
   - every behavior change has a testable outcome (exactly-once/idempotency,
     fail-closed, terminal state);
   - every fail-closed path asserts **no silent success** (§40/§44 #17);
   - always end with `make test` green + `gofmt` clean;
   - reference the §24 idempotency / §42 dry-run constraints where they apply.
5. **Documentation updates (this ticket)** — any README/AGENTS.md/design changes
   required; never a silent side-effect of implementation.
6. **Estimation** — a task table: `# | Task | CP (1-10 §11 rubric) | Est. time`
   plus a total (CP + days). Size each task so review is one concern per task.

Cross-cutting rules:
- **No ticket without AC** (unnavigable → undeliverable).
- Scope OUT dependencies explicitly ("see #<n>", "separate follow-up") so the
  milestone graph and execution order fall out of the tickets themselves.
- Add tickets to a `<version>-<theme>` milestone (see above); the execution
  order is the milestone's dependency-first list.
- Keep content plan-only when asked ("plan, do not modify"): capture the plan in
  the issue, apply changes only after explicit approval.

## Delivery workflow

1. Update the local branch from `main`.
2. Implement the change.
3. `make test` green (+ `gofmt` clean).
4. Branch `vitdo82/<kebab>`.
5. Push.
6. Open the PR.
7. Close the ticket.

## Non-configurable contract list

These strings/shapes are hard-coded invariants — never config-driven, reworded,
or reordered:

- Idempotency markers: `konoha-parent` / `konoha-child` / `konoha-decision`.
- Agent output parser: `VERDICT:` / `BLOCKING:` / `NITS:` / `CONCLUDE:`.
- §6 workflow state names.
- `GH_*` credentials (see #48).
