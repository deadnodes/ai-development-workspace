# Human and agent authentication

`RCP_AUTH_MODE=local` preserves the trusted local installation and its optional
legacy `RC_TOKEN`. Public installations must explicitly use
`RCP_AUTH_MODE=oidc`. OIDC mode fails startup when required configuration is
missing and ignores the legacy shared token. It protects both HTTP API and MCP;
static login assets and health endpoints remain publicly reachable.

All domains, realm/client identities, role names, storage paths and secrets are
operator configuration. No organization-specific identity service is compiled
into the application.

| Environment variable | Meaning |
| --- | --- |
| `RCP_AUTH_MODE` | `local` or `oidc`; omitted defaults to local only when public/OIDC settings are absent |
| `RCP_PUBLIC_URL` | Exact HTTPS public origin, without a path, query or fragment |
| `RCP_OIDC_ISSUER_URL` | Exact HTTPS issuer, including realm path when applicable |
| `RCP_OIDC_CLIENT_ID` | Dedicated OIDC client audience |
| `RCP_OIDC_CLIENT_SECRET` | Confidential client secret when required by the provider |
| `RCP_OIDC_ROLES_CLAIM` | Dot-separated claim path; default `realm_access.roles` |
| `RCP_OIDC_ALLOWED_ROLES` | Comma-separated allowed membership roles |
| `RCP_OIDC_ADMIN_ROLES` | Comma-separated instance administrator roles |
| `RCP_AUTH_PRODUCTS` | Comma-separated product grants for admitted humans; default `*` |
| `RCP_AUTH_ENCRYPTION_KEY` | Base64 encoding of exactly 32 random bytes, supplied from a secret backend |
| `DATABASE_URL` | PostgreSQL connection; authentication uses its own private table |
| `RCP_AUTH_FILE` | Separate bbolt authentication file when PostgreSQL is absent; required in that mode |
| `RCP_AUTH_SESSION_TTL` | Human session lifetime, default `8h`, bounded to `1m`–`24h` |
| `RCP_AUTH_BOOTSTRAP_TOKEN` | Optional independently random service bootstrap secret, at least 32 characters |

For Keycloak, enable the authorization-code flow for a dedicated client and allow
only the exact redirect URI `${RCP_PUBLIC_URL}/auth/callback`. Publish the role
claim in ID tokens. Configure the issuer to require PKCE S256 as defense in
depth. At least one membership/admin role must be configured. A valid signed ID
token without an allowed role does not admit the user. Client roles may use a
claim path such as `resource_access.rcp.roles` when the client name contains no
dots. No email/name/profile claim is needed.

`GET /auth/login` initiates authorization code + PKCE S256 with a random browser
state cookie and nonce. The callback verifies the issuer, signature, audience,
expiry, nonce, one-use state and browser binding through `coreos/go-oidc` and
`oauth2`. The redirect URI always comes from configuration, never request headers.
A return location is limited to same-origin paths. Provider access/refresh tokens
are never persisted or used as agent credentials.

Human sessions use random opaque `__Host-rcp_session` cookies with Secure,
HttpOnly, Path=/ and SameSite=Lax. Session verifiers are SHA-256 hashes. Mutation
requests require the configured exact Origin or (when Origin is absent) the
session-bound `X-CSRF-Token` returned by `GET /auth/me`. A foreign Origin is
rejected even if the token is supplied. Approval forms carry a hidden CSRF token.
Logout deletes only the human session. Identity and role changes at the issuer
are picked up on the next login; existing sessions expire within the configured
TTL. This implementation does not subscribe to provider back-channel logout.

`GET /auth/me` returns `{auth_mode, principal, csrf_token?}`. A principal includes
`id`, `kind`, `role`, `products` and `permissions`. Human IDs are deterministic,
keyed pseudonyms of issuer + subject, rather than email, name or caller labels.
Human permissions are read/write/deploy; configured administrators additionally
receive admin permission. The application enforces product and action policies
and derives audit actors from this identity. Production approval requires a
human principal. A service bootstrap principal has the explicit identity
`service/bootstrap`; it cannot approve device enrollment or act as a human.

## Independent agent enrollment

1. POST `/auth/device/code` with JSON
   `{"name":"workstation agent","scope":"read write","products":["product-id"]}`.
   Scope accepts only read/write/deploy. Omitted scope defaults to read/write;
   omitted products use configured product grants.
2. Display the returned `user_code` and `verification_uri_complete` to the human.
   The response includes `device_code`, `verification_uri`, `expires_in` and
   `interval`. Codes expire after 10 minutes; initial polling interval is 5s.
3. The human opens the verification page and logs in. The page displays the exact
   requested permissions, product scope and untrusted agent-supplied name. Only
   explicit, CSRF-protected approval grants the credential. Service and agent
   identities cannot approve. A human cannot delegate more than their own scope.
4. Poll POST `/auth/device/token` with JSON
   `{"grant_type":"urn:ietf:params:oauth:grant-type:device_code","device_code":"…"}`.
   Wait for `authorization_pending`, increase the interval by 5s on `slow_down`,
   and stop on `expired_token` or `access_denied`.
5. Store the returned `access_token` in private local credential storage and send
   it as `Authorization: Bearer …` to API/MCP. Do not log or commit it.

The token has its own agent identity and never expires automatically. It remains
valid independently of browser sessions and issuer refresh tokens until revoked.
Concurrent/retried redemption of the same approved device code atomically creates
one agent and returns the same token until the original device-code expiry. This
recovers a lost response safely: the server derives the token with HMAC from the
high-entropy device secret and server key, and stores only its hash verifier.
The short human code cannot recover a token. Revocation also disables recovery.
After code expiry, a lost credential requires fresh enrollment and revocation of
the old entry; there is no secret-reveal API. Rotation uses fresh enrollment,
client credential replacement and explicit revocation of the old credential.

GET `/auth/agents` lists metadata for the current human's agents (all agents for
an administrator). POST `/auth/agents/revoke` with `{"id":"agent/…"}` immediately
revokes an owned agent; administrators may revoke any agent. Tokens cannot list
or revoke other tokens. Logout does not revoke agents.

## Private persistence and operation

Authentication state is excluded from `domain.State`, public snapshots, MCP
context, domain backups and configuration exports. PostgreSQL stores one
transactionally locked AES-256-GCM encrypted payload in `rcp_auth_private`.
bbolt uses a distinct `RCP_AUTH_FILE`, opened with mode 0600, and transactional
updates. Use a different file from the domain store. The record contains token
hashes, pseudonymous ownership, encrypted metadata, encrypted short-lived PKCE
flows, device grants and sessions. Each write uses a fresh random GCM nonce and
version-specific authenticated associated data. Authenticated read failures fail
closed; a wrong encryption key prevents startup.

Back up the private authentication table/file separately together with the
external encryption secret. Moving only a domain backup does not migrate users,
sessions or agent credentials. Full database migration preserves the private
table, but the destination must receive the same encryption key. Losing the key
makes the data unrecoverable. Automatic encryption-key rotation is not implemented;
changing it without an explicit decrypt/re-encrypt migration invalidates private
state and changes pseudonymous human identities.

The singleton encrypted record is intended for small control-plane installations;
PostgreSQL row locks serialize auth updates across instances. Device/login entry
points have bounded in-memory per-peer and global rate limits, bounded active
flows and device grants, expiration and bounded polling intervals. Rate limits
are per process and use the direct peer address, not untrusted forwarded headers.
Use gateway limits for distributed abuse protection. Keep replicas on the same
issuer/configuration/encryption key; use PostgreSQL for multiple replicas.

Local tests use a TLS mock OIDC issuer and real signatures to cover issuer,
audience, nonce, state, expiry, role and PKCE rejection, concurrent redemption,
recovery, revocation, logout independence, CSRF and encrypted restart persistence.
`TEST_DATABASE_URL` enables a real PostgreSQL encrypted-store restart test in an
isolated temporary schema. A skipped PostgreSQL test is not database evidence.
