# Private Products

Products are shared by default. Set `data.visibility` to `private` when creating
one, or use `set_product_visibility` later. A private Product and every row
attached to it are visible to its owner and invited members. Agents inherit
their owning human's access. A human with the instance admin role does not gain
access to another user's private Product merely by being an administrator.
New shared Products also record their authenticated creator as owner (the
owning human for an agent). Only a genuinely ownerless Product can be claimed
via `set_product_visibility`. A credential with a limited Product grant must
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

The instance admin role does not bypass Product membership for reads or
commands. The trusted `service/bootstrap` principal is the recovery exception:
it can inspect every Product and perform whole-instance backup and restore.
Shared Products remain visible under the existing Product permission rules.

Navigation and project summaries show visibility, owner, and members only for
visible Products. A hidden Product or one of its records is indistinguishable
from a missing record on direct reads. Navigation and browser projections
filter out hidden rows before applying limits. If a navigation cursor's anchor
is no longer visible or has been deleted, restart the traversal without it.

The same visibility rules apply to indexed search, excerpts, graph neighbors,
worker and runtime reports, reference checks, and Product configuration exports.
Worker credentials authorize capabilities, not Product membership; a worker
without a human identity cannot claim or replay work for a private Product.
The public production-signature endpoint never exposes private Products.
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
