# Deliver archive reductions as separate pull requests

Status: proposal delivery in progress, refreshed 2026-11-13. The recorded fork
`main` baseline is `4bf12972e19486a59788692b0c832fff0002668e`. This plan
organizes the [compression proposal](database-compression-proposal.md); it does not supersede
its preservation requirements or acceptance gates. The
[concern matrix](database-compression-concerns.md) remains the source map.

## How does newer upstream affect delivery?

The first seven fork PRs retain the version-167 base recorded above: five
feature/measurement PRs, their integration draft, and the additional index
feature. Their results are not measurements of current upstream. The
[November 13 schema audit](database-compression-upstream-audit.md) pins upstream
at `d0d5a77b5168e09250e93cbf3b79b99d709bec20`, data version 179, and owns the
required schema, search, export and auxiliary-storage updates.

Do not raise a version constant or relax a schema guard to claim compatibility.
An archive can retain an old header while a newer writer has installed DDL and
left an upgrade pending. The older proposals now refuse the known newer schema
markers; this does not implement the newer archive contract. See the
[current safety validation](database-compression-integration.md#current-safety-hardening-and-validation)
for exact feature fixes and pending full aggregate validation after the
cancellation follow-up.
Before upstream submission, port each feature's own code to a deliberate
recorded upstream base, audit the actual diff, and rerun its affected
preservation and
performance gates. Keep branches independent and compose only on integration.
Neither this refresh nor the refusal work authorizes a merge into `main`.

## How will the work be divided?

Keep each change independently reviewable. Every feature branch starts from
the fork's `main`, and every initial feature PR targets `main` in that same fork. No
feature branches from another feature or from integration. Test compatible
changes together on a separate integration branch and draft PR in the fork.
A failed experiment can remain a documented result without becoming a
production feature. Any later upstream submissions remain separate, focused
PRs; the aggregate is not an umbrella submission to upstream.

The initial proposals all use the recorded baseline above. Do not update or
merge into `main` as a prerequisite for reviewing them. Keep unaccepted
experimental features out of `main`; combining them for testing happens on
integration. Include reusable
measurement support in the first feature PR when it is directly needed. Split
that support into its
own PR only if it provides an independently useful developer capability; do not
open empty placeholder PRs.

The five initial feature and measurement PRs are open on the fork, with their
combined draft in PR 6. The additional seven-iteration sequence, its original
November 10 research, refreshed upstream requirements, and the stricter
correctness and performance gates are owned by
[additional storage reductions](database-compression-next-steps.md). The
duplicate message-order index is now fork draft PR 7; the remaining six
iterations are queued. Finish its standalone and aggregate review before
implementing the next item. Every additional feature still starts directly
from the recorded fork `main`, independently of the initial proposals and the
other additional features.

| Order | Feature branch | Focus and decision |
| --- | --- | --- |
| 1 | `codex/storage-session-refs` | Reduce repeated conversation-history session identifiers and associated indexes. Preserve exact external identities and every retained history row. Include baseline accounting and standalone migration, reversal and query measurements. |
| 2 | `codex/storage-digest-packing` | Pack canonical digests with exact fallback for other values. Implement and measure it against the layout on `main`, independently of session-reference normalization. Measure their combined benefit separately. |
| 3 | `codex/storage-payload-codec` | Redesign optional per-value compression with bounded decoding and complete reader/writer coverage. Demonstrate search, update, export, mirror and upgrade behavior before enabling the representation. Split reader/recovery support from enablement if the release contract requires it. |
| 4 | `codex/storage-page-compression` | Evaluate page/VFS compression separately. Start with feasibility, licensing, platform builds and lifecycle evidence. A production implementation PR follows only if those gates pass; do not publish licensed source without redistribution rights. |

These names are reserved by this plan; create each feature branch when its
iteration begins so that it uses the correct baseline. The metadata work is
split into two PRs because identifier normalization and digest packing have
different correctness risks and independently measurable savings. Sharing a
file or table does not by itself require a dependency between them.

Each feature must build and work against the recorded `main` independently.
The layout benchmark is useful measurement tooling, not a required prerequisite
merge. The two metadata proposals carry the small format-validation and
maintenance-opening support that each needs; integration reconciles that
overlap and recognizes both format capabilities. No feature includes another
feature's commits. If a later change cannot stand alone, revise its scope
explicitly rather than silently introducing a stack or a prerequisite merge.

## What is the integration branch for?

The local planning branch `codex/storage-integration` was initially created at
upstream commit
`e1fa83ffedb816d0e680d457cc58fa013aa9f94f`. It holds this plan and the dated
research documents. It contains no application changes from the old
v0.46.1-based compression prototype.

Before combining the first feature, confirm that the integration branch uses
the same fork-`main` baseline. If it does not, start a new integration branch
from the fork's recorded `main` and carry over only these planning documents.
Preserve the old branch and its measurements rather than rewriting history.

The current aggregate is `codex/storage-integration-v2`, created directly from
the recorded fork `main`. It carries only the three planning documents from
the old planning branch, then combines the independent feature tips. The old
`codex/storage-integration` remains preserved. See the
[integration evidence](database-compression-integration.md) for implemented
scope, exact revisions, measured costs and remaining release gates.

Merge selected, validated feature-branch tips into this branch to build and
benchmark their combined behavior. A Git branch merge for local aggregate
testing does not merge or approve a GitHub pull request. Merging PRs into the
fork's `main` remains the repository owner's decision; upstream merges remain
the upstream maintainer's decision.

Keep the integration branch and its eventual draft PR in the fork, also
targeting the fork's `main`. The draft is a place to inspect the combined diff
and record the exact tested revisions. Keep it a testing PR rather than the
route for merging all experiments into `main`. Publish it when the first
feature provides a useful aggregate to review. Keep its description aligned
with the current contents and clearly identify experimental features.

**Never start a feature from another feature or the integration branch, and
never merge integration back into a feature.** This keeps the feature's PR
independently reviewable. Planning documents on integration are not a runtime
dependency and need not be copied into every feature PR.

If an integration conflict reveals a feature bug or a necessary compatibility
change, fix and validate it on the responsible feature branch before combining
the updated tip. Keep unavoidable conflict-resolution changes visible and
reviewed on integration. A feature must still work against `main` by itself;
integration-only fixes cannot conceal a broken standalone PR.

Record the fork-`main` base, feature tips and integration commit for each
result. After a feature changes, its old measurements do not validate the new aggregate.
When a feature is accepted into the fork or upstream, use that accepted
implementation in subsequent baselines rather than applying the experimental
copy again. If ancestry becomes confusing after a squash merge, start a new
numbered integration branch from the updated fork `main` and preserve the old
tested branch. Avoid rewriting published history merely to keep one branch
name.

A small fork PR does not guarantee a small upstream PR: the fork's `main` may
contain accepted work that upstream does not have. Before upstream submission,
land required prerequisites upstream first or isolate the feature's changes on
an appropriate upstream base. Inspect the actual upstream comparison. Fork
acceptance is not upstream acceptance, and an unrelated prerequisite must not
silently enter the submitted diff.

## Which combinations should be tested?

Metadata normalization and digest packing should compose if both pass their
individual gates. Payload compression and page compression start as competing
storage approaches, even though both are investigated. Already-compressed
payloads can reduce the additional space benefit from page compression while
retaining both layers' CPU and compatibility costs.

Measure these configurations from equivalent isolated starting fixtures:

| Configuration | What it establishes |
| --- | --- |
| Baseline | Recorded fork `main`, including the intended upstream deduplication and search improvements. |
| Baseline plus session references | Standalone saving and cost of the first metadata change. |
| Baseline plus digest packing | Standalone saving and cost of the second metadata change. |
| Baseline plus payload codec | Standalone codec behavior and costs without either metadata change. |
| Baseline plus page compression | Standalone page/VFS behavior and costs without other experiments. |
| Metadata plus digest packing | Incremental digest benefit and combined metadata behavior. |
| Metadata plus payload codec | Benefit and cost of individual compressed values after metadata changes. |
| Metadata plus page compression | Alternative whole-storage representation without precompressing payloads. |
| Metadata plus both compression layers | Optional comparison only after both implementations independently pass. Include this combination in the primary aggregate only if it proves worthwhile and compatible. |

Use separate integration variants for incompatible alternatives. Do not force
both compression layers into one default merely to say that every PR was
combined. Report byte counts and timings for each configuration; percentages
from different baselines must not be added together.

## What happens in each iteration?

1. Confirm the exact fork-`main` base, applicable project guides and standalone
   feature scope. Resolve genuine prerequisites through the separate process
   above. Preserve the dated source-research snapshot; recheck
   relevant upstream changes before relying on its status claims.
2. Capture baseline storage and performance on isolated scratch fixtures.
   Preserve the original inputs and identify the fixture and configuration in
   the result record. Use synthetic/public fixtures for published regression
   tests; do not commit transcript content or private infrastructure details.
3. Implement the focused change and meaningful regression tests. Cover its
   migration, interruption/restart, reversal, future writes and existing query
   behavior, including all relevant concern-matrix requirements.
4. Run relevant project checks and standalone benchmarks. After Go changes,
   run formatting and vet as the repository requires. Compare against the
   recorded `main` baseline using the same fixtures and conditions.
5. Review adversarially and resolve material findings. Prepare the focused PR
   in the fork, targeting its `main`, with the benefit and limits clearly
   stated. Follow the project's summary-only PR format; retain detailed
   validation in the evidence record.
6. Combine the reviewed feature tip with compatible work on the integration
   branch. Rerun the affected checks plus cross-feature migration, recovery,
   search/export, mirror and concurrency checks. Record the exact aggregate
   commit and incremental as well as total space and performance results.
7. Review the result in the fork and decide whether it needs another iteration,
   should remain experimental or is ready for a separate upstream proposal.
   Start the next feature from `main` after documenting that outcome. Readiness
   for the next iteration does not require merging the current PR into `main`.

Passing each PR separately does not prove the combination is safe. Integration
coverage must include sequential upgrades from older layouts, fresh archives,
partial/interrupted migrations and reversal in dependency order. Allocate
compatible migration identities and test independent changes in both
application orders; two PRs working alone must not collide when combined. A
configuration that cannot support a particular reversal order must refuse it
clearly before mutating data. Preserve logical history and export identities
throughout.

All builds, migrations and benchmarks use scratch data. The integration branch
is a test candidate, not an instruction to replace the installed application or
convert a live archive. Existing backups and experimental artifacts remain
preserved. The proposal's losslessness, queryability, bounded-resource and
release-compatibility gates apply to each feature and to every supported
combination.
