# Message-order index consolidation

New archives enforce `(session_id, ordinal)` uniqueness through the named
`idx_messages_session_ordinal` index. Older archives have both an inline UNIQUE
constraint and a separately named index on the same columns. Normal startup and
ordinary compaction leave those existing archives unchanged.

Operators can request `db compact --consolidate-message-index` with direct archive
ownership. See the [command reference](../commands.md#agentsview-db-compact) for
space requirements and recovery. This change does not bump the parser version,
reparse sources, or remove retained content.

## Why retain the named index?

The usage-cache append extraction uses `INDEXED BY
idx_messages_session_ordinal`. Removing that name would break older read-only
builds. Keeping one explicitly named UNIQUE index supports that reader and
`ON CONFLICT(session_id, ordinal)` writers. Older writable startup uses
`CREATE ... IF NOT EXISTS` and does not recreate the inline constraint on an
existing table.

An actual cross-build check used unchanged main `4bf12972`: it created a populated
old-layout archive, this branch converted a scratch copy, and unchanged main
then opened it read-only, read messages and pins, executed the forced-index
extraction, and performed an upsert through its writable opener. This branch
subsequently read the resulting rows and pin. This checks that exact baseline;
it does not establish compatibility with every historical or future release.

## Candidate-only conversion

The existing write-owner lock and compaction write barrier protect the source.
A first `VACUUM INTO` copy is checked against the original schema and core table
counts before conversion begins. Only the disposable candidate changes:

1. Accept the known message columns, constraints, named index, and message
   triggers. Permit legacy column order, but refuse unknown definitions. CJK
   state must already match the available tokenizer; no search repair runs as
   part of this maintenance operation.
2. Pin one SQLite connection, disable foreign keys before its transaction, and
   check that they are disabled. Save schema objects and temporarily remove
   views and triggers inside that transaction.
3. Copy all message columns into a replacement table. Compare row counts and
   every value and SQLite storage type by the original integer message ID.
   SQLite performs the copy and comparison; Go does not load message bodies.
4. Drop the old candidate table, rename the replacement, restore indexes,
   views, and triggers, and check foreign keys. Run `ANALYZE messages` to
   refresh statistics for its surviving indexes; other tables are not analyzed. The expected schema fingerprint
   permits only the message table definition and named-index uniqueness to
   change.
5. Compact the candidate again, check it, retain the original backup through
   installation, and use the existing drained-reader swap and recovery path.

The table copy and targeted statistics refresh are one transaction with disk
work proportional to the archive.
It is not a bounded-batch migration. Candidate rollback journals and the final
VACUUM output live in the preflighted staging directory. Index sorting may spill
to SQLite's separately selected temporary directory. That filesystem has its own
preflight; shared-filesystem budgets are added. The command reads the configured
SQLite override and otherwise follows the pinned platform VFS selection order,
without changing process-wide temporary settings. The standalone maintenance
command does not change those environment variables after SQLite starts. Cancellation before
installation removes known candidate artifacts and leaves the source layout
intact. Unknown files in abandoned staging directories are not deleted.
Cancellation after installation finishes verification and the durable commit.

A transformed operation uses recovery manifest version 2, recording separate
original and candidate schema fingerprints. Recovery can restore an intact old
backup when the transformed candidate is unusable. Older builds refuse this
unfinished operation; recover with a compatible build before downgrading.
Completed archives need no new format bit. Version 1 manifests remain supported,
and an already canonical archive continues to use version 1.

## Reproducible synthetic measurement

Run the committed benchmark with the pinned SQLite dependency and optional
`dbstat` accounting:

```bash
CGO_ENABLED=1 GOFLAGS=-trimpath GOPROXY=off go test \
  -tags 'fts5 sqlite_dbstat' ./internal/db -run '^$' \
  -bench '^BenchmarkMessageIndexConsolidation$' -benchtime=1x -count=3
```

On 2026-11-11, darwin/arm64 on an Apple M3 Max, the fixture contained 1,000
sessions with 100 messages each. IDs are synthetic opaque strings; messages
contain approximately 100 bytes of text, including Unicode, and many optional
fields use their defaults. The full production schema, Porter FTS, and trigram
index were maintained. The optional CJK runtime was absent. Both layouts were
VACUUM-compacted; the consolidated layout came from the production conversion
of the same archive.

| Measurement | Legacy layout | Consolidated layout |
| --- | ---: | ---: |
| Main database bytes | 98,635,776 | 92,794,880 |
| Combined message-order index bytes | 11,681,792 | 5,840,896 |

All three runs removed 5,840,896 bytes, or 5.92% of this fixture's compacted main
file. The final implementation, including targeted analysis and temporary-space
preflight, took 3.237–3.267 seconds, with a 3.260-second median. These
figures do not predict savings on an archive with different ID lengths, content,
FTS corpus, or other retained data. No savings from another feature are included.

The initial fixed-order query sample was followed by a controlled
comparison after the statistics fix. The final space/conversion-time measurement
above includes that fix. It retained both layouts, analyzed the source before
conversion, and checked planner statistics and query plans. Ten alternating
ABBA/BAAB rounds produced 20 batch means per layout, each with 250 operations per
case. Reads were warmed and literal results checked; write transactions were
rolled back. Changed-content upserts changed the text on every revisit.

Reproduce the controlled normal-start experiment with:

```bash
AGENTSVIEW_INDEX_ABBA_REOPEN=both CGO_ENABLED=1 GOFLAGS=-trimpath GOPROXY=off \
  go test -tags 'fts5 sqlite_dbstat' ./internal/db -run '^$' \
  -bench '^BenchmarkMessageIndexConsolidationInterleaved$' -benchtime=1x
```

Omit `AGENTSVIEW_INDEX_ABBA_REOPEN` to measure the immediate post-compaction
object. Set `AGENTSVIEW_INDEX_BENCH_OUTPUT` to a new filename to retain raw
samples; an existing file is never overwritten. The checked-in
[measurement record](message-index-consolidation-measurements.json) retains all
controls and identifies superseded or confounded profiles.

For the supported offline-command workflow, both files were opened through the
normal application opener, using equivalent cached reader pools, and both WALs
were checkpointed to zero before sampling:

| Controlled sample, median microseconds/operation | Legacy | Consolidated |
| --- | ---: | ---: |
| Ten-message page | 10.165 | 10.166 |
| FTS first ten matches | 20.952 | 20.948 |
| Append | 112.570 | 104.593 |
| Changed-content upsert | 59.395 | 59.098 |
| Same-content upsert | 17.176 | 16.923 |

This sample found no warm-read regression above 5% on the normal-start path. It
is a small synthetic SQL workload, not full application parity or a throughput
guarantee. It does not measure request tails or cold disk I/O. Read samples include query dispatch, row iteration, cache lookup, and any
cache-miss preparation. Write statements are prepared outside the timed region.

A separate control kept the converted database open immediately after `Compact`
instead of reopening it through normal startup. Despite matching query plans,
planner statistics, cached reader pools, and zero-length WALs, its page sample
was about 9.7% slower and FTS about 6.4% slower. The cause remains unresolved;
the statistics fix alone did not remove that difference. The supported command
is offline and closes the database when it exits, but callers reusing the
in-process API must treat this as an outstanding performance concern.

Peak RSS, WAL, temporary space, and retained-backup footprint were not
instrumented. Admission budgets are conservative preflights, not disk
reservations or measured hard limits. No savings from another feature are
included, and CJK-runtime coverage is separate from these measurements.

## Remaining validation limits

The November 13 upstream audit compared the recorded `4bf12972` baseline
(parser version 167) with `d0d5a77b` (version 179). Upstream now has conversation
usage revisions, OCR search rows, diagnostic events, whole-session usage totals
and retained frustration counts. The existing version fence rejects completed
newer archives. Additional schema checks reject known newer tables or usage
columns even when an unfinished upgrade retains an older version header. These
checks run before writable initialization, archive imports and maintenance,
including already-canonical index plans. They read schema metadata only.

This refusal protects unsupported data; it does not establish version-179
support. A deliberate port must refresh the exact trigger templates, preserve
usage and analytics counters, and exercise the expanded OCR/search corpus and
copy paths before claiming compatibility. The measurements above remain tied
to the version-167 synthetic fixture.

The CJK preservation and future-write regression is present but has not run in
this environment because the optional tokenizer runtime is unavailable. It
remains a validation gate. The Windows temporary-directory helper was compiled
against the pinned Windows API bindings; the full Windows CGO archive runtime
was not exercised. These results support review of this feature branch, not an
unqualified production-readiness claim.
