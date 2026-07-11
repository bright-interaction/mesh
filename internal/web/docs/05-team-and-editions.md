# Team sync and editions

## Team sync

Share a vault across a team with no git on any client. Each teammate runs:

```
mesh join https://hub.example.com <invite> my-vault   # clone, no git needed
mesh sync my-vault                                     # push yours, pull theirs
```

`mesh sync` is a three-way reconcile. Two people appending to the same page
auto-merge; a true overwrite of the same lines keeps the hub version and saves yours
to a `*.sync-conflict-*.md` sibling to resolve by hand. Deletes and renames
propagate, and the hub authors git history attributed to each user. Add
`mesh sync --watch` for real-time push.

## Trust and access (team hub)

A team hub is fail-closed and built for regulated teams:

- **Roles**: owner > admin > member > viewer, enforced on every write and admin action.
- **Access scopes**: horizontal partitions (for example `dev` and `sales`) so a member
  only ever retrieves, syncs, or is answered from the notes in their scope. Every read
  surface (search, fetch, neighbors, deltas, health, code tools, ask) is scope-filtered.
- **Audit log**: joins, role changes, invites, syncs, and curation are recorded.
- **GDPR**: per-user export and "forget" (revoke, tombstone authored notes, purge the
  user's audit rows).
- **SSO**: pluggable OIDC (Zitadel, Google, Okta, Entra, Auth0, any compliant issuer).
- **Per-member web login**: each teammate signs in with their own scoped account.

Everything runs on your own hardware (or an EU-resident hosted hub). The members and
their seats are managed from the `/team` page; this app and an agent reach every
capability with no shell.

## Editions

Mesh is open core, dual-licensed:

- The **open core** (this single-user app: vault, graph, retrieval, viewers, CLI,
  the MCP surface, and the sync client) is the Mesh Sustainable Use License (fair-code).
- The **team-sync hub** and the BYOAI conflict **curator** are a commercial product,
  available hosted or as a sovereign self-host license with support.

A commercial license to the core is available for uses the Mesh Sustainable Use
License does not fit.
