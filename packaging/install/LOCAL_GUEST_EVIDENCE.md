# Verified templates for the official runtime installer

The official-runtime release builder checks the managed template archive by SHA-256. The recorded checks ran on Linux amd64 with smolvm 1.37.1, virtio-net and internal storage on 2026-11-25. Capability declarations bind that runtime and architecture to these exact archives.

| Archive | SHA-256 | Recorded checks |
| --- | --- | --- |
| Default r3 | `ad5b45167c999b4af5a845de918aff0f6c14d30544c4cadfd32c8aead95f1c64` | Persistent Task, clean checkpoint, independent clone, native exec, files, Live Fork, automatic idle and active recovery. The package publishes the catalog ID `debian-managed-ad5b45167c99-v2`. |
| r4 | `e77444dc02446153136008fe4383132579381fdcd403ca73b476e4e646ece281` | Persistent Task and bounded log retention. Four concurrent readers received the same bytes across a segment boundary after the retention limit dropped older output. |

The r3 checks included native Fork with a running root task, independent parent and child task control, exact artifact verification on restore, and activity and lease protection during idle. The signed active restore used a new execution binding. The release registry `managed-template-gates.json` supplies the verified capabilities published by `GET /v1/templates`.

An explicitly supplied archive with an unregistered digest starts with an empty verified capability declaration. Managed Worker Runtime Bundles carry their own per-profile verification.

The original gate transcript is available in the platform source history with `git show 69a42f4:docs/phase1/LOCAL_GUEST_EVIDENCE.md`.
