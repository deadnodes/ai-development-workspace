# Private Products

Products are shared by default. Set `data.visibility` to `private` when creating
one, or use `set_product_visibility` later. A private Product and every row
attached to it are visible to its owner and invited members. Agents inherit
their owning human's access. A human with the instance admin role does not gain
access to another user's private Product merely by being an administrator.

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
