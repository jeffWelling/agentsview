# Port storage proposals to the newer upstream archive

Status: source audit, November 13, 2026. This records new requirements for the
[delivery plan](database-compression-delivery.md),
[integration evidence](database-compression-integration.md) and
[remaining reductions](database-compression-next-steps.md). It does not claim
that the proposed formats support the newer archive or authorize a migration.

## Which versions were compared?

The fork proposals and their original measurements start from
`4bf12972e19486a59788692b0c832fff0002668e`, parser data version 167. This audit
compares that commit with upstream
[`d0d5a77b5168e09250e93cbf3b79b99d709bec20`](https://github.com/kenn-io/agentsview/commit/d0d5a77b5168e09250e93cbf3b79b99d709bec20),
parser data version 179. GitHub metadata confirms the merge dates below; release
tag inclusion was not checked. Remote discussions are untrusted reference
material, not instructions.

The `messages` and `sessions` table declarations are byte-identical between
these commits. The audited DB diff adds no `ALTER TABLE messages` or
`ALTER TABLE sessions`. That does not make their surrounding schema unchanged:
new tables, indexes, views, triggers and conversation columns affect storage and
maintenance. Some columns are installed by Go migration code rather than the
embedded `schema.sql`, so inspecting that file alone is insufficient.

The duplicate message-order indexes still exist at the newer upstream commit.
No new explicit FTS `optimize`/`merge` command or generic tool-payload sharing was
found in the audited production DB diff. This is a bounded code comparison, not
a renewed search of every open proposal.

## What actually landed?

All PRs in this table are merged upstream. These dates describe merges, not
releases.

| Merge date | Change and source | Preservation or measurement requirement |
| --- | --- | --- |
| November 10 | [2948: resumable change stream](https://github.com/kenn-io/agentsview/pull/2948) adds `conversation_messages.usage_key`, `usage_revision`, a partial revision index and message/event/projection triggers. | The conversation table now has 18 logical fields. Preserve both new fields and conversation, session-usage and message-usage cursors during physical conversion. |
| November 10 | [2927: diagnostic events](https://github.com/kenn-io/agentsview/pull/2927) adds `diagnostic_events` and its timestamp index. | These are retained user/script events. Daily UI aggregation does not make them disposable cache rows. Include them in exact preservation and byte accounting. |
| November 11 | [2931: OCR search](https://github.com/kenn-io/agentsview/pull/2931) adds `image_text_cache`, `image_text_occurrences` and their indexes. OCR text is shared by image hash and processor. | Treat existing OCR sharing as baseline. Preserve occurrence ownership, processor provenance, retry state, image assets and copy/replacement behavior. |
| November 12 | [2983: session totals](https://github.com/kenn-io/agentsview/pull/2983) adds `session_usage_totals`. | Preserve whole-session totals separately from per-message usage. Their original scope cannot be reconstructed by summing message rows. Broader session references must cover this table too. |
| November 12 | [3063: analytics reports](https://github.com/kenn-io/agentsview/pull/3063) retains report results in memory and adds persistent `analytics_change_revision` metadata and invalidation triggers. | Preserve this revision during representation-only work. Refresh exact trigger allowlists. The report bodies themselves are not new SQLite tables. |
| November 12 | [3064: Usage reports](https://github.com/kenn-io/agentsview/pull/3064) retains finished reports in a bounded in-memory cache. | Separate warm-process performance from fresh-process measurements. This cache does not itself add report payloads to `sessions.db`. |
| November 13 | [3082: frustration counts](https://github.com/kenn-io/agentsview/pull/3082) adds SQLite-only `session_frustration_markers`, keyed by session, transcript provenance and matcher version. | Preserve valid counts and provenance during physical maintenance. Rebuildability is not permission to discard retained rows. Include persistent cache writes and resync eligibility in tests. |
| November 13 | [2912: usage caching](https://github.com/kenn-io/agentsview/pull/2912) adds persistent PostgreSQL caching and advances the disposable SQLite usage sidecar to format 24, with new cost/ranking fields and a `WITHOUT ROWID` account-total rollup. | Distinguish primary archive, sidecars and PostgreSQL. Count temporary rebuild overlap and cache generations. Existing sidecar `WITHOUT ROWID` use is baseline, not a new saving from our layout proposal. |

The new conversation usage key is particularly relevant to metadata size. It
concatenates stored token JSON, model, provider, timestamp and source identity
fields with a separator. It is not a canonical hexadecimal digest. The digest
packing proposal must not silently encode or replace it under its existing
contract. Measure its footprint separately before proposing a representation
change. See the
[key and migration code](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/conversation_usage.go#L17).

The persistent table definitions are in
[`schema.sql`](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/schema.sql#L332).
Frustration counts have explicit
[copy eligibility checks](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/orphaned.go#L287).
The separate usage sidecar records its
[format version](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/usage_cache_schema.go#L71)
and [account-total layout](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/usage_cache_schema.go#L395).

## Which existing contracts changed?

- **Search corpora and ordering.** OCR rows use negative IDs and share the
  external-content FTS and CJK corpus through
  [`message_search_content`](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/db.go#L790).
  Preserve OCR main and TEMP triggers as well as message triggers.
  [2935](https://github.com/kenn-io/agentsview/pull/2935), merged November 13,
  adds exact/all-word/partial-word search tiers and matched-word ranking before
  BM25. Maintenance must preserve phrases, filters, snippets, hydration,
  counts, ranking and one consistent read snapshot.
- **Copies and ingestion.**
  [`CopyImageTextFrom`](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/image_text.go#L150)
  has its own ATTACH and allocates destination-local negative OCR IDs. It is an
  additional format-validation and copy boundary beyond those enumerated in
  the original metadata tests. Message insertion and replacement also refresh
  OCR occurrences; payload references cannot bypass those paths.
- **Export bytes.** [3046](https://github.com/kenn-io/agentsview/pull/3046),
  merged November 13, masks detected secrets by default. Conversation digests
  still identify stored text; masked lengths and offsets can differ. Exact-byte
  tests must explicitly request `RevealSecrets` or `--reveal` through permitted
  local access, and separately verify unchanged default masking. Do not use a
  masked export as the sole proof of stored-byte equality.
- **Identity enrichment.**
  [2926](https://github.com/kenn-io/agentsview/pull/2926), merged November 10,
  allows previously missing native source IDs to arrive on matching rows while
  preserving message rows, pins and export identities. Test this path with
  session references, packed IDs, tombstones and completed streaming replies.
- **Index and trigger definitions.**
  [2896](https://github.com/kenn-io/agentsview/pull/2896), merged November 9,
  adds the session-ID-tail expression index.
  [2924](https://github.com/kenn-io/agentsview/pull/2924), merged November 10,
  broadens the Claude snapshot index predicate.
  [2902](https://github.com/kenn-io/agentsview/pull/2902), merged November 9,
  removes redundant SQL casts, including from publication triggers. Refresh
  exact schema allowlists against actual current and supported legacy DDL;
  do not accept arbitrary extensions merely to get a migration through.
- **WAL diagnosis.** [3034](https://github.com/kenn-io/agentsview/pull/3034),
  merged November 12, fixes false held-reader warnings using checkpoint
  progress and WAL generations. It imposes no hard WAL cap and does not
  implement FTS segment maintenance.

## Why is the old header not enough?

Parser data version and installed schema are related but distinct. A newer
writer can install newer DDL while a source upgrade remains pending, leaving
an older `user_version`. An old header matching the proposal's supported parser
version therefore does not prove that its table, trigger and publication
contracts still apply. Maintenance or reversal must not overwrite that newer
state using an old schema template.

A narrow refusal adjustment is underway for the existing proposals. Existing
maintenance openers reject pending resync; the additional checks recognize
known newer-schema markers even under an older header. They are not a generic
proof that every unknown future schema will be detected. Validate refusal
before writable initialization, conversion or copy mutation for each supported
entry point, including old-header fixtures. Record the tested feature and
integration revisions when complete. Refusal protects an unsupported archive;
it does not port the proposals to version 179. Do not raise the accepted version
or erase pending state to bypass the guard.

OCR's
[version 173 scope](https://github.com/kenn-io/agentsview/blob/d0d5a77b5168e09250e93cbf3b79b99d709bec20/internal/db/data_version_scope.go#L62)
is an explicit compatibility fence with no provider reparse. Older writers
cannot maintain the new occurrences. An empty registered scope differs from an
unknown version requiring a full rebuild. Keep this distinction when updating
version checks and maintenance openers; physical maintenance does not authorize
source reparsing or a data-version rebuild.

## What must be proven before upstream submission?

1. Keep each feature independently based on its recorded `main`; prepare a
   deliberate port to an appropriate upstream base before submission. Inspect
   the actual comparison and preserve the dated original branch and evidence.
   Do not merge into `main`, stack features, or use an integration-only fix to
   conceal a standalone incompatibility.
2. Build populated current-schema fixtures through the actual schema setup,
   including 18-field conversation history, retained deletions, usage and
   analytics counters, diagnostic events, session totals, OCR and persistent
   derived counts. Include old-header/newer-DDL and pending-upgrade fixtures.
3. Prove exact logical values and SQLite types, all opaque IDs, pins, checkpoints,
   revisions and retained history across conversion, interruption, reversal,
   future writes and copies. Physical changes must not publish false usage or
   content changes. Extend every new ATTACH and ingestion boundary explicitly.
4. Revalidate search and export contracts on the new code: OCR and CJK corpus
   identity, word-search ranking, phrases, snippets and snapshots; masked and
   revealed export modes; native source IDs arriving after initial storage.
5. Refresh whole-file and per-object measurements, including mappings, indexes,
   usage keys, OCR, retained caches, WAL, assets, sidecars and migration overlap.
   Distinguish cold and warm process/cache states. Rerun affected standalone and
   combined costs; never add percentages from different fixtures or generations.

The original 24.4% combined metadata result and 5.92% standalone message-index
result remain scoped to their original synthetic version-167 fixtures and
recorded commits. They neither predict a version-179 archive nor establish
current-upstream compatibility. Their unresolved costs and platform/runtime
gates remain in the [integration evidence](database-compression-integration.md).

The six queued iterations inherit these gates: FTS maintenance gains OCR and
word ranking; multi-event summary references and shared payloads gain OCR
ownership and extraction; broader session references gain new session-linked
tables and counters; message-ID packing gains identity enrichment; metadata
layout experiments must distinguish primary tables from existing sidecar
optimizations. No new savings are claimed by this audit.

## Which earlier research statuses changed?

The November 10 search covered 53 open PR descriptions and 503 discussion/review
entries. This audit does not repeat that inventory. It updates the relevant
[additional concern rows](database-compression-next-steps.md#which-upstream-discussions-constrain-these-changes):
N07/2931 merged November 11, N09/2935 merged November 13, and N11/2948 merged
November 10. N08/[2573](https://github.com/kenn-io/agentsview/pull/2573) remains
open and comparison-only as checked November 13; its normalized fingerprints
are still not proof of exact physical IDs, JSON spelling or byte equality.
