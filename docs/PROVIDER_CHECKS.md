# Native checks and build publication

An optional operator recipe binding publishes one stable GitHub App check for
the exact source SHA executed by a native worker. It reuses WorkerJobs, Attempts
and existing Operations; workers still execute the checks/build, and the
controller publishes their accepted result through the existing GitHub adapter.
There is no Actions dispatch or second CI engine.

## Operator prerequisites

The inactive fragment [check-binding.json](../examples/workers/check-binding.json)
illustrates the `check` field of a trusted `local_oci_build` recipe. It is not a
complete worker configuration and is never loaded automatically. Enabled recipes
must have an empty parameter schema. Set a stable `RCP / ...` name, the expected
GitHub App numeric ID, and a finalization allowance of 60–600 seconds. Recompute
the ordinary recipe hash and update the immutable server/worker configuration
revision together; the binding is part of that recipe identity.

The source repository must already have an imported registered provider binding
with its numeric repository ID and canonical owner/name. The connection App ID
must equal `expected_app_id`. Check name ownership is checked across configured
recipes/Products for the same physical API/App/repository, including registered
numeric aliases. A missing or contradictory mapping fails closed. Recipes with
no binding retain their existing behavior, including parameterized recipes.

The App needs repository **Checks: write** access. The adapter requests an
ephemeral installation token with `repositories: [exactRepositoryName]` and
`permissions: {checks: write}`; it validates the response's exact repository
numeric ID/name, permission and bounded expiry. Its cache is isolated from the
ordinary installation token cache. A name reused for another numeric repository
cannot silently acquire this authority. There is no broad-token repository
lookup, App permission change or new secret. Existing private-key references,
HTTPS/API allowlisting, redirect rejection and bounded responses still apply.

Selecting this App and name in branch protection is a separate owner-managed
configuration step. This source implementation does not enable branch
protection, update an App installation, create credentials or write live checks.
The REST contract follows the existing
[GitHub Checks API](https://docs.github.com/en/rest/checks/runs?apiVersion=2022-11-28)
with the adapter's existing `2022-11-28` request header.

## Meaning and ordering

Both standalone `queue_worker_job` and managed native delivery create a durable
`CHECK_PUBLICATION` child alongside the existing worker operation. Its immutable
snapshot binds the job/input/recipe/configuration, Product, registered source
repository, source connection/App and exact source SHA. The external ID derives
from the child operation, not a caller-selected value.

The worker cannot claim until the initial queued check is observed. A running
attempt can publish in-progress status. Completed success requires an accepted,
timely PASSED report with the exact verified local OCI receipt and current
authority; managed publication and GitOps PR creation wait for that check result.
A successful check means the declared **checks and build** succeeded. It does
not mean the artifact was published or deployed, that every individual Make
stage has a separate context, or that optional compiler-local PostgreSQL/Docker
tests ran. Registry publication can still fail after truthful build success.

Failed checks/execution, cancellation and expiry publish non-success outcomes
when immutable origin and current authorization still permit reporting. Unknown
execution remains unresolved/in-progress and cannot turn green. Standalone old
SHA results remain associated with that SHA after a branch moves. Managed
supersession, cancellation, configuration drift, grant revocation and quarantine
still fence admission and downstream work. A provider write completing after a
post-I/O fence does not imply that the physical remote write was reversed.

## Uncertain writes and deadlines

CREATE and UPDATE intent, exact output and timestamps persist before network
I/O. A lost CREATE response is reconciled by bounded exact-SHA/name/App listing;
unrelated external IDs are ignored, exact duplicates or identity mismatches are
rejected. Pagination must be complete and consistent. A missing result after an
uncertain CREATE never triggers a replacement POST. A lost PATCH response is
reconciled only by reading the adopted check ID and comparing exact frozen
identity/content. Neither path blindly repeats a write or regresses its stage.

The child has a fixed finalization deadline equal to the execution deadline plus
the operator allowance, with a finite observation budget/backoff. This permits
timely accepted results, or a timeout first discovered after expiry, to finish
reporting during the allowance. It never extends worker execution/report
authority or admits a late PASSED report. Exhausted or ambiguous publication is
visible as failed/reconciliation attention and cannot be treated as success.
HTTP 401/403/404, rate limits and provider errors do not expand permissions or
recreate checks. Backup/reopen validate the typed nested state; restore cancels
active children and quarantines the Product, including against terminal-metadata
resurrection. Operator investigation is required for unresolved publication.

## Source acceptance fixtures

The HTTP adapter tests use fake RSA credentials and an allowlisted local server.
They cover narrow-token/cache isolation, lifecycle enums/timestamps, strict
identity/output, unrelated and duplicate checks, pagination truncation,
malformed/oversized responses, errors, redirects and lost replies. They never
call GitHub or print credentials/provider response bodies.

The opt-in `TestRealNativeChecksWorkerPublisherPR` uses a protected ephemeral API,
real existing native worker/BuildKit, durable spool, separate publisher,
Distribution and the concrete GitHub checks adapter against that local server.
Both local and isolated PostgreSQL lanes exercise queued/in-progress/success,
lost CREATE/PATCH replies and actual failed source checks without publication or
a GitOps PR. Lightweight protected standalone API cases in the same fixture
exercise cancellation, expiry and a missing executor after START: they never
manufacture a PASSED report or replay execution. Core application tests also
cover authority and restore fences. Run the actual fixture with the existing isolated
test database and `RCP_TEST_DOCKER=1`; ordinary runs honestly skip it.

GitHub may supply the App homepage as `details_url` when no URL was requested.
The adapter ignores that provider field: it is never followed, persisted or used
as authority. Requests contain no caller-selected details URL or annotations.

These native Linux arm64 fixtures do not prove native x86, live App/branch-policy
adoption, fleet recovery, retention, full compositions/conflicts or the A+B Dev /
selective Prod pilot. Those remain separate requirements of the full roadmap.
