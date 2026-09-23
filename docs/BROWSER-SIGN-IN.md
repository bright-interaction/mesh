# Team browser sign-in

Mesh reuses the team's configured OIDC identity provider for browser sign-in.
It does not create another password database or accept Stage passwords/tokens.
The standalone local viewer is unchanged; hosted team identity uses the Pro hub.

## User journey

1. In the IDE, choose **Mesh: Connect** and note the displayed code.
2. The browser opens Mesh's consent page. If signed out, choose **Continue with
   your account**. The identity provider handles authentication/account recovery.
3. Mesh returns to the same request. Verify the account, code and scope, then
   click **Connect**. Sign-in alone never approves a connection.
4. Return to the IDE. It stores the connection in native secret storage and renews
   it automatically. **Mesh: Sign Out** revokes this native connection. Browser
   sign-out clears browser sessions, not independently approved native grants.

## Operator prerequisites

Configuration is explicit and requires a reviewed deployment; source support is
not proof that the public server has SSO enabled. Use a private staging account
first. Keep the existing scoped-key fallback until the real journey is verified.

The checked-in Compose deployment explicitly forwards the settings below. Both
services receive the same `MESH_UI_PUBLIC_URL`; only the hub receives provider
credentials and admission policy. All sign-in URLs and provider credentials
default to empty, and automatic enrollment defaults to `false`. Upgrading the
image alone does not enable SSO, enroll users or change their existing scopes.
Setting a variable in `.env` does nothing on an older Compose file that does not
forward it: deploy the reviewed configuration together with both images.

Use the deployment secret store for the client secret. Never paste a resolved
`docker compose config` output into chat or a report: it contains environment
secrets. Syntax-check production configuration with `docker compose config
--quiet` instead. From the private deployment checkout, the regression check
below uses only isolated fake values and does not start containers or contact an
identity provider:

```sh
MESH_COMPOSE_E2E=1 go test ./internal/hub -run '^TestCompose' -count=1
```

With no explicit connection-database override, Compose keeps native grants at
`/var/lib/mesh-hub/vault/.mesh/auth/connections.db` on the existing persistent
volume. An override must also point inside protected persistent storage.

- Run current Pro hub and viewer builds against the same protected hub database.
  Route `/auth/oidc/*` to the hub and the configured viewer base (for example
  `/app/*`) to the viewer on **one HTTPS origin**. Do not expose internal ports.
- Configure the hub's `MESH_HUB_OIDC_ISSUER`, `MESH_HUB_OIDC_CLIENT_ID`,
  `MESH_HUB_OIDC_CLIENT_SECRET`, and `MESH_HUB_OIDC_REDIRECT_URL`. Register the
  exact HTTPS callback `/auth/oidc/callback` with the provider. Inject secrets
  through the deployment secret store, never repository files or chat.
- The issuer and discovered authorization, token and signing-key endpoints must
  all use HTTPS. Discovery, token exchange and signing-key requests do not follow
  redirects; configure the provider's final endpoints directly. Back-channel
  requests have bounded deadlines and do not share browser cookies.
- Set `MESH_UI_PUBLIC_URL=https://mesh.example/app` on **both** processes. This
  pins the viewer audience and the hub's allowed post-login return destinations.
- On the viewer, set `MESH_UI_HUB_DB` and
  `MESH_UI_HUB_SIGN_IN_URL=https://mesh.example/auth/oidc/login`. The latter must
  have the same HTTPS origin as `MESH_UI_PUBLIC_URL`. This intentionally refuses
  cross-host cookie sharing or an arbitrary user-supplied sign-in URL.
- The viewer's separate `connections.db` must be private and persistent (default
  `<vault>/.mesh/auth/connections.db`; override `MESH_UI_CONNECTIONS_DB`). Back it
  up as sensitive operational state; it is not the rebuildable knowledge index.
- Bootstrap a controlled owner using the existing operator invite workflow
  before opening SSO enrollment. SSO cannot bootstrap an owner on an empty hub.
- New-account enrollment is **off by default**. Existing explicitly bound
  identities can still sign in. To enable automatic enrollment intentionally, set
  `MESH_HUB_OIDC_AUTO_PROVISION=true`, a non-empty
  `MESH_HUB_OIDC_ALLOWED_DOMAINS`, and `MESH_HUB_OIDC_DEFAULT_SCOPE`.
  The scope must already exist; `dev` is an explicit all-scope grant and must not
  be selected casually. `MESH_HUB_OIDC_DEFAULT_ROLE` defaults to `viewer`; only
  `viewer` and `member` are accepted, never `admin` or `owner`.
- Enrollment requires an explicitly true `email_verified` claim and an exact
  allowed email domain. Missing verification is not treated as verified. Restrict
  the IdP application to the intended tenant/group as well: a shared consumer
  email domain is not organization membership. Review seat limits and folder ACLs.

SSO identities are bound to the exact verified issuer and stable subject, not to
email, name or preferred username. New accounts have an opaque authorization
principal so editable profile claims cannot inherit an existing user's ACLs.
Deleting a member leaves a hashed identity deny record in the authoritative hub
database, including through user-removal and forget paths. A new SSO exchange
for that identity stays denied until explicit operator readmission. Keep these
records in backups; they must not be deleted by ordinary account cleanup.

This denies the same identity, not every possible future account belonging to
the same person. Complete offboarding also removes access at the identity
provider, especially when automatic enrollment is enabled.

The viewer verifies the hub cookie read-only against the current member and role.
No password, provider token or Mesh access key enters the IDE/webview. A full
connection scope cannot exceed the account's current permissions.

## Upgrade and validation

Hub session cookies now have a signed, server-enforced 30-day expiry. Existing
legacy hub browser sessions without an expiry must sign in once after upgrade;
team access keys are unchanged. OIDC flow state has a signed 10-minute expiry.
Deploy hub and viewer together so they agree on the cookie format. Preserve the
database and secrets on restart. A rollback to the old cookie format also requires
browser reauthentication; do not restore old grant state to undo revocation.

The new additive identity tables are authoritative security state, not derived
metadata. Back up `hub.db` before approved migration. Existing subject-only SSO
accounts are **not** silently associated with whichever provider is now configured.
An operator must verify the original issuer/subject and explicitly bind each
legacy account. Existing team keys, roles and scopes are preserved. Revocations
predating this migration cannot be reconstructed from already-deleted rows; keep
automatic enrollment disabled until the pre-existing membership audit is complete.

Use the existing client list to select the exact member. Then, on the trusted
hub host, use the supported command (example identifiers, not a command to run
against an unknown deployment):

```sh
mesh-hub oidc-bind --repo /srv/mesh/vault --db /srv/mesh/hub.db \
  --id 7 --expected-user alice --issuer https://identity.example/tenant \
  --subject provider-stable-subject --apply
```

The command requires the repository/database association, exact member name,
and explicit `--apply`. It preserves that member's permissions and records an
audit event. A revoked identity additionally requires `--readmit` and an existing,
deliberately provisioned target member. This never resurrects the removed member's
old client ID, tokens or native connection grants. Email matching is not a binding
mechanism. Do not remove identity deny rows or edit the database to bypass this.

Do not roll back an enabled SSO deployment to a build without the admission
ledger: older code could ignore revocations or issuer binding. If such a rollback
is unavoidable, disable SSO routes/enrollment first and retain the protected
database; use the scoped-key compatibility path until a supported build returns.

Before launch, verify the actual configured provider with two distinct users:
fresh sign-in, retained code, explicit consent, role downgrade, denied user,
expiry, renewal, native disconnect, browser logout, account removal followed by
a fresh rejected SSO exchange, explicit readmission and provider cancellation. Also
verify recovery/rollback independently. The local TLS fixture exercises a real
authorization-code/S256 exchange and ID-token signature verification, but it is
not a production-provider or installed-client acceptance receipt.

Protocol references: [OpenID Connect Core](https://openid.net/specs/openid-connect-core-1_0.html)
and [OAuth security best practices](https://www.rfc-editor.org/rfc/rfc9700.html).
