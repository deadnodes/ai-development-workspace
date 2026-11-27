# Private Products

Products are shared by default. Set `data.visibility` to `private` when creating
one, or use `set_product_visibility` later. Among human users, a private Product
and its records are visible only to its owner and invited members. Agents inherit
their owning human's access. A human with the instance admin role does not gain
access to another user's private Product through their own login without an
invitation. Operator-configured infrastructure tokens (any `service` or
`worker` principal) do not need membership. They can see and act on private
Products only within their configured permissions and Product, environment,
or other applicable scope. An operator who can mint service tokens is trusted
infrastructure.
New shared Products also record their authenticated creator as owner (the
owning human for an agent). An authenticated human with Product write access
can claim an unowned Product or one with the synthetic owner `local` through
`adopt_product_ownership`. An administrator can explicitly name a human
`user_id` for recovery. Agents cannot adopt ownership, and changing visibility
never assigns an owner. A credential with a limited Product grant must
explicitly choose an ID in that grant; server-generated IDs require `*`.

The Product owner invites a person with `add_product_member` and the person's
`user_id`. The person can print that identity with:

```sh
release-control whoami --url https://control.example --profile default
# or: release-control whoami --url https://control.example --token-env RCP_TOKEN
```

The command reads `GET /auth/me` and prints the principal kind, ID, owner, and
the user ID to provide to the Product owner. The owner can remove members with
`remove_product_member`; the owner cannot be removed. Owners alone may change
visibility and manage members. The trusted bootstrap service can manage
visibility and membership for instance recovery.

Trusted local processes without a principal see all Products. The bootstrap
service principal also sees all Products and whole-instance backups include
private Products. Ordinary users and agents cannot use backups to access
private Products they cannot see.

The trusted `service/bootstrap` principal has instance-wide recovery scope:
it can inspect every Product and perform whole-instance backup and restore.
Shared Products remain visible under the existing Product permission rules.

Navigation and project summaries show visibility, owner, and members only for
visible Products. A hidden Product or one of its records is indistinguishable
from a missing record on direct reads. Navigation and browser projections
filter out hidden rows before applying limits. If a navigation cursor's anchor
is no longer visible or has been deleted, restart the traversal without it.

The same visibility rules apply to indexed search, excerpts, graph neighbors,
worker and runtime reports, reference checks, and Product configuration exports.
Workers can claim and report work for private Products within their configured
Product, repository, and capability scope. Runtime reporters can submit within
their configured Product, environment, and cluster scope, but cannot read
reports. Production promoters remain bound to their configured Product and
environment. The public production-signature endpoint never exposes private
Products and responses are not cacheable.
Whole-instance backup fails rather than returning a partial archive if the
caller cannot see every Product; use bootstrap for instance recovery.
For authenticated callers other than bootstrap, audit and other retained rows
are hidden if their Product no longer exists, including in backup exports.
The journal is retained for recovery, and a deleted Product ID cannot be
reused while its deletion entry is retained. Restore summaries carry only
instance-level metadata; original interrupted or quarantined records are
recorded in separate Product-scoped evidence events.

Workspace import preserves the source Product's visibility but assigns
ownership to the importing user (the owning human for an agent). The source
membership list is cleared; the importing owner already has access. A private
workspace cannot be imported without a user identity and is rejected before
anything is written.
