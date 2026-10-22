# Grouped candidate Dev proposals

`deploy_candidate_dev` prepares one image-only Flux proposal for all selected
components sharing a physical GitHub API, numeric repository and target ref.
Two components in the same Helm file therefore share a commit and a PR.
Different physical repositories or refs remain separate groups; a partial
outcome across them retains the reservation and is reported explicitly.

The command's inputs are unchanged. The same attributed owner supplies the
reservation receipt, a new request key and the exact current selection. RCP
derives membership, leader, selectors, destinations and artifacts. Callers
cannot submit a batch, manifest, replacement digest, approval or target.
The [physical baseline](DEV_BASELINE_CAPTURE.md) must already be captured.

## Current resource and desired image

Each component has two distinct image identities:

- Its frozen baseline repository selects the current Dev resources.
- Its admitted candidate artifact supplies the desired repository and digest.

For example, resources currently using `registry.example/dev/api` can receive
the exact native candidate from `registry.example/prod/api`. The current
selector remains pinned to the baseline, and the candidate's original build
profile, source and publication provenance remain unchanged. No registry
authority is inferred from either name.

All selected components at one physical destination must use the same exact
configured GitOps connection. Different credential/configuration aliases at
that destination fail closed instead of splitting overlapping proposals.

Before a proposal, every member's existing artifact is inspected and its
current GitOps image and blob are checked against the captured baseline.
Reads use a common stable head containing the captured head. An unrelated
branch advance can be incorporated only while every selected blob remains
unchanged. No member can be omitted after a failure.

## One bounded patch

Only the existing root `image.repository`/`image.digest` mapping or HelmRelease
`spec.values.image.repository`/`spec.values.image.tag` mapping is supported.
All replacements in a shared file are derived from its original bytes.
Overlapping scalar targets, ambiguous selectors and collisions with unselected
resources fail before proposal writes. Unrelated resources, fields, comments,
formatting, file modes and paths remain unchanged.

The proposal has one operation-owned branch, one commit with the pinned base
as its only parent, and one PR per group. The writer never updates the target
ref, merges a PR, deletes a ref or edits application source. It does not rebuild
or republish the candidate.

The GitHub implementation uses the existing tree as `base_tree`, writes only
the permitted file entries, and creates a commit with that base as its single
parent. The wire contracts are documented in GitHub's
[tree](https://docs.github.com/en/rest/git/trees#create-a-tree) and
[commit](https://docs.github.com/en/rest/git/commits#create-a-commit) APIs.

Limits are 16 members and files, path depth 16, 1 MiB per file, and 4 MiB each
for the total original and rendered bytes. The typed batch request is limited
to 64 KiB and its activation/group proof and persisted batch/results to 256 KiB.
New batch proof reads are bounded to 128 requests and 16 MiB of responses,
with at most 2,048 entries per tree directory. Exceeding a limit fails closed;
it never drops members or paths. New batch state stores typed metadata and
hashes rather than raw YAML or provider payloads.

## Unknown outcomes

The complete prepared batch and its hash are committed as durable intent
before the first possible mutating HTTP send. A latched callback checks the
leader's lease and every member's current authority immediately before each
such send, including authentication and redirects.

Grouped admission and preparation use cached authentication for their GitHub
reads as well. A cold or expired source/GitOps connection fails closed before
proposal writes instead of creating a token before intent. Existing separately
permitted ordinary observations can populate the exact connection caches;
proposal recovery never performs that refresh. Restarting RCP does not preserve
those in-memory sessions or grant new credentials.

Only the group's leader proposes and observes the PR. Followers wait without
falling back to individual legacy writes. A lost response or crash leaves an
uncertain intent. Recovery uses GET-only proof with an already cached usable
token; it never refreshes authentication or recreates a tree, commit, ref or PR.
A partial object/branch without the complete exact PR remains unresolved.

The verifier compares the complete before/after tree, including every sibling
and unselected subtree. It hashes blob bytes and reproduces the entire allowed
image patch, then checks repository, base, single parent, commit marker, PR and
stable head. A successful POST response alone cannot admit a PR receipt.

A previously verified exact PR may remain observable after an external merge
deletes its proposal branch, using its persisted pinned head and immutable
commit/tree proof. A missing branch does not grant write permission. Changed
or ambiguous evidence retains the hold.

## After an external merge

RCP observes every member's desired image at one common stable target head
containing the verified PR commit. It records all child results atomically
under the leader's current lease and the final member guards. Each child keeps
its own artifact, path and blob evidence while sharing the group PR/commit.

`GITOPS_APPLIED` still describes desired state. Use the separate
[complete runtime verification](DEV_RUNTIME_VERIFICATION.md) for all configured
roles, including workers. Neither result is functional acceptance or permission
to restore, unlock or promote production.

The candidate's existing one-hour execution fence, original baseline roots,
backup quarantine and historical replay rules remain. Unknown, cancelled or
failed effects never release the reservation automatically. The governing
human-only protected Flux merge policy continues to apply. This source
protocol does not update the running control plane or authorize a live rollout.

Verified restoration remains a separate lifecycle: an exact reviewed restore
PR, separate rollback confirmation, post-restore runtime proof and a scoped
unlock are still required.
