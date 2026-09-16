# Local Git through MCP

The Control Plane matches registered repositories to local checkouts. It reads Git
state and persists sync plans/results. The host coding agent executes Git with its
existing credentials; the service never stores those credentials, edits source,
or fetches/merges automatically. A read-only Docker workspace mount is sufficient.

1. `match_local_repositories({product_id, actor})` links attached repository URLs
   to local SSH/HTTPS origins without creating unrelated repositories.
2. `get_project_context({product_id})` returns checkout IDs and relative paths.
3. `get_local_git_state({product_id, checkout_id})` returns branch, exact HEAD,
   dirty state, cached origin upstream SHA and ahead/behind. **Cached remote refs
   are not live remote evidence.** Each observation has a timestamp.
4. `plan_local_git_sync({product_id, checkout_id, actor, mode: "FETCH",
   expected_head: "<exact SHA>"})` persists a plan and returns `git_arguments`.
5. The agent resolves `relative_path` under its trusted host workspace, checks
   origin identity and exact HEAD again, reviews local Git config/credential
   helpers, and runs the arguments using `git -C <host-checkout> ...`.
6. `record_local_git_sync({product_id, checkout_id, actor, plan_id})` re-reads state
   and appends audit evidence. A FETCH record says `OBSERVED`, not that network
   execution was proven. An unchanged upstream after fetch is a legitimate result.

To update the checked-out branch, request a separate plan with
`mode: "FAST_FORWARD"` and the newly observed exact HEAD. It requires a clean,
attached branch strictly behind origin, with no local commits or submodules. The
plan pins the observed upstream SHA and uses `merge --ff-only`; recheck branch,
HEAD, clean state and upstream SHA before execution. Recording requires the exact
planned target and original branch, with a clean working tree. Never substitute
reset/rebase/force/push or resolve conflicts automatically.

The returned `path` belongs to the server and may be a container mount. Use the
relative path to resolve a checkout on another machine. A client cannot ask the
service to inspect arbitrary paths. Origin must still match the registered
repository. Includes, configured filters, alternate object storage and unsupported
worktree configuration require external-agent inspection instead of server reads.
Submodule contents are not inspected; fast-forward plans refuse submodules.

Plans are `PLANNED` / `EXTERNAL_AGENT`, not completed operations. The service
records `plan_local_git_sync` and `record_local_git_sync` append-only audit events.
A plan never constitutes permission to overwrite developer work.

HTTP equivalents:

- `GET /api/products/{product_id}/local-checkouts/{checkout_id}/git`
- `POST /api/local-git/plan` with the same plan fields
- `POST /api/local-git/record` with the same result fields
