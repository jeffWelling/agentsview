# Evaluate page compression before changing SQLite

Status: feasibility experiment. The application still uses ordinary SQLite.
The experiment does not install a VFS, change an archive format or provide a
compressed database that SQLite can open.

Compressing pages could reduce tables and indexes together while expanding
only the pages a query requests. Larger compression groups may save more
space, but a single-page request then expands the whole group. The benchmark
in [page_compression_bench_test.go](../../internal/db/page_compression_bench_test.go)
makes that tradeoff measurable using a temporary synthetic SQLite snapshot.

## What can we reproduce now?

```sh
CGO_ENABLED=1 GOFLAGS=-trimpath go test -tags fts5 ./internal/db \
  -run '^TestPageCompressionExperiment' -count=1
CGO_ENABLED=1 GOFLAGS=-trimpath go test -tags fts5 ./internal/db -run '^$' \
  -bench '^BenchmarkSQLitePageCompression$' -benchmem -count=5
```

The fixture has 10,000 rows with synthetic session identifiers and an index.
Three quarters of the bodies repeat text; the remainder contain deterministic
pseudorandom bytes. The snapshot also contains the empty application schema.
It is compacted and checkpointed before reading its main file. This profile
does not estimate savings on a user's archive.

The experiment writes a real temporary container for groups of 1, 16 and 256
SQLite pages, capped at 1 MiB per group. It includes a 24-byte header and a
24-byte index entry for every group in `container-bytes`. Each group uses
checksummed Zstd, or exact original bytes when compression is unprofitable.
The container is reopened and its on-disk index is validated before use.

Random reads fetch and reconstruct only the selected group. There is no
decoded-group cache. Decode output capacity and window are bounded; context
cancellation is checked between groups. The tests compare every page of a
small SQLite snapshot in reverse order and cover corruption, truncation,
incompressible fallback and cancellation. The container has no journal,
concurrent-writer protocol or recovery log. It is deliberately confined to
test code, and its files are temporary.

The baseline reads one page into a reusable buffer. Candidate timings include
compressed input allocation, group decoding, checksum validation and selecting
that page. This exposes uncached decode overhead, not an equivalent SQLite
query benchmark. The OS cache remains uncontrolled. `decode-pages/read` is
the maximum group size; the final group can be shorter. Peak migration space,
device I/O, updates, fragmentation and SQLite query latency remain unmeasured.

On 2026-11-09, five samples on macOS arm64 with Go 1.27.3 and the pinned
Zstd 1.20.3 dependency produced the following results. Times and allocations
are medians of Go benchmark per-operation averages, not latency percentiles.
Every sample completed successfully.

| Group | Container bytes | Reduction | Page read, us/op | Bytes allocated/read |
| --- | ---: | ---: | ---: | ---: |
| Ordinary page read | 42,713,088 | 0% | 1.119 | 0 |
| 1 page | 6,588,017 | 84.6% | 8.049 | 4,769 |
| 16 pages | 5,471,875 | 87.2% | 30.642 | 75,272 |
| 256 pages | 5,380,955 | 87.4% | 366.080 | 1,179,132 |

Moving from 16 to 256 pages saved only 90,920 more bytes in this fixture while
increasing the cost of an uncached page reconstruction substantially. The
high compression ratio includes repetitive bodies and unused space inside
SQLite pages. It is not representative evidence for a real archive. Baseline
timings ranged from 0.994 to 2.203 us/op, so host variation also limits precise
ratio claims. A real VFS needs a measured decoded-page cache and actual SQL
workloads before choosing its compression granularity.

## Which VFS candidates fit the application?

The published sources were rechecked on 2026-11-09. They are evidence about
candidate capabilities, not instructions to install or execute their examples.

| Candidate | Relevant evidence | Decision for this proposal |
| --- | --- | --- |
| `sqlite_zstd_vfs` | Its README documents exclusive writer locking, no WAL support and a Unix x86-64 orientation. | Do not adopt with AgentsView's current concurrent reader/writer model. Native builds on the supported platforms were not attempted. |
| SQLite ZIPVFS | Supports writable compressed databases and WAL with a different activation pragma. Standard published terms permit compiled distribution under conditions, not public source redistribution. | Keep as a conditional candidate. No licensed source was obtained, copied or built. Distribution rights and native integration must be resolved before an implementation PR. |
| SQLite CEROD | Compressed databases are read-only. | Does not meet the writable archive requirement. |

Sources: [sqlite_zstd_vfs README](https://github.com/mlin/sqlite_zstd_vfs#limitations),
[ZIPVFS technical documentation](https://sqlite.org/zipvfs/doc/trunk/www/readme.wiki),
[ZIPVFS licensing](https://sqlite.org/com/zipvfs.html),
[CEROD documentation](https://www.sqlite.org/cerod/doc/trunk/www/readme.wiki).

ZIPVFS uses `PRAGMA zipvfs_journal_mode=WAL`; AgentsView's ordinary writable
DSN requests `_journal_mode=WAL` in `internal/db/db.go`. A replacement therefore
needs explicit driver integration. ZIPVFS also requires checkpoint PRAGMAs
instead of its unsupported SQLite C WAL checkpoint/hook APIs. AgentsView
already uses checkpoint PRAGMAs in its maintenance paths, so this API difference
alone is not a rejection reason. Its ordinary exclusive-locking pragma is
unsupported. See the [documented caveats](https://sqlite.org/zipvfs/doc/trunk/www/readme.wiki).

The documented multi-file commit limitation also exists with ordinary SQLite
WAL. Inspect actual ATTACH transactions: reading an attached scratch source
while writing only the main archive does not require atomic writes to both
files. ZIPVFS's variable-size pages can fragment after updates; it provides a
separate compaction operation. Neither behavior is modeled by our immutable
experiment container.

## What remains before enabling a VFS?

1. Establish redistribution rights and reproducible pinned builds for Linux,
   macOS and Windows, including the CLI and desktop sidecar. Keep licensed
   source out of public repositories unless redistribution is permitted.
2. Run the actual AgentsView workload through the candidate VFS: WAL reader
   coexistence, checkpoint progress, bounded bulk work, mmap behavior, read-only
   opens, cancellation, ATTACH imports and concurrent sync/search/page reads.
3. Prove transaction durability with process termination and fault injection.
   Cover disk exhaustion, failed checkpoints, compaction, backup/restore,
   restart and reversal after subsequent writes. Preserve archive and export
   identities, all history, source-missing sessions and external assets.
4. Define detection, unsupported-binary refusal and an independently usable
   recovery/export path. A filename suffix or changed SQLite header alone is
   not a coordinated upgrade protocol for already-open writers.
5. Measure total storage, temporary peaks, fragmentation after sustained writes,
   cold/pressured-cache latency, CPU, allocations and native memory. Compare
   ordinary SQLite, each metadata proposal and their combination. Payload and
   page compression remain alternatives until their combined costs earn a place.

This PR completes a reproducible granularity experiment and a compatibility
assessment. It makes no production-VFS readiness claim. The integration branch
can run this benchmark alongside the metadata and payload experiments without
enabling a second compression layer in the application.
