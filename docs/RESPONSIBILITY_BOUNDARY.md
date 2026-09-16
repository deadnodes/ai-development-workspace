# Metastore and control plane boundary

Release Control Plane is the authoritative **metastore for development and delivery**, and a control interface for developers, coding agents and external executors.

Its database contains project configuration, intent, plans, commit references, ownership, decisions, discoveries, handoffs, verification evidence, artifact provenance, desired compositions, operations and history. It does not become the storage or editing backend for managed applications.

## Responsibilities

| Concern | Control Plane | Executor / storage |
| --- | --- | --- |
| Understand and continue work | Generate context, record decisions and handoff | Agent or developer implements the change |
| Source changes | Record repository/branch/SHA/PR relationships and observations | Developer/agent edits files; Git stores source and commits |
| Branch/release composition | Select immutable inputs, plan actions, apply policy, track conflicts | Git provider performs permitted ref/merge operations; agent resolves semantic conflicts |
| Builds and checks | Request execution, associate inputs/results/evidence | CI or agent executes build/test code |
| Artifacts | Retain digest, source provenance and availability | Registry stores image bytes |
| Deployment | Select versions, produce desired state, track operations | GitOps/Flux/Kubernetes perform rollout |
| Application business data | No ownership or direct access | Managed application and its data stores |

The service must not edit application source files, implement features, resolve code conflicts, execute arbitrary shell/SQL, run application migrations, modify business records, or become a proxy for accessing application databases. Its own PostgreSQL migrations and backup/restore affect only the Control Plane metastore.

Commit management means relationships, provenance, branch/ref operations and explicit composition intent. It does not mean creating an alternate source history database or rewriting meaningful developer history silently. Agents produce code changes using their own development environment and credentials, then report commits and evidence through MCP. Future conflict assistance supplies both semantic contexts and required checks; resolution remains an agent/developer task.

## Existing external mutations

Current code can dispatch a configured GitHub Actions workflow and update the configured DEV GitOps image fields after an explicit deployment request and policy checks. This is orchestration of build/deployment configuration; it is not application-source editing. No background source-editing or business-data mutation capability exists.

The GitOps adapter's bounded image mapping is the current write boundary. Do not turn it into a generic repository file editor. Automatic build and DEV mutation permissions remain opt-in. GitOps writes are still real external changes and must be represented by the requested operation and audit; do not describe the whole service as read-only.

Workflow execution can itself have effects, so a configured build-only contract is required; the Control Plane must not silently substitute an arbitrary deploy/migration workflow. No direct Kubernetes patching is used for normal release execution.

## Agent interaction

Read context → claim/start integration → work externally → link commits → record progress/evidence → request permitted orchestration → inspect results → hand off. All semantic state is recorded through UI/API/MCP. Git configuration mirrors are derived exports. Full backup/restore transfers Control Plane state, not managed applications or their business data.
