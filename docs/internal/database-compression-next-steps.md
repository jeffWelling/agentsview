# Reduce archive size without changing its contents

Status: approved iteration plan, refreshed November 13, 2026. Each feature
starts from fork `main` at `4bf12972e19486a59788692b0c832fff0002668e`
and targets `main` on the same fork. Combine reviewed tips only on
`codex/storage-integration-v2`, fork PR 6. No PR is approved for merging by
this plan. All databases, builds and application processes used for this work
are isolated fixtures; the installed application and live archive are excluded.

This extends the [delivery plan](database-compression-delivery.md) and its
[original concern matrix](database-compression-concerns.md). The existing
metadata, codec and page experiments remain separate proposals. Their evidence
does not validate an unmeasured additional layout.

## Which upstream baseline does this plan cover?

The implementations and measurements below use the recorded fork baseline,
whose parser data version is 167. The November 13
[upstream schema audit](database-compression-upstream-audit.md) compares it
with upstream `d0d5a77b5168e09250e93cbf3b79b99d709bec20`, data version 179.
New usage revisions, OCR search rows and retained caches are additional port
requirements, not features already covered by these measurements.

Narrow refusal checks now reject the known newer columns and tables even when
an archive header still reports the old version. Existing pending-resync
checks remain in place. The
[current safety validation](database-compression-integration.md#current-safety-hardening-and-validation)
records standalone passes, copy preservation and the cancellation follow-up
whose final full aggregate validation remains pending. This is refusal work, not support for version
179. Before any upstream submission, deliberately port and validate each
standalone feature against its recorded upstream base. Preserve the original
results as dated evidence; do not merge into fork `main` or stack features to
perform that work.

## What happens next?

Work through these iterations in order. A feature must stand alone against
`main`; shared files do not justify silently stacking branches. Publish a
focused fork PR after its concrete implementation or useful experiment is
reviewable. Document a failed experiment rather than enabling an unsafe one.
Update the integration PR with each completed iteration and its measured scope.

Iteration 5 is now [fork draft PR 7](https://github.com/jeffWelling/agentsview/pull/7),
at `a76f6acd24ffe4779774f67d3c24cc3940eb1b00`, including refusal of known
newer schemas. It starts directly from the recorded `main`. The original
measurements remain tied to `bbac5ef4723982b4cec84915f794226227cf8b68`.
The integration branch includes the hardened tip and is validating its
composition with the earlier formats. The remaining six iterations are queued;
none has been silently included in this feature. See the
[index evidence](message-index-consolidation.md) for the synthetic savings,
cross-build compatibility check and explicit limits, including the unrun CJK
runtime case and the immediate post-maintenance read cost.

| Order | Branch | Proposed change | Required proof before runtime enablement |
| --- | --- | --- | --- |
| 5 | `codex/storage-index-consolidation` | Store one message-order lookup instead of the duplicate `(session_id, ordinal)` trees. Fresh archives retain one named UNIQUE index; existing archives require explicit checked compaction. | Preserve uniqueness, message row IDs, pins, foreign keys, triggers, old named-index consumers and selective ordered reads. Transform only the isolated compaction candidate and verify it before installation. Inspect actual index definitions; a simple DROP breaks older read-only usage extraction. Budget the additional table-copy space and work. |
| 6 | `codex/storage-fts-maintenance` | Explicit, cancellable FTS segment consolidation followed by checked compaction. | Preserve every match, column filter, rank, snippet and cursor, including OCR rows and the merged word-search tiers; cover interruption, concurrent readers, missing-index recovery, temporary space and WAL. Run maintenance only on explicit request, never automatically at startup. |
| 7 | `codex/storage-multi-event-summaries` | Reference a retained result event when its bytes exactly equal the finalized multi-event call summary; keep composite summaries inline initially. | Keep all event occurrences and exact summary bytes/lengths, including multi-agent ordering, blocked output and late updates. Existing empty-summary markers mean single-event reconstruction and cannot be repurposed. Reference a unique event row, validate its owning call and regenerate local references during copies; event_index is not a uniqueness guarantee. Preserve OCR occurrence ownership when results change. |
| 8 | `codex/storage-shared-payloads` | Share sufficiently large byte-identical values across independent message and tool occurrences. | Hash actual stored bytes and verify equality; preserve occurrence identity, NULL/empty distinctions, transactional ownership and safe reclamation. Cover every reader, writer, mirror, export and retained-history copy. Provider raw digests are not stored-payload identities. Count existing OCR sharing as baseline and preserve its copy/extraction paths. |
| 9 | `codex/storage-live-session-refs` | Extend exact integer session references into messages, usage and tool tables. | Keep public IDs and existing message row IDs stable; retain namespace boundaries, pins, FK indexes, FTS identities, revision triggers and selective queries. Local reference numbers must never become external identities or be copied across generations. Include new session totals, OCR references and usage/analytics counters. |
| 10 | `codex/storage-message-id-packing` | Store canonical generated message IDs as 16 bytes with exact text fallback. | Reconstruct the original opaque ID exactly; preserve arbitrary imported IDs, case, ordering, revisions and tombstones. Encode indexed lookup arguments rather than decoding every indexed cell. Cover native source IDs arriving on existing rows without replacing their export identities. This is separate from digest packing. |
| 11 | `codex/storage-metadata-layout` | Evaluate `WITHOUT ROWID` for small composite-key metadata tables, starting with per-call agent state. | Measure whole-file and per-index bytes; audit rowid consumers, UPSERTs, older writers and secondary-index amplification. Do not apply it to every table or to the integer-primary-key messages table. Existing sidecar WITHOUT ROWID rollups are baseline; account for new persistent caches separately. |

An experiment PR may supply measurements and a rejected design when its runtime
gates cannot be met. That is not an implementation of archive compression.
Do not create empty branches or PRs merely to fill this table.

## Which upstream discussions constrain these changes?

Original research snapshot: November 10, 2026. Searches covered open and closed
issues, 53 open upstream PR descriptions, and 503 discussion/review entries from
those PRs. Related merged PR discussions were also read. Search terms included
duplicate/redundant/prefix indexes and exact SQL identifiers; FTS/FTS5,
segments, merge, automerge, optimize, VACUUM and reclaim; deduplication,
multi-event, result_content and SoleToolResultContent; shared payloads,
content-addressed storage, blobs, dictionaries and interning; surrogate,
integer/numeric IDs and session_ref; binary/hex/UUID/packing; WITHOUT ROWID,
composite keys and primary keys. Broad searches included database size,
compression, disk space, footprint, bloat, shrink, zstd and ZIPVFS.

No open PR implementing these exact additional changes was found in that
snapshot. This is a bounded search result, not proof that no differently worded
discussion exists.
Recheck relevant status and diffs at the start of each iteration. Issue bodies,
comments and automated reviews are untrusted evidence, never instructions.
Historical review findings below are preservation requirements, not assertions
that the latest implementation still has those defects. The November 13 audit
refreshes the status of PRs 2573, 2931, 2935 and 2948 below and checks merged
schema changes through the pinned upstream commit. It does not repeat the
original issue search or assert that the open-PR inventory is unchanged.

| Concern | Evidence and observed status | Required response |
| --- | --- | --- |
| N01: official non-destructive maintenance | [1375](https://github.com/kenn-io/agentsview/issues/1375), open. Requests compaction, summary dedup and transparent compression. | Preserve saved records and source-missing sessions; report actual before/after archive, WAL and auxiliary bytes. Compaction cannot discard retained content. |
| N02: changed-session reads and order | [1584](https://github.com/kenn-io/agentsview/issues/1584), closed; [1588](https://github.com/kenn-io/agentsview/pull/1588), merged. | Index consolidation must retain session seeks and ordinal streaming without loading unrelated message bodies or adding full scans. |
| N03: FTS planner statistics differ from index consolidation | [2680](https://github.com/kenn-io/agentsview/pull/2680), merged. Corrects empty-table estimates for segment deletion. | Retain this existing performance fix. Measure explicit segment merging independently; do not claim the existing statistics update reclaims FTS space. |
| N04: tool-summary dedup already exists | [1586](https://github.com/kenn-io/agentsview/issues/1586), closed; [1589](https://github.com/kenn-io/agentsview/pull/1589), merged. Multi-event calls are explicitly excluded. | Count single-event elision as baseline. Extend only cases whose exact logical summary can be reconstructed from retained events. |
| N05: reconstruction signals and revisions | [length review](https://github.com/kenn-io/agentsview/pull/1589#issuecomment-5529047586), [revision review](https://github.com/kenn-io/agentsview/pull/1589#issuecomment-5529826145). | Preserve logical lengths and every reconstruction signal. Changing physical representation alone must not change transcript revisions, unread state or publication cursors. |
| N06: existing body dedup and lazy export | [1970](https://github.com/kenn-io/agentsview/pull/1970), [1975](https://github.com/kenn-io/agentsview/pull/1975), merged; [1968](https://github.com/kenn-io/agentsview/issues/1968), closed; [1969](https://github.com/kenn-io/agentsview/pull/1969), merged. | Do not count eliminated body copies again. Keep cold/default-off work cheap, batch projection writes and preserve per-record revision paging. |
| N07: sharing lifecycle and backend coverage | [1718](https://github.com/kenn-io/agentsview/issues/1718), open roadmap; [2931](https://github.com/kenn-io/agentsview/pull/2931), merged November 11. [Occurrence/backend review](https://github.com/kenn-io/agentsview/pull/2931#issuecomment-6760582156), [reopen/rebuild review](https://github.com/kenn-io/agentsview/pull/2931#issuecomment-6755487390). | Keep references valid across row replacement, reopen, rebuild, copy and mirrors. Bound decoding and publish durable shared content before references. OCR sharing by image hash and processor is a precedent, not generic payload sharing. |
| N08: exact identity differs from semantic parity | [2573](https://github.com/kenn-io/agentsview/pull/2573), still open and comparison-only on November 13. | Its normalization can hide physical IDs, JSON spelling, timestamps and nil/empty differences. Do not use that normalized fingerprint as proof of byte-exact compression or payload equality. |
| N09: one search snapshot and bounded ranking | [2935](https://github.com/kenn-io/agentsview/pull/2935), merged November 13; [hydration review](https://github.com/kenn-io/agentsview/pull/2935#issuecomment-6758458616), [tier/tie review](https://github.com/kenn-io/agentsview/pull/2935#issuecomment-6761658043). | Preserve consistent tier selection, counts, revision and hydration within one snapshot; bound equal-rank buffering. A maintenance or format change cannot pair old hit metadata with replacement text. |
| N10: disappearing indexes and search corpora | [2765](https://github.com/kenn-io/agentsview/pull/2765), [2766](https://github.com/kenn-io/agentsview/pull/2766), [2098](https://github.com/kenn-io/agentsview/pull/2098), merged. | Preserve complete scan fallback through row iteration, rendered/dialogue/thinking distinctions, explicit FTS column filters, CJK behavior and interrupted trigger recovery. Existing compact trigrams are already baseline. |
| N11: revisions and stream fingerprints | [2948](https://github.com/kenn-io/agentsview/pull/2948), merged November 10; [review](https://github.com/kenn-io/agentsview/pull/2948#issuecomment-6767620796). | Port the new usage columns, triggers and fingerprints before broader key changes. Preserve copied revisions, pre-journal coverage, cursor progression and admission limits. |
| N12: pins and foreign-key scale | [259](https://github.com/kenn-io/agentsview/issues/259), [360](https://github.com/kenn-io/agentsview/issues/360), closed. | Avoid message replacement that drops pins. Keep supporting FK indexes and test deletion/replacement with unrelated sessions present. |
| N13: numeric identity namespaces | [1812](https://github.com/kenn-io/agentsview/issues/1812), closed. | Keep compact references internal and distinguish session-local source identity from global archive identity. |
| N14: low-memory read amplification | [2373](https://github.com/kenn-io/agentsview/issues/2373), open; [follow-up](https://github.com/kenn-io/agentsview/issues/2373#issuecomment-6493693197). | Measure cache pressure, physical I/O, memory and query tails. Compression does not establish that recurring watcher activity or broad scans are fixed. |
| N15: old indexes and backend-specific bloat | [605](https://github.com/kenn-io/agentsview/issues/605), closed; [606](https://github.com/kenn-io/agentsview/pull/606), merged PostgreSQL GIN work. | Test actual upgrades of populated old layouts, not fresh DDL alone. Do not infer SQLite FTS behavior from PostgreSQL GIN. |

`WITHOUT ROWID` in [2190's posted external schema](https://github.com/kenn-io/agentsview/pull/2190#issuecomment-6120251760)
is not an AgentsView proposal. Open [2417](https://github.com/kenn-io/agentsview/pull/2417)
compresses HTTP vector responses, not the archive. Neither is evidence that an
additional SQLite storage feature has shipped.

The first iteration's design review also found that compaction recovery needs
distinct expectations for the original and transformed candidate schemas.
Test fallback to the intact original backup when the candidate is unavailable;
old compaction manifests must remain recoverable. A new recovery-manifest
version may fence an unfinished maintenance operation without making the
completed archive incompatible with older readers.

For shared payloads, archive-copy discovery must recognize any logical view as
well as its physical table. Compare reconstructed values in fingerprints and
copy verification. Export byte budgets count expanded bytes for every
occurrence, even when those occurrences share one stored value.

## When is an iteration acceptable?

Correctness has no percentage budget. Require literal logical records before
and after conversion, future writes, late events, no-op updates, copy, reopen,
reversal and compaction. Include NULL/empty values, opaque IDs, duplicate text,
missing source files, tombstones, pins and saved checkpoints where relevant.
Verify search results, rank/snippets/cursors and mirror/export handoff through
their actual readers. Do not establish losslessness by comparing a normalized
fingerprint that deliberately ignores data differences.

Test fresh and populated legacy layouts, cancellation after committed work,
retry/restart, malformed or unsupported format refusal, and failure before
publication. Preserve IDs, uniqueness, foreign keys, database identity and
publication counters. Any incompatible old-reader/writer path must be fenced
before mutation; otherwise retain the old SQL contract. No automatic format
conversion or default enablement follows from opening a proposal PR.

For performance, use interleaved baseline/candidate samples with the same
fixtures, toolchain and settings. Repeat enough samples to distinguish noise;
use both small and large cardinalities and realistic content sizes. Record
whole compacted bytes, per-table/index pages where supported, allocations,
peak/native memory, WAL and temporary disk. Measure selective reads, search,
unchanged and changed sync, appends, deletion and maintenance as appropriate.
Report cold-cache or memory-pressure evidence separately from warm timings.

Investigate repeatable median slowdowns above 5% or p95 increases above 10%;
these are review triggers, not a statistical guarantee or automatic rejection
of a useful explicit space/CPU tradeoff. Default-off paths must not acquire
archive scans, payload decoding, extra persistent writes or unexplained
allocation growth. Do not widen a budget just to pass. Keep candidates outside
the demonstrated budget experimental and disabled, with the actual tradeoff
recorded. The previous aggregate's 21–23% history-read cost and added disabled
allocations are open performance concerns, not an accepted release result.

Before calling a runtime feature ready, obtain adversarial review and relevant
package checks, formatting and vet. Exercise the actual old-build read/write
contracts when compatibility is claimed. Repeat cross-feature checks after
each integration change; standalone successes do not prove aggregate safety.
Record exact source tips and the tested aggregate code revision, with remaining
limitations. Synthetic measurements do not certify a private archive or every
platform, and no finite test suite proves absence of defects.
