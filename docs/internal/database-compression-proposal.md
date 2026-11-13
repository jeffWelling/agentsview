# Proposal: shrink the archive without losing queryable history

Status: historical design and investigation snapshot, 2026-11-08. This is not
an accepted upstream design or a release announcement. The current
[integration evidence](database-compression-integration.md) records the later
implementations and measurements; historical measurements below remain tied
to their original prototypes and fixtures.
The [source concern matrix](database-compression-concerns.md) records the issue
and pull request evidence, including historical fixes and unresolved limits.

The [delivery plan](database-compression-delivery.md) records the agreed
2026-11-09 workflow: each feature branches from the fork's `main` and opens a
PR back to that `main`, with independent development and a separate integration
branch for combined validation. Features do not branch from one another.
Agreement to investigate each approach does not establish that each should ship
or that both compression
layers should be enabled together.

## What should we propose first?

Start with a measured, lossless reduction of conversation-export metadata and
its indexes. Preserve every external identity, revision and retained deletion
record. Evaluate repeated session identifiers and canonical digests before
adding a codec to frequently searched tool outputs. Keep SQLite as the durable
archive and keep normal application reads available without expanding the
whole database.

Do not submit the existing trigger and Go-callback compression prototype as a
general feature. It proved that individual tool outputs can be compressed and
read back exactly, but its measured CPU, allocation, concurrency and upgrade
costs are too large to treat the space saving alone as success.

The proposed sequence is:

1. Establish a current upstream baseline, including existing deduplication and
   search indexes. Measure occupied tables, indexes and total archive storage.
2. Compare compact conversation-metadata layouts on isolated snapshots. Advance
   only a variant that preserves the export contract and produces a worthwhile
   net saving without unacceptable query or migration cost.
3. Evaluate a separate, optional per-value codec at an explicit storage boundary
   after the metadata measurements. It must pass the resource, search and
   release gates below before enabling compression in a supported archive.
4. Keep page/VFS compression as a separate feasibility experiment. It could
   compress indexes too, but its build, licensing and lifecycle requirements
   have not been demonstrated for AgentsView.

This is a staged proposal, not a claim that metadata normalization will achieve
a particular ratio. Its savings are unmeasured. If the first experiment fails
its gates, retain the existing layout and use that evidence to choose the next
experiment.

## What existed in the original prototype?

The experimental branch at `632342fa`, based on v0.46.1 commit `65523169`, stores
eligible `tool_result_events.content` values as individually compressed Zstd
BLOBs. A versioned envelope identifies the representation and records the
decoded length. Small values, values above 64 MiB and values saving less than
roughly 10% remain ordinary SQLite TEXT. Eligible values start at 1 KiB.

The implementation uses checksummed frames, a 1 MiB window limit, a 64 MiB
absolute decode limit, transactional compression triggers and resumable
conversion/reversal. The reviewed revision adds a separate decoded-size table
and a native SQL path for ordinary TEXT. Its SQL readers use an application
function to decode BLOBs. Metadata and indexes remain queryable, but a plain
SQLite client sees compressed bytes in those cells. Older stock AgentsView
binaries refuse a compressed archive until reversal finishes.

The codec is lossless for supported values: it changes their stored
representation, not their decoded bytes. It does not remove images, tool
results, history or source files. Each selected value is expanded independently;
opening a session does not expand the entire database. A broad content search
can nevertheless need to decode every candidate value. Queryable does not mean
every query becomes cheaper.

### What did the experiments establish?

The earlier full-size isolated candidate shrank from 28,455,108,608 to
23,362,633,728 bytes after compression and compaction: **17.9%**, or about
5.09 GB. Decoded hashes and metadata matched for all 1,135,310 tool-result
events. Row counts, integrity and foreign-key checks passed, as did ten
application comparisons with documented volatile metadata exclusions. This was
the original experimental format, before the later size-table fixes; the
full-size candidate was not upgraded during that fix task.

The original allocation audit attributed approximately 13 GB to conversation
export history and indexes, versus 8.125 GB to tool-result events. The history
included about 30.6 million removed/deleted records. Those records are retained
identity and change history, not disposable free space. This distribution
motivates the metadata experiment; it does not establish its achievable saving.

A separate XZ backup shrank from 28,475,273,216 to 4,064,094,932 bytes, an 85.7%
reduction, with a verified exact streamed round trip. That backup was taken at a
different instant and must be restored before normal SQLite use. Its result
cannot be presented as queryable database compression.

An independent investigation sampled 8,192 SQLite pages and observed about
65.2% compression; larger sampled chunks reached about 71.2%. These are sample
codec results. They do not include a working compressed VFS, write behavior,
random-read latency or a verified full database result.

### Why hold the prototype?

Warm-cache synthetic measurements used 1,000 JSON-like outputs of approximately
64 KiB, three samples of five iterations and two Go CPU threads. Payload bytes
fell from 65.55 MB to 7.01 MB. Despite that favorable ratio, several operations
became slower:

| Operation | Plain | Compressed |
| --- | ---: | ---: |
| Search without a match | 68.1 ms | 101.9 ms |
| Search with matches | 142.1 ms | 178.8 ms |
| Load a 32-message page | 1.17 ms | 2.25 ms |
| Fingerprint 1,000 outputs | 78.6 ms | 108.8 ms |
| Insert 64 outputs | 20.3 ms | 18.6 ms |
| Update 64 outputs | 5.32 ms | 18.2 ms |
| Reimport 64 unchanged outputs | 4.88 ms | 6.69 ms |
| Reimport 64 changed outputs | 30.2 ms | 27.3 ms |

No-match search allocated approximately 148.7 MiB cumulatively in Go, compared
with about 15 KiB for plain storage. This is allocation volume, not peak or
retained memory. One search decoded 1,000 candidates and another 100 matching
values although only 20 results were returned. A shared decoder also caused
contention: with three bulk scans, page p95 rose from 2.48 to 9.32 ms. A larger
decoder pool did not reliably improve the tail.

In a deliberately incompressible random-byte TEXT fixture, enabling the codec
saved no payload bytes but increased insert time by about 30% and update time
by about 70%. This is a worst case, not representative transcript text. Even
the guarded ordinary-TEXT SQL path retained a microbenchmark cost, 1.91 versus
2.39 ms over 20,000 small rows; that is not a whole-application slowdown claim.

The adversarial review also found that decoding checks the advertised length
after expansion. A damaged envelope claiming one byte around a valid 8 MiB
frame passed metadata admission, then allocated about 8.42 MB through a public
reader with a 4 MiB budget before returning a length error. The absolute 64 MiB
limit held, and no silent incorrect output was observed. Normal encoder output
has the correct length, but damaged or imported bytes must not bypass budgets.

Prior regression checks fixed two other problems: ordinary TEXT crossing the
Go callback boundary and a header preflight that materialized an entire BLOB.
Those fixes are useful but do not resolve the remaining costs. No
normal-operation data loss or missed logical reader was found. Full current
upstream integration, cold-cache production-scale measurements and the full
external-mirror matrix remain unvalidated.

## What must remain true?

These requirements define this proposal. They are not claims that every issue
contains an upstream mandate. The matrix distinguishes user outcomes,
maintainer preferences, historical review findings and engineering inferences.

| ID | Required behavior |
| --- | --- |
| R1 | Preserve exact logical bytes, NULL/empty distinctions, ordering, opaque IDs, revisions, tombstones, source-missing history, curation and deletion scope. No retention policy change. |
| R2 | Preserve complete application search, pagination, snippets, exports and metadata queries. No whole-database expansion before use. Do not load bodies just to obtain lengths or list metadata. |
| R3 | Bound compressed and decoded bytes, decoder concurrency, transaction duration and migration work. Preserve cancellation, quick-reader capacity and checkpoint progress. Measure both Go and native memory. |
| R4 | Use archive-owned migration and recovery, writer exclusion and checked installation. Preserve the authoritative archive on failure; support interruption, restart and reversal with explicit space requirements. |
| R5 | Cover ordinary writes, late results, bulk staging, ATTACH copies, orphan/trash preservation, archive-only copies, imports and maintenance. Keep logical fingerprints and mirror/export semantics stable. |
| R6 | Define format capabilities, supported readers/writers, desktop/CLI/update behavior and recovery tooling before enabling a new representation. Stop incompatible already-open processes as well as refusing new opens. |
| R7 | Report net steady-state storage, peak migration/reversal space, I/O, CPU and latency against the same current baseline. Count indexes and assets; separate backups, raw sources and mirrors. |

The owning project contracts remain in
[Storage Rules](../agents/storage.md), especially conversation export,
archive-content projection, staged imports and archive-preserving rebuilds.
Compression must preserve the archive as it exists. It cannot recover bytes
that an earlier retention policy or parser already discarded.

## How would metadata normalization work?

The current `conversation_messages` table repeats a text session ID in every
row and in multiple indexes. Its key is `(session_id, message_id)`, with
revision, source and live-ordinal lookup indexes. Export bodies already live in
archived messages; the history table retains identity and change metadata.
See [schema.sql](../../internal/db/schema.sql) and
[conversation_export.go](../../internal/db/conversation_export.go).

Evaluate a v2 physical layout with these constraints:

- Store a repeated session identity once in a permanent mapping, and refer to
  it internally with an integer. Mapping lifetime follows retained export
  history, not the live `sessions` row. Deleting or trashing a session must not
  cascade away its history or make an external ID unrecoverable.
- Preserve message IDs as exact opaque values. Do not derive them from text,
  timestamps or ordinals. Do not automatically add a dictionary for every
  unique message ID: an extra mapping table and unique index may cost more
  than they save. Compare inline storage, exact tagged packing where valid,
  and mappings only where repetition justifies them.
- Compare storing canonical lowercase hexadecimal SHA-256 digests as 32 raw
  bytes instead of 64 ASCII bytes. Retain a tagged exact fallback for empty,
  noncanonical and legacy values. Decoding must reconstruct the original
  representation; case folding and Unicode normalization are prohibited.
- Preserve source identity, removed/deleted state, gaps, text lengths and
  revisions. Keep the live ordinal lookup selective even when a session has
  extensive removed history. Preserve cold versus active export-index state;
  do not activate a cold archive merely to migrate it.
- Compare actual index plans and total bytes for each layout. Do not assume
  `WITHOUT ROWID` is smaller: wide primary keys can enlarge secondary indexes.
  Include mapping indexes and the added joins in the comparison.

This layout leaves ordinary SQLite access possible without a compression
extension. External SQL written against the old table shape may still break.
A compatibility view, if proposed, needs explicit read/write and query-plan
validation; it is not a free substitute for the old table. Existing
application/export contracts must remain unchanged even if the private schema
changes.

Use an archive-only schema transformation, not a reparse of source files. The
implementation must choose an additive/resumable migration consistent with the
existing maintenance protocol, retain a verified rollback representation until
installation succeeds, and clean up old physical storage only through supported
maintenance. A table/index layout change must not silently renumber export
revisions or invalidate cursors. If a database-generation change is necessary,
use the existing explicit rebuild contract and surface its resumption behavior.
That choice remains a design gate, not an unspecified implementation detail.

The first experiment should compare session-reference normalization alone,
digest packing alone and their combination. This separates benefits and makes
one narrow implementation reviewable. It must retain every tombstone; tombstone
garbage collection needs a separate consumer-watermark and recovery contract.

## What would a later payload codec need to change?

A later codec should use an explicit representation with independently bounded
values and metadata available without loading bodies. Compare decoding after
bounded SQL selection with a native codec implementation where SQL predicates
require decoded text. Removing a Go callback is not itself proof of lower total
allocation or CPU cost; measure the native allocator too.

The design must address all of the following before becoming a feature:

- Keep compression-off SQL on its native path. Avoid per-row codec dispatch and
  compression-only startup work for ordinary archives. Preserve genuinely
  read-only access where the existing format supports it.
- Validate stored type, envelope version, decoded length, frame length/window
  and checksum. Enforce the caller's remaining byte budget during expansion,
  not after allocating the output. Reject inconsistent metadata and malformed,
  concatenated or truncated frames according to a documented owned format.
  Already-encoded imports need full validation too. External provider formats
  have their own compatibility rules; valid checksum-free source frames must
  not be rejected merely because the owned archive format requires checksums.
- Leave oversized or unprofitable values intact, or use a separately validated
  streaming path. Never truncate a value to fit the compressor. Bound both
  compressed input admission and expanded output, including concurrent readers.
- Prefer filtering by indexed metadata and selecting a result page before
  hydrating bodies where semantics permit. Reuse decoded text within a bounded
  operation rather than decoding every match again for snippets. Avoid retaining
  an unbounded result set merely to remove repeated decoding.
- Audit every write surface, including set-based `INSERT ... SELECT` and
  `UPDATE`, staging/ATTACH, orphan/trash copying, preservation rebuilds, late
  results, image maintenance and archive-only import. Each path must encode and
  update size metadata atomically through a shared boundary, or fail explicitly.
  Replacing triggers only in the normal Go insertion path is incomplete.
- Compute equality, hashes, lengths, secret offsets, signals and export changes
  from the established logical representation. Encoding-only changes must not
  mark sessions unread, create new logical changes or trigger repeated remote
  transcript uploads. Keep partial copies and failures transactional.

Search needs particular care. Upstream's dialogue/thinking trigram indexes do
not index tool results. An optional tool-result index may accelerate some
queries, but its storage and write costs count against compression savings.
Short patterns, Unicode/case behavior and arbitrary regular expressions still
need exact fallback behavior. A regex without an indexable literal may require
examining every in-scope payload. Preserve completeness; bound resources and
support explicit cancellation/timeouts instead of silently returning partial
results. A changed deep-search interface would be a separate product decision.

Run scans through the existing bulk-read admission rules and keep quick reads
available. A single global decoder serializes work; unlimited decoders amplify
memory and CPU contention. Choose concurrency from combined sync/search/page
benchmarks, not an isolated throughput test. Read snapshots must remain
consistent without indefinitely pinning WAL pages.

## What alternatives did we consider?

| Approach | Decision and reason |
| --- | --- |
| Existing deduplication and compaction | Baseline first. Exact duplicate elimination is valuable and often avoids recurring decode cost. Compaction only reclaims physical free space; it cannot shrink live bytes by itself. |
| Conversation-metadata normalization | First experiment. It targets a large measured allocation while leaving payload bytes and ordinary SQLite access intact. Savings, joins and migration complexity still need measurement. |
| Current trigger/Go-UDF Zstd BLOB prototype | Keep as research evidence. It demonstrates exact per-value round trips, but has measured scan/update costs, repeated decoding, budget enforcement and stock-reader compatibility gaps. |
| Redesigned typed per-value compression | Conditional second experiment. Incremental reads and writes are feasible, but search, all write surfaces, bounded decoding and release compatibility must be solved together. |
| Transparent `sqlite-zstd` table/view mode | Do not adopt as a drop-in layer. Its documented limitations include ATTACH support, DDL restrictions and changed `sqlite3_changes()` behavior. Using only native codec functions would be a different design with its own tests. |
| Open-source `sqlite_zstd_vfs` | Current implementation is unsuitable as the default: its documented no-WAL and exclusive-writer constraints conflict with the application model. This does not prove all compressed VFS designs are unsuitable. |
| SQLite ZIPVFS | Worth a separate feasibility/licensing decision. It supports incremental compressed access and WAL through a special PRAGMA, but requires native build integration and supported lifecycle behavior across platforms. No AgentsView integration was validated. |
| Filesystem compression | Useful where an operator's filesystem supports it. It is not a portable application feature, and platform support for transparent reads does not prove newly written SQLite pages stay compressed. |
| Immutable compressed segments | Potential future architecture. It adds mutable overlays, manifests, ownership/GC, search, export and crash-recovery complexity; compressed artifact copies can also add another corpus copy. |
| XZ or Zstd backup files | Keep for backup/storage use. They are not directly usable as the live SQLite archive. |
| Retention, stripping, remote-only history or local eviction | Separate policies and products. They may reduce local space but do not satisfy preservation of all locally queryable data. |

Primary mechanism references:
[SQLite VACUUM](https://www.sqlite.org/lang_vacuum.html),
[sqlite-zstd](https://github.com/phiresky/sqlite-zstd),
[sqlite_zstd_vfs](https://github.com/mlin/sqlite_zstd_vfs),
[ZIPVFS technical notes](https://sqlite.org/zipvfs/doc/trunk/www/readme.wiki),
[ZIPVFS licensing](https://sqlite.org/com/zipvfs.html),
[Btrfs compression](https://btrfs.readthedocs.io/en/latest/Compression.html),
[Windows filesystem compression](https://learn.microsoft.com/en-us/windows/win32/fileio/file-compression-and-decompression)
and [Apple volume compression capability](https://developer.apple.com/documentation/foundation/urlresourcevalues/volumesupportscompression).

ZIPVFS deserves an accurate comparison. Its WAL configuration differs, and some
SQLite C WAL checkpoint/hook APIs are unavailable, although checkpoint PRAGMAs
are supported. AgentsView already uses PRAGMAs for much of checkpoint control.
Multi-database atomicity under WAL is not a demonstrated new blocker: ordinary
SQLite WAL has that limitation too, and the examined staged import reads an
attached scratch database while writing the main archive. Verify the actual
transactions rather than rejecting every ATTACH use. Licensing, redistribution,
backup, compaction, mmap/read-only behavior, crash recovery and platform
packaging remain unresolved integration questions.

## How would we decide whether to ship it?

Use a fresh upstream baseline, including merged
[#2837](https://github.com/kenn-io/agentsview/pull/2837) dialogue/content
deduplication, [#1970](https://github.com/kenn-io/agentsview/pull/1970) export-body
deduplication, [#1975](https://github.com/kenn-io/agentsview/pull/1975) lazy export
indexing and [#2765](https://github.com/kenn-io/agentsview/pull/2765) trigram
search. Merged on upstream main does not establish inclusion in an installed
release. Do not count these improvements as new proposal savings.

Acceptance evidence must include:

1. Exact logical hashes and counts before/after/reversal, SQLite integrity and
   foreign-key checks, all external IDs/tombstones and export cursor/revision
   behavior. Include empty/NULL, NUL, malformed UTF-8, legacy identities,
   source-missing sessions, archive-only archives, curation and deletion scopes.
2. A write/read matrix covering every R5 path and PostgreSQL, DuckDB and
   ClickHouse boundaries. Verify unchanged publication fingerprints, image
   assets, secret locations, signal results, raw/formatted export and all search
   modes. Include future writes after migration, not just migrated rows.
3. Process-kill, cancellation, full-disk, invalid-temp, corrupted-metadata and
   competing-owner cases. Prove bounded restart progress and preserve the
   authoritative archive after failure. Follow existing prepared/committed
   recovery phases and the checked checkpoint before main-file replacement.
4. Total database-file and asset storage after supported compaction, plus
   WAL/SHM and temporary peaks. Break down indexes and mappings within the
   database file without counting them twice. Report retained backups, raw
   sources, caches and mirrors separately. Preflight enough space for conversion
   and reversal, including installation copies; never advertise only the final
   small file.
5. Warm and cold/pressured-cache workloads, small and large archive cardinality,
   append/update/no-op sync, first and subsequent exports, substring/regex
   search, pagination and concurrent sync plus interactive reads. Measure p50,
   p95, throughput, physical I/O, CPU, cumulative allocation, peak/live memory,
   WAL progress and cancellation latency on supported operating systems.

Provisional experiment gates are a net reduction of at least 10% on a
representative large archive, and no more than 5% p95 regression on ordinary
interactive/default-off paths within measured variance. These numbers are
proposed decision aids, not upstream requirements or measured outcomes. Agree
final budgets with maintainers; disclose any
accepted enabled-mode tradeoff rather than hiding it in an aggregate average.

For a new codec, ship supported readers and recovery/export tooling before
enabling the representation. Test CLI and desktop sidecar upgrades together.
Older readers must fail closed with actionable reversal instructions; format
gating cannot protect a process that opened the database earlier. Preserve
backups until the new archive and reversal have been verified. A fresh upstream
implementation should be reviewed independently of the experimental branch.

## What can maintainers reasonably accept from this investigation?

The evidence supports a narrower next experiment, not a merge-ready compression
feature. There is a real space opportunity, exact per-value compression is
possible, and direct use does not require expanding a whole archive. The same
measurements also show why compression ratio is insufficient: search and
updates can consume substantially more CPU and allocation, and format changes
affect ordinary updates and external tools.

The first proposed change therefore targets repeated metadata while preserving
all history. It builds on existing upstream work, avoids claiming retention as
lossless compression, and has explicit stop conditions. A later codec or VFS
should earn its added complexity with measured net savings and complete
behavioral preservation. The concern matrix records how every identified
source concern is preserved, tested, already addressed upstream or explicitly
outside this proposal's scope.
