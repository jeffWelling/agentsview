# Review the storage proposals together

Status: integration candidate, 2026-11-10. No proposal is merged into `main`.
Every feature starts directly from fork `main` at
`4bf12972e19486a59788692b0c832fff0002668e`; the combined branch is
`codex/storage-integration-v2`. It is a review and scratch-data validation
candidate, not an installation or migration instruction.

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
| R3: resource bounds | Bounded migration batches, cancellable writer admission, bounded chunk decoding and worker count. | Final verification and index construction can still be large transactions; production latency, native memory and crash stress need separate evidence. |
| R4: maintenance and recovery | Explicit writer-owned maintenance, checked representation conversion, resumable digest packing, restartable reference staging, current-state reversal. | Adequate temporary disk space and a verified backup are operator requirements; no claim of a complete cross-platform fault-injection campaign. |
| R5: ingestion and copies | Metadata proposals cover ordinary writes, no-ops, late changes, archive-preserving copies and compaction. | External database services are not deployed or exercised by these local checks; payload ingestion integration is still gated. |
| R6: format compatibility | Separate capability bits, metadata agreement checks, incompatible-open refusal, explicit reversal, detached payload recovery. | Already-open clients must be stopped; no automatic upgrade or desktop rollout is introduced. Page/VFS packaging and licensing remain unresolved. |
| R7: honest storage/performance | Actual index/container bytes and standalone costs are recorded with fixture and cache limits. | Synthetic ratios are not predictions for an individual archive; peak migration disk, cold-cache tails and broader platforms need further measurements. |

The objective is a defensible proposal, not a claim that every acceptance gate
has already passed. Unsupported paths stay disabled or outside the supported
scope. The aggregate must demonstrate both metadata application and reversal
orders before publication; its final evidence is recorded below.

## Combined evidence

Combined validation and exact feature revisions will be recorded after the
independent metadata branches pass review and are composed here. All fixtures
are synthetic and stored in temporary directories. No installed application,
live archive, original backup or source transcript is used or changed.
