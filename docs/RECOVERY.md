# Backup and recovery

A successful `mesh index` is not proof of a complete restore. Published notes are
Markdown, but the vault also has database-only work and security state. Never
delete `.mesh/` as an index reset or restore an old database over a running service.

## What must be preserved

| State | Recovery requirement |
| --- | --- |
| Markdown, attachments, ignore/configuration files and repository history | Back up the actual files, including uncommitted work, untracked notes and conflict artifacts. A Git commit alone is not a complete vault backup. |
| `.mesh/mesh.db` | Search and note links can be rebuilt; pending reviews and usage/reuse history cannot. Embeddings require their original cache or a separately approved embedding run. Preserve the database. |
| Other `.mesh/` state | Preserve the directory securely, including queued operations, sync progress/conflicts, credentials, per-vault settings and extraction-cap slots. Do not restore stale process locks as proof of ownership or blindly replay old queued actions. |
| Connection database | Preserve the configured `connections.db` (normally `.mesh/auth/connections.db`), grant revocations and refresh history. It is separate from the knowledge index and must remain private. |
| Team hub database and configuration | Preserve the configured `hub.db`, membership/ACLs, identity bindings, deny records, sync/deletion history and deployment configuration. These may be outside the vault. Retain required signing secrets through the secret store, never a public archive or report. |
| IDE credentials | Native SecretStorage is separate from the vault. Do not export credentials into a Markdown backup; reconnect explicitly if native credentials cannot be recovered. |

Inventory actual configured paths, schema/build versions, backup time, file hashes,
owners and permissions. Treat backups as private: they can contain complete note
bodies, credentials and security records. Keep an independently recoverable copy;
a second directory on the same failed disk is not disaster recovery.

## Consistent backup

For the simple offline procedure, obtain approval for the exact services, stop
writers and readers cleanly, and verify they have stopped before copying. Freeze
hub changes and client sync too when taking a coordinated team snapshot. Record
which processes were stopped; do not start a second index owner.

Copy the complete selected state while quiescent, preserving private modes. Do
not copy only a live SQLite main file: committed data can still be in its WAL.
If graceful closure cannot be established, preserve the database and sidecars as
evidence and use a supported consistent database-backup procedure; do not delete
sidecars or assume an ordinary file copy is sufficient. A database-only online
snapshot does not by itself coordinate Markdown, queued operations and hub state.

Mesh does not currently provide an all-state backup/restore command. Use reviewed
operator tooling and verify the result independently; these instructions are not
an instruction to stop or replace any existing deployment without approval.

## Restore drill before cutover

1. Restore into a **new private directory**, with client sync, public routing,
   extraction and model providers disabled. Preserve the old deployment and
   original backup. Verify hashes and permissions before opening databases.
2. Use a compatible reviewed Mesh build. Do not open a newer authoritative auth
   or hub schema with an older binary as an improvised rollback.
3. Check the restored database and Markdown with `mesh doctor <restored-vault>`.
   If the index needs reconciliation, `mesh index <restored-vault>` is a write:
   preserve the restored copy first. Reindexing a readable supported database
   preserves pending reviews; discarding a corrupt database does not.
4. Check known note bodies and safety warnings byte-for-byte; search with allowed
   and denied scopes; check meaningful links, pending reviews, queued operations,
   sync/deletion state and conflict files against the backup manifest. No missing
   note, deletion or review should be silently labeled a successful full restore.
5. Verify authorization separately. A token revoked before the snapshot must stay
   rejected. **A backup cannot know later revocations, removals or permission
   reductions.** Keep public access and sync disabled until those later changes
   are reconciled from authoritative evidence. If that evidence is unavailable,
   keep affected access disabled and require reviewed re-provisioning/reconnection;
   do not guess that restoring old grants is safe.
6. Obtain a separate cutover approval bound to the exact host, build and snapshot.
   Start one index owner, then readers, and verify freshness and the actual client
   journeys. Enable team sync/public traffic only after deletion and security
   checks. Retain the pre-cutover state and a tested rollback plan.

Rolling back code does not authorize rolling back revocations. The
[browser sign-in guide](BROWSER-SIGN-IN.md#upgrade-and-validation) covers additional
hub identity-ledger and cookie-format restrictions.

If only Markdown survives, label the result **partial knowledge recovery**:
search and note links can return, but missing pending reviews, usage history,
embeddings and authorization/sync state have not been recovered. Do not run model
extraction to recreate lost candidates automatically or re-enable Stop extraction.

## Repeatable local evidence

From the module root:

```sh
go test -race ./cmd/mesh -run '^TestOfflineRestorePreservesReviewAndConnectionState$' -count=1
```

The synthetic fixture closes its stores, copies a fixed file manifest into a new
directory, runs the real index/doctor command handlers, and checks note bytes,
links, scoped search, the pending review and separate connection state. It verifies
that active grants can renew while previously revoked access/refresh tokens stay
denied. A Markdown-only negative control demonstrates the missing review queue.

It starts no watcher, HTTP server or model. It is **not** proof of a production
backup, provider permissions, post-snapshot revocations, live two-user sync,
rendered UI or deployment rollback. Those require independent launch receipts.
