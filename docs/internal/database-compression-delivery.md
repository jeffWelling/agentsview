# Deliver archive reductions as separate pull requests

Status: agreed delivery workflow, 2026-11-09. Implementation and measurements
remain pending. This plan organizes the
[compression proposal](database-compression-proposal.md); it does not supersede
its preservation requirements or acceptance gates. The
[concern matrix](database-compression-concerns.md) remains the source map.

## How will the work be divided?

Develop one change at a time, give each a focused feature PR, and test compatible
changes together on an integration branch. A failed experiment can remain a
documented result without becoming a production feature. Do not make a
single umbrella PR the upstream delivery path.

Before the first implementation, record a reproducible baseline from an exact
upstream commit and isolated fixtures. Include reusable measurement support in
the first feature PR when it is directly needed. Split that support into its
own PR only if it provides an independently useful developer capability; do not
open empty placeholder PRs.

| Order | Feature branch | Focus and decision |
| --- | --- | --- |
| 1 | `codex/storage-session-refs` | Reduce repeated conversation-history session identifiers and associated indexes. Preserve exact external identities and every retained history row. Include baseline accounting and standalone migration, reversal and query measurements. |
| 2 | `codex/storage-digest-packing` | Pack canonical digests with exact fallback for other values. Measure its incremental benefit separately from session-reference normalization. Declare a dependency on the first PR if they share the new physical layout. |
| 3 | `codex/storage-payload-codec` | Redesign optional per-value compression with bounded decoding and complete reader/writer coverage. Demonstrate search, update, export, mirror and upgrade behavior before enabling the representation. Split reader/recovery support from enablement if the release contract requires it. |
| 4 | `codex/storage-page-compression` | Evaluate page/VFS compression separately. Start with feasibility, licensing, platform builds and lifecycle evidence. A production implementation PR follows only if those gates pass; do not publish licensed source without redistribution rights. |

These names are reserved by this plan; create each feature branch when its
iteration begins so that it uses the correct baseline. The metadata work is
split into two PRs because identifier normalization and digest packing have
different correctness risks and independently measurable savings. If the
second change needs the first, retain that dependency explicitly.

## What is the integration branch for?

`codex/storage-integration` starts at upstream commit
`e1fa83ffedb816d0e680d457cc58fa013aa9f94f`. It holds this plan and the dated
research documents. It contains no application changes from the old
v0.46.1-based compression prototype.

Merge selected, validated feature-branch tips into this branch to build and
benchmark their combined behavior. A Git branch merge for local aggregate
testing does not merge or approve a GitHub pull request. Upstream PR merges
remain the maintainer's decision.

Keep the integration branch and its eventual draft PR in the fork. The draft
is a place to inspect the combined diff and record the exact tested revisions;
it is not a replacement for focused upstream PRs. Publish it when the first
feature provides a useful aggregate to review. Keep its description aligned
with the current contents and clearly identify experimental features.

Feature branches start from the recorded upstream base or from a declared
feature dependency. **Never start a feature from the integration branch.**
Otherwise the feature PR inherits unrelated work and cannot be reviewed or
accepted independently. Planning documents on the integration branch are not
a runtime dependency and need not be copied into every feature PR.

Independent PRs target upstream `main`. For a genuinely dependent change,
develop on the preceding feature branch and identify that dependency. While
the prerequisite is unmerged, use a stacked comparison in the fork or wait to
submit upstream until the prerequisite lands. Preserve the focused diff when
moving to the accepted upstream base. Do not silently include earlier pending
features in a PR described as standalone.

Record the upstream base, feature tips and integration commit for each result.
After a feature changes, its old measurements do not validate the new aggregate.
If upstream squash-merges a feature, use the accepted upstream implementation
in subsequent integration cycles rather than applying the experimental copy
again. When branch ancestry becomes confusing, create a new numbered
integration branch from the new upstream base and preserve the old tested
branch. Avoid rewriting published history merely to keep one branch name.

## Which combinations should be tested?

Metadata normalization and digest packing should compose if both pass their
individual gates. Payload compression and page compression start as competing
storage approaches, even though both are investigated. Already-compressed
payloads can reduce the additional space benefit from page compression while
retaining both layers' CPU and compatibility costs.

Measure these configurations from equivalent isolated starting fixtures:

| Configuration | What it establishes |
| --- | --- |
| Baseline | Current upstream behavior, including existing deduplication and search indexes. |
| Baseline plus session references | Standalone saving and cost of the first metadata change. |
| Metadata plus digest packing | Incremental digest benefit and combined metadata behavior. If independent, also measure digest packing alone. |
| Metadata plus payload codec | Benefit and cost of individual compressed values after metadata changes. |
| Metadata plus page compression | Alternative whole-storage representation without precompressing payloads. |
| Metadata plus both compression layers | Optional comparison only after both implementations independently pass. Include this combination in the primary aggregate only if it proves worthwhile and compatible. |

Use separate integration variants for incompatible alternatives. Do not force
both compression layers into one default merely to say that every PR was
combined. Report byte counts and timings for each configuration; percentages
from different baselines must not be added together.

## What happens in each iteration?

1. Confirm the exact upstream base, applicable project guides, feature scope
   and dependencies. Preserve the dated source-research snapshot; recheck
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
   baseline or declared prerequisite using the same fixtures and conditions.
5. Review adversarially and resolve material findings. Prepare the focused PR
   with its benefit, limits and dependency clearly stated. Follow the project's
   summary-only PR format; retain detailed validation in the evidence record.
6. Combine the reviewed feature tip with compatible work on the integration
   branch. Rerun the affected checks plus cross-feature migration, recovery,
   search/export, mirror and concurrency checks. Record the exact aggregate
   commit and incremental as well as total space and performance results.
7. Decide whether the measured result is ready for upstream review, needs
   another iteration or should remain experimental. Start the next feature
   only after documenting that outcome and any dependency it introduces.

Passing each PR separately does not prove the combination is safe. Integration
coverage must include sequential upgrades from older layouts, fresh archives,
partial/interrupted migrations and reversal in dependency order. A configuration
that cannot support a particular reversal order must refuse it clearly before
mutating data. Preserve logical history and export identities throughout.

All builds, migrations and benchmarks use scratch data. The integration branch
is a test candidate, not an instruction to replace the installed application or
convert a live archive. Existing backups and experimental artifacts remain
preserved. The proposal's losslessness, queryability, bounded-resource and
release-compatibility gates apply to each feature and to every supported
combination.
