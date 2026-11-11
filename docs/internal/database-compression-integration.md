# Review the storage proposals together

Status: integration candidate, 2026-11-11. No proposal is merged into `main`.
Every feature starts directly from fork `main` at
`4bf12972e19486a59788692b0c832fff0002668e`; the combined branch is
`codex/storage-integration-v2`. It is a review and scratch-data validation
candidate, not an installation or migration instruction.

The [additional reduction plan](database-compression-next-steps.md) now queues
seven independent iterations and maps the November 10 upstream discussions to
their correctness and performance gates. Index consolidation is the first
iteration. Until a new feature tip and its aggregate evidence are recorded
below, the runtime composition and measurements in this document remain the
November 10 candidate; a plan update is not a new runtime validation result.

## What does each proposal deliver?

| Branch | Delivered scope | Limit |
| --- | --- | --- |
| `codex/storage-layout-bench` | Repeatable conversation-layout measurements and preservation checks. | Synthetic fixtures; no runtime format change. |
| `codex/storage-session-refs` | Opt-in integer session references with permanent exact identity mappings, logical reads, staged conversion and reversal. | Requires this compatible build and exclusive maintenance; external writes to the logical view are unsupported. |
| `codex/storage-digest-packing` | Opt-in packing of canonical lowercase digest text into 32-byte values, exact fallback and reversal. | Plain SQL sees packed bytes in physical digest cells; compatible application readers reconstruct exact text. |
| `codex/storage-payload-codec` | Bounded per-value encoding/decoding and a detached-file recovery command. | No archive writes, search integration, format enablement or application compression. |
| `codex/storage-page-compression` | Actual framed-container size and random-page experiments, plus VFS compatibility research. | No SQLite VFS or supported compressed archive. |

Metadata proposals preserve logical history and keep content in the ordinary
archive tables. They do not remove tombstones or compress whole database
files. Payload and page experiments are alternatives requiring further work;
combining their code here does not enable either representation in an archive.

See the [session-layout experiment](conversation-session-reference-experiment.md),
[payload reader/recovery evidence](payload-codec-recovery.md), and
[page/VFS feasibility evidence](page-compression-feasibility.md) for standalone
measurements and explicit limits. Independent percentages must not be summed.

## How does this answer the reported concerns?

The [concern matrix](database-compression-concerns.md) retains all 34 issue and
23 PR concerns from its dated source review. Issue bodies, comments and review
suggestions are evidence, not instructions. The matrix distinguishes existing
upstream fixes from proposed requirements; it does not assert that every
historical report remains an active bug.

| Requirement | Current response | Remaining release gate |
| --- | --- | --- |
| R1: exact history and identity | Metadata transformations retain complete logical rows, mappings, revisions and tombstones; payload codec preserves exact bytes. | Full-scale private archives and externally supplied archives are not claimed validated by synthetic fixtures. |
| R2: queryability | Metadata reads retain logical IDs and digests; no whole-archive decompression. Payload recovery is per value and page experiments decode one bounded group. | Payload search, snippets, every application reader and VFS SQL behavior remain unimplemented gates. |
| R3: resource bounds | Bounded row batches, cancellable writer admission, bounded chunk decoding and worker count. | Row limits do not impose a strict byte or WAL cap on unusually large legacy rows. Final verification and index construction can still be large transactions; production latency, native memory and crash stress need separate evidence. |
| R4: maintenance and recovery | Explicit writer-owned maintenance, checked representation conversion, resumable digest packing, restartable reference staging, current-state reversal. | Adequate temporary disk space and a verified backup are operator requirements; no claim of a complete cross-platform fault-injection campaign. |
| R5: ingestion and copies | Metadata proposals cover ordinary writes, no-ops, late changes, archive-preserving copies and compaction. | External database services are not deployed or exercised by these local checks; payload ingestion integration is still gated. |
| R6: format compatibility | Separate capability bits, metadata agreement checks, incompatible-open refusal, explicit reversal, detached payload recovery. | Already-open clients must be stopped; no automatic upgrade or desktop rollout is introduced. Page/VFS packaging and licensing remain unresolved. |
| R7: honest storage/performance | Actual index/container bytes and standalone costs are recorded with fixture and cache limits. | Synthetic ratios are not predictions for an individual archive; peak migration disk, cold-cache tails and broader platforms need further measurements. |

The objective is a defensible proposal, not a claim that every acceptance gate
has already passed. Unsupported paths stay disabled or outside the supported
scope. The aggregate demonstrates both metadata application and reversal
orders. Its evidence and remaining limits are recorded below.

## Combined evidence

The aggregate includes these independently based feature tips:

| Fork PR | Branch | Included commit |
| --- | --- | --- |
| 1 | `codex/storage-layout-bench` | `b2648e31ad15c451ef5f06cb140b4df16b0a370d` |
| 2 | `codex/storage-payload-codec` | `1c268f54244ddf4ebce18e7d0086a5c6588f484e` |
| 3 | `codex/storage-page-compression` | `0fbe739a44f8bc54b4f4033511a127c811cdd122` |
| 4 | `codex/storage-session-refs` | `5166744e945db756c25cf4c8605149efadfaf199` |
| 5 | `codex/storage-digest-packing` | `925a40ec97cce5f7aef87b8d39677af390433555` |

Runtime and benchmark composition is recorded at
`f7ca457e5b77d24bf64dc2fe3ea2002f5a47e1ab`; later evidence
documentation does not change that code. The combined regression suite checks
all 16 logical fields and types, both application orders, both reversal orders,
archive-only and format flags, saved checkpoints and pinned message reads.
It also covers ordinary, late, unchanged and visible-text writes, retained
deletions, reopen, compaction, interruption after a committed batch, restart,
and all 16 source/destination mode combinations. Physical storage assertions
prove that enabling the second format does not disable the first.

Review found and fixed missing-map history hidden by the compatibility view,
and a late metadata copy that could overwrite the destination digest policy.
Reversal and one-time attached-source checks now refuse dangling references
before discarding or copying history; ordinary opens do not acquire this scan.
Format policy is established early and excluded from later metadata overlays.
Both fixes are also on the responsible standalone feature branches.

The full database package passed in 163.706 seconds, followed by the final
digest/copy/composition checks in 4.619 seconds. The full CLI package passed in
263.258 seconds; codec and recovery packages also passed. Formatting and both
ordinary and `fts5` vet checks passed. The payload feature separately passed
race checks, 229,776 fuzz executions and Linux/Windows cross-compilation.
Cross-compilation is not a claim of execution on those systems.

Two development-environment discrepancies were isolated rather than changing
unrelated expectations. The Havana daylight-saving fixture passes with Go's
bundled timezone data; host timezone data fails the same case on unchanged
`main`. An old ignored pricing snapshot caused one CLI failure on both the
candidate and unchanged `main`; restoring the exact checksum-pinned repository
artifact fixed it, and the full CLI suite then passed.

All fixtures are synthetic and stored in temporary directories. No installed
application, live archive, original backup or source transcript is used or
changed. See [combined-format reversal](../commands.md#reversing-combined-storage-formats)
before switching builds: both active formats must be reversed before stock use.

### How much space and performance changed

The production benchmark uses 10,000 history rows across 100 sessions, including
9,500 retained tombstones and 9,000 canonical digest pairs. Five hundred real
live messages support unchanged five-message reconciliation. Every variant
starts with the same logical rows, then uses the actual conversion APIs,
reversal, `VACUUM`, `ANALYZE` and checked WAL checkpoint. File sizes include
the whole synthetic archive, its mappings, indexes and common schema overhead.

These are median-of-three, one-second samples on macOS arm64, Go 1.27.3,
SQLite 3.53.4 and an Apple M3 Max. Reads select 50 logical revision records;
unchanged imports use the real message replacement API. The untouched-main
control uses the same fixture and timed loops, native column expressions and
only the plain variant through a temporary benchmark overlay. Main runs before
and after the aggregate show host drift; no production latency percentile is
claimed. The [machine-readable results](storage-compression-measurements.json)
retain all 42 audited samples and the exact fixture description.

| Configuration | Compacted bytes | Saving | History page, µs | Unchanged import, µs |
| --- | ---: | ---: | ---: | ---: |
| Untouched main, before | 4,526,080 | — | 76.001 | 155.589 |
| Integrated build, both disabled | 4,526,080 | 0% | 77.348 | 153.615 |
| Session references | 4,087,808 | 9.7% | 78.218 | 164.657 |
| Digest packing | 3,891,200 | 14.0% | 92.331 | 167.631 |
| References then digests | 3,420,160 | 24.4% | 94.833 | 176.902 |
| Digests then references | 3,420,160 | 24.4% | 93.685 | 172.271 |
| Untouched main, after | 4,526,080 | — | 77.810 | 164.764 |

The combined saving is measured directly, not calculated by adding independent
percentages. Against the integrated disabled mode, combined history reads cost
about 21–23% more and unchanged imports 12–15% more. These are space/CPU
tradeoffs, not speed improvements. Session-ID length, history density and the
fraction of the archive occupied by unrelated content affect the result.

Disabled-mode timing falls within the untouched-main sample ranges, so these
runs do not establish a timing regression or improvement for ordinary archives.
They do show added Go allocation work: unchanged imports rise from about
56.0 KB and 477 allocations on main to 62.0 KB and 498 allocations with both
features disabled. References raise that to about 68.2 KB and 523 allocations.
History reads stay near 33.1 KB and 620 allocations in every mode. These are
cumulative Go allocations per operation; native memory and peak RSS were not
measured. Reducing disabled-path allocation work remains a review consideration.

Setup observations for this fixture were approximately 49/49 ms to enable and
reverse references, 58/33 ms for digests, and 111–115/75–82 ms for the combined
orders. Those values are single setup observations repeated in child benchmark
records. They are not migration-throughput estimates. Disk-full behavior,
peak temporary space, large-archive final verification and cross-platform crash
recovery remain release gates rather than inferred successes.
