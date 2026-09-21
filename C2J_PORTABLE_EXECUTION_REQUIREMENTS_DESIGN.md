# Design: Portable Execution Requirements and Environment Handoff

## Status

Implementation in progress for the requirements in `C2J_FEATURE_REQUESTS.md`.

Implemented so far: `pkg/execution` provides shared requirement/allocation
types, canonical validation, field-wise overlays, and compatibility diagnostics.
See [the package guide](pkg/execution/README.md) for the implemented API.
`cmd/c2j/internal/executionflags` provides shared per-field argument/environment
parsing and explicit list-filter opt-in validation, with precedence tests.
Recipe declarations, CLI wiring, and durable environment handoff are not yet
enabled; the remainder of this document describes the target behavior.

Dependency checkpoint: the pinned JobDB v0.0.13 and the currently available
v0.0.17/main lack generic workflow-context payload access and suspension, and
do not preserve arbitrary payload fields through all task completion paths.
Integrating the handoff requires a JobDB source/release decision. Do not enable
constrained execution until those generic guarantees are available and tested.
The outstanding upstream contract is recorded in
[JobDB workflow payload requirements](JOBDB_WORKFLOW_PAYLOAD_REQUIREMENTS.md).

This document treats the request's example YAML, command names, and suggested
storage choices as illustrative. It preserves the underlying behavior while
choosing interfaces that fit the current c2j and JobDB architecture.

Reviewed against c2j commit `69812e7f182beee1c5df039821e1f9310b392205`
and `github.com/colony-2/jobdb v0.0.13`.

## Decision Summary

Keep execution requirements in the recipe and c2j's durable continuation data.
JobDB remains an opaque payload and lease/reschedule service; it does not match
CPU, memory, storage, image, or platform when granting a lease.

- c2j owns the recipe declaration, quantity/image/platform validation,
  requirement overlay, executor preflight, suspension directives, and CLI
  presentation.
- A trusted provisioner supplies the actual usable allocation to c2j when an
  execution process starts. Each allocation field can be passed as a CLI
  argument or its corresponding environment variable, following c2j's
  existing argument-over-environment precedence.
- JobDB owns the existing lease, durable opaque reschedule payload, and atomic
  release of the old lease. Its workflow layer must preserve c2j's payload
  section across ordinary waits and task handoffs.
- A recipe declaration supplies the immutable base requirements. Optional
  job-level requirements override the corresponding base properties; their
  overlay is the current effective demand.
- Submission metadata may carry initial requirements for early provisioning.
  When it is absent or incomplete, the provisioner starts a default
  environment; the executor itself loads the recipe and checks its allocation
  before requirement-dependent work.
- Runtime work changes the job requirements by suspending for new execution
  requirements through JobDB's existing reschedule model and opaque payload.
  It does not execute a special recipe operation.
- When initial preflight finds an insufficient environment, c2j reschedules
  with a complete current-requirements snapshot and stops. When the environment
  is sufficient, it continues without a gratuitous reschedule.
- The handoff is a workflow checkpoint, not process migration. The recipe job
  ID, recipe snapshot, chapters, artifacts, and completed task outputs remain
  unchanged.

The provisioner reads initial metadata or the latest reschedule payload to
choose an environment; it never resolves or advances a recipe. The executor is
the final safety check against its injected *actual* allocation. JobDB's
`NextNeed` continues to route work capability, not resource sizes.

## Goals

- Allow every root recipe form (`op`, `sequence`, `state`, and `child_group`)
  to declare optional initial execution requirements.
- Make initial and currently published requirements visible in job listings,
  with explicit provenance and unresolved/unpublished states.
- Let list callers filter jobs by compatibility with a supplied execution
  environment, without making the filter a lease or readiness guarantee.
- Allow running recipe work to change one or more requirements without
  replacing the job.
- Preserve durable workflow progress across an environment handoff.
- Prevent an insufficient executor from starting requirement-dependent work,
  and prevent a stale or cancelled executor from committing later progress.
- Report the requested state separately from the allocation actually supplied
  to an attempt.
- Give unattended callers a stable outcome for environment handoff rather than
  treating it as an execution failure or an indefinite wait.
- Keep existing recipes and existing jobs runnable with deployment defaults.

## Non-goals

- Provisioning machines, selecting a cloud/provider/region/account, or placing
  jobs onto a particular cluster.
- Live process migration, memory snapshots, transfer of temporary files, or
  transfer of open connections.
- Automatically increasing memory after an OOM kill.
- Portable performance equivalence between CPUs.
- Exactly-once external side effects around a checkpoint.
- Container image construction or proving that a selected image contains the
  tools required by a recipe.
- General node affinity, credentials, networking, accelerators, or provider
  policy in the first version.

## Current Architecture and Gaps

The current tree already provides useful foundations:

- `recipe.RecipeMetadata` is embedded in every root recipe shape, so it is the
  common declaration point.
- embedded recipe YAML is stored as a job artifact by
  `starter.StartRecipeJobWithOptions`.
- deferred root resolution runs as the durable
  `recipe_root_source_resolve` task. Its output includes the resolved selector,
  commit, and expanded recipe YAML, so later replay does not need to read a
  moving source again.
- workflow task inputs and outputs are durable chapters, and replay skips
  completed task work.
- JobDB leases already fence chapter writes and provide rescheduling with an
  opaque `Payload`.
- targeted execution already distinguishes completed, failed, suspended, and
  not-leaseable states, including missing exact worker capabilities.
- job listings are paginated `jobdb.JobSummary` records and public
  `recipejob.RecipeJob` DTOs.

JobDB v0.0.13's `NextNeed` is an exact work capability, not an environment
description. Its `RescheduleExecutionRequest.Payload` is opaque JSON, visible
again through leases and job summaries. That is sufficient for a provisioner
to observe current demand without making JobDB a resource scheduler. One gap
remains: JobDB's workflow runner copies the existing payload for time and
dependency waits but replaces it when it reschedules to a task worker. A
namespaced c2j execution section must survive every reschedule path.

The other gap is knowing what was actually allocated. A request is not proof
that a container received those resources. c2j needs a trustworthy allocation
descriptor at process start and must check it after loading the recipe and
before executing work that relies on its requirements.

## Domain Model

### Requirement values

The version 1 portable model is:

```go
type ExecutionRequirements struct {
    Image     string
    Platform  *Platform
    Resources ResourceRequirements
}

type Platform struct {
    OS           string
    Architecture string
    Variant      string
}

type ResourceRequirements struct {
    CPU              *CPUQuantity
    Memory           *ByteQuantity
    EphemeralStorage *ByteQuantity
}
```

Pointers represent presence. An omitted value is not zero and does not prevent
a deployment from applying a default. Internally, CPU is compared as canonical
millicores and memory/storage as canonical bytes. Public recipe and JSON forms
use strings such as `500m`, `2`, `4Gi`, and `10Gi`.

The public envelope carries `schema_version: 1`. Readers must report an unknown
version as unsupported; they must not interpret it as absent requirements.

### Job execution state

c2j computes a current requirements snapshot, not a JobDB scheduling row:

```go
type ExecutionDemand struct {
    SchemaVersion    int
    RecipeResolution RecipeResolution // unresolved or resolved
    RecipeDigest     string           // pinned content identity when resolved
    RecipeBase       *ExecutionRequirements
    JobRequirements  ExecutionRequirementPatch
    Effective        ExecutionRequirements
    Revision         uint64
}
```

`RecipeBase == nil` with a resolved state means the resolved recipe omitted
requirements; it differs from an unresolved recipe. The field-wise rule is:

```text
effective.property = job_requirement.property ?? recipe_base.property
```

Thus an override of only `memory` retains the recipe's `cpu`, `image`,
`platform`, and storage values. Defaults are applied later by the provisioner;
they are not written into either layer. The pinned recipe snapshot makes the
base reproducible. Runtime changes affect only the job layer and increment the
logical revision. An incoming snapshot is rejected if its recipe digest
disagrees with the job's pinned recipe.

Initial metadata can contain job overrides and, when the submitter already has
the recipe, a validated base/effective snapshot for early provisioning. It is
immutable submission information, not proof that the recipe was loaded in an
execution attempt. After a requirements reschedule, a versioned c2j section
of the opaque payload contains the complete current snapshot. The provisioner
uses the latest published snapshot, preferring payload over initial metadata.
Partial patches are never the only durable value in the payload, because a
later unrelated reschedule replaces the entire opaque payload.

The listing view must distinguish unresolved, resolved-but-unspecified,
published, malformed, and unsupported versions. When a deferred recipe
resolves and the default environment is already sufficient, no requirements
reschedule is necessary. To retain the live-listing requirement, c2j needs a
separate read projection updated at resolution even without a handoff; this
projection is not used for JobDB lease matching.

### Allocated environment

At process start, the trusted provisioner injects the environment actually
allocated for that attempt:

```go
type AllocatedEnvironment struct {
    SchemaVersion int
    Image         *AllocatedImage
    Platform      *Platform
    Resources     AllocatedResources
}

type AllocatedImage struct {
    Reference      string // launch reference, often a mutable tag
    ManifestDigest string // resolved OCI manifest digest, when known
    ImageID        string // runtime's actual image/config ID, when known
}
```

The launch reference and actual identity are separate facts. A tag explains
what the provisioner asked the runtime to start; an image ID or digest
identifies what ran. The OCI manifest digest and a container runtime's image
ID may name different objects, so they must not be silently treated as
interchangeable. When available, record all three for diagnostics and replay.

Allocated resource fields are simultaneous usable capacities for c2j and
recipe processes, after provider overhead. Usable ephemeral storage excludes
image/provider overhead. If scratch is memory-backed, reported memory and
scratch must both be satisfiable at the same time. The provisioner reports
what it supplied, not merely what c2j requested.

Unknown allocation fields never prove compatibility with explicit
requirements. c2j may record allocation diagnostics with the attempt, but
JobDB does not use them to decide lease eligibility. A previous allocation is
never displayed as the current one.

## Author-facing Contract

### Recipe declaration

Add an optional field to `recipe.RecipeMetadata`. A recommended YAML shape is:

```yaml
execution:
  image: registry.example/recipe-runner:1.2
  platform: linux/amd64
  resources:
    cpu: "2"
    memory: 4Gi
    ephemeral-storage: 10Gi
```

The declaration is root recipe metadata, not `NodeMetadata`, so all four root
forms receive the same behavior without making arbitrary nodes implicit
environment boundaries.

All recipe-loading paths must call the same validation routine. This also
avoids the current difference where reader-based loading enables YAML known
field checks but byte-string loading uses `yaml.Unmarshal` directly.

### Suspend for new execution requirements

Do not add an `execution.require` recipe operation. Extend the existing
workflow suspension/reschedule model with an execution-requirements condition,
alongside suspension for a new worker need, time, or child jobs. Conceptually:

```go
ctx.Suspend(SuspendRequest{
    ExecutionRequirements: &ExecutionRequirementPatch{
        Memory:           ptr("16Gi"),
        EphemeralStorage: ptr("40Gi"),
    },
})
```

The exact API name may follow JobDB's existing await/reschedule terminology.
The important contract is that this is workflow control, not an output-producing
recipe node or activity.

The suspension carries a partial job-requirement patch:

- omitted fields preserve the prior job requirement, or continue falling back to
  the recipe base when no prior override exists;
- a supplied value replaces that job requirement, including a lower numeric
  minimum or a different image/platform;
- empty strings, zero, and negative quantities are invalid;
- explicit clearing is not supported in version 1.

When operation code discovers a new requirement, its durable activity output
can carry a continuation directive containing this suspension. The compiler
persists the activity output and artifacts first, observes the directive, and
suspends before exposing the output to dependent recipe work. On replay, the
cached activity is not rerun; the compiler reaches the same suspension and
continues once a later attempt validates its actual allocation.

A Go operation may use an `OpDependencies` helper to attach the directive to
its result. That helper records suspension intent; it does not mutate JobDB
from the middle of the activity. This ordering preserves discovered results
without presenting a special operation in the recipe language. A future
declarative recipe syntax should compile to the same suspension primitive
rather than register an activity.

An explicit runtime request creates a real suspension boundary even if the
current allocation could satisfy it: c2j publishes the new full snapshot in
the reschedule payload and releases the lease. A later attempt may use the
same allocation, but recipe code resumes only through normal replay. Initial
recipe preflight is different: it reschedules only when the starting
environment is insufficient.

### Inline recipe reuse

Inline expansion must not discard an included recipe's declaration. During
execution, the compiler reaches a suspension checkpoint immediately before the
included body and applies the included declaration as job requirements. This does
not create a child job, activity, or second executor. The checkpoint uses the
same replay and lease behavior as a runtime-discovered suspension.

The overrides remain part of the job state after the included recipe returns;
there is no implicit restoration. If later work needs a different image or a
lower resource set, it must suspend with another override. This keeps effective
requirements replayable and avoids hidden environment transitions at scope
exit.

Separately submitted child jobs do not inherit the parent's job requirements.
Their own recipe declaration becomes their base, and their job-requirement
layer starts empty.

## Validation and Canonicalization

Validation occurs when loading a recipe, before publishing a known recipe's
initial metadata, before accepting a suspension override patch, and when
reading an execution-demand payload. JobDB treats the c2j section as opaque.

- CPU uses Kubernetes-style cores/millicores, is positive, and has at most
  millicore precision. Equivalent values such as `1` and `1000m` compare
  equally.
- Memory and ephemeral storage use positive Kubernetes byte quantities with an
  explicit SI or IEC unit. Values must resolve to an integral byte count and
  fit the stored range.
- Platform is normalized to OCI OS/architecture/optional-variant components.
- Image is validated as an OCI/distribution reference. Digests are preferred;
  tags are allowed.

Use a well-tested quantity/reference parser rather than handwritten arithmetic,
then apply the narrower c2j rules above. Compare canonical scalar values in
executor preflight and return canonical strings in public DTOs. Preserve the
launch image reference, resolved manifest digest, and actual image ID as
separate attempt fields.

Image compatibility is true when a tag/reference requirement matches the
allocation's launch reference, or a digest requirement matches its resolved
manifest digest. An untyped image ID alone does not prove that an OCI manifest
digest requirement was met.
Platform must match all supplied components exactly after normalization.
Resource allocations must be greater than or equal to their minima. c2j makes
this comparison after it receives a lease and before dependent recipe work;
JobDB does not make it during leasing.

## JobDB and payload contract

No resource-aware JobDB lease API is required. `SubmitJob.Metadata` can carry
initial c2j data; `RescheduleExecutionRequest.Payload` already carries opaque
JSON and is persisted with the reschedule. `JobSummary` exposes both metadata
and the latest payload. JobDB continues to match `NextNeed`, waits,
prerequisites, cancellation, and lease ownership exactly as before. Resource
compatibility is checked by c2j *after* acquiring a lease and before dependent
work; the provisioner uses the published demand to select future allocations.
Human-facing or other external task completion is not gated by the current
resource demand; the next automated execution checks its allocation again.

### Initial submission and restart

Extend `starter.JobMetadata` with an optional, versioned execution section.
It may contain job-level overrides and, when the submitter already has the
pinned embedded recipe, a derived base/effective snapshot. The recipe remains
the base source of truth; the metadata copy is an early provisioning projection
identified by the recipe digest. For a deferred selector, the base is marked
unresolved and no provisioner is asked to resolve it. If metadata lacks enough
information to choose an environment, the provisioner launches its configured
default environment.

`starter.StartRecipeJobOptions` should expose optional job-level overrides for
non-CLI callers. An explicit restart seeds the new job's initial metadata with
the prior job's current snapshot by default, including accepted overrides; an
opt-out must be deliberate. Ordinary continuation does not create a restart.

### Reschedule payload

The existing opaque payload has two consumers: JobDB's workflow runner uses
fields such as `run_policy` and `task_wait`, while c2j and the provisioner use
a reserved `c2j.execution` section. That section carries the complete current
snapshot: version, pinned recipe identity, base, job overrides, effective
requirements, revision, and replay checkpoint. It is not merely the latest
patch. A provisioner prefers this snapshot to initial metadata when both are
present. Unrelated workflow fields and unknown namespaced fields are preserved.

JobDB need not parse the execution section. Its workflow runner does need a
generic payload-preservation guarantee across *every* reschedule and task
completion. Today time/dependency waits copy the payload, but handoff to a
task worker constructs a new one; that path must merge or carry forward opaque
namespaced data. Direct, SQLite, toy, and remote runtimes must preserve the
same bytes/meaning through the existing API. A future payload format change
must not silently drop the execution section.

The current `workflow.JobContext` exposes awaits and task calls but not its
lease payload or a general suspend operation. Give c2j a generic, lease-scoped
way to read the current payload and reschedule with a replacement payload;
the API shape is open. This is a workflow-control extension, not a JobDB
resource model.

### Lease-owned handoff and replay

For an insufficient initial environment or an explicit runtime change, c2j
calls the existing lease `Reschedule` with the full new payload and the
appropriate `NextNeed`/wait conditions. JobDB atomically persists that payload
and releases the lease. The old executor stops; it cannot commit later chapters
under the revoked lease. No resource-specific JobDB operation or requirements
receipt is introduced.

c2j must make its own suspension replay-safe. A deterministic checkpoint
identity accompanies the full snapshot and is tied to the durable activity
output or recipe boundary that requested it. On a later attempt, a checkpoint
already represented by the current or a newer published snapshot is passed
without reapplying an old override; reuse of the same identity with different
content is an error. The exact cursor/ledger representation is left to the
workflow implementation. If a reschedule response is lost, the old attempt
must stop and treat ownership as uncertain; a new attempt reads the accepted
payload before replaying dependent work.

Cancellation and lease fencing remain JobDB responsibilities. A cancelled job
must not be revived by a reschedule, and an expired lease cannot publish a
different payload. These are generic JobDB guarantees, not resource matching.

### Read contract

c2j derives its listing view from initial metadata, the latest opaque payload,
and a c2j-owned read projection for resolved requirements that did not cause a
handoff. Newer validated state takes precedence. The view labels initial
hints, current demand, and recipe resolution status. It labels an allocation
as current only while the corresponding lease is verifiably active; otherwise
it may show a separately named last-attempt allocation. Malformed or
unsupported data is reported, never treated as absent requirements. The
projection must support paginated/bulk
listing without loading recipe artifacts or replaying every chapter. Its
storage/API shape is open; JobDB need not use it for lease eligibility.

## c2j Execution Flows

### Known recipe at submission

1. c2j loads and validates the fully expanded recipe snapshot.
2. `StartRecipeJobWithOptions` can put the target root declaration, recipe
   digest, and any job-level overrides into submission metadata. This is a
   derived initial provisioning view, not a second authored declaration.
3. The provisioner uses that view if available; otherwise it starts the
   configured default environment.
4. After leasing, the executor loads the pinned recipe, computes the effective
   overlay, and compares it with its injected actual allocation. If adequate,
   it continues. If inadequate, it reschedules with the full snapshot and
   stops before dependent work.

Existing recipes with no declaration continue under deployment defaults.

### Deferred recipe resolution

Ordering is important:

1. submit the job with unresolved base information and any explicit job
   overrides in metadata; the provisioner starts its default environment;
2. the c2j execution invokes root source resolution as normal durable
   workflow work (which may itself use a task worker);
3. persist the resolved selector, commit, expanded YAML, and content hash as
   the existing durable task output;
4. load that pinned snapshot, compute the base plus overrides, and compare it
   with the injected actual allocation;
5. continue directly if compatible, or reschedule with a full current-demand
   payload and stop before `ExecuteRecipe` starts.

The snapshot must be durable before a handoff. Otherwise a replacement
executor could resolve a moving tag or branch to different recipe logic and
requirements. The provisioner does not load or advance the recipe at any point.
The default execution image must be able to run c2j and its recipe/source
resolution path; it need not contain the tools or capacity required by later
recipe operations.

On replay, the recorded resolution is reused and never resets later job
requirements.

### Runtime increase with handoff

| Point | Durable workflow state | Recipe base | Job requirement | Published demand | Attempt |
| --- | --- | --- | --- | --- | --- |
| Before discovery | manifest task is pending | `memory=2Gi` | none | initial metadata or none | small executor |
| Discovery complete | manifest output, artifacts, and suspension directive are cached | `memory=2Gi` | none | unchanged | small executor |
| Suspension committed | cached output is unchanged | `memory=2Gi` | `memory=16Gi` | full `memory=16Gi` snapshot in payload | yielded |
| Compatible retry | manifest task replays from its cached chapter | `memory=2Gi` | `memory=16Gi` | retained in payload | large executor |
| Suspension replay | checkpoint recognizes the published revision | `memory=2Gi` | `memory=16Gi` | retained in payload | large executor |
| Dependent work | compiler exposes the cached result to the next recipe node | `memory=2Gi` | `memory=16Gi` | retained in payload | large executor |

Even if the first allocation already had 16 GiB, this *explicit runtime*
suspension releases its lease. A later attempt may use that same allocation;
replay passes the checkpoint. The override remains in the full payload after
later time, task, or dependency reschedules. This differs from initial
preflight, which yields only when its allocation is insufficient.

### Checkpoint semantics

The preferred boundary is after an activity output has been durably recorded.
A lower-level immediate suspension invoked part-way through a custom task may
still cause that task to run again. In-memory values, temporary work
directories, open connections, and unrecorded side effects are not transferred.
Data needed after suspension must already be in normal durable task output,
artifacts, or external idempotent storage.

## c2j Package Changes

### Recipe and compiler

- Add declaration DTOs and validation near `pkg/recipe` (or in a small
  dependency-free `pkg/execution` package used by it).
- Add the optional declaration to `RecipeMetadata`; update generated recipe
  JSON Schema and YAML/JSON round-trip tests.
- Have `pkg/starter/start_recipe.go` emit optional initial execution metadata
  from a known embedded recipe and job-level overrides; mark a deferred base
  unresolved without resolving it during submission.
- In `pkg/worker/compiler/job_worker.go`, load the pinned recipe, overlay job
  requirements from metadata/current payload, and preflight the injected
  allocation before invoking `ExecuteRecipeWithExecutor`.
- Extend `ActivityInvocationOutput` with an optional continuation/suspension
  directive and let operation dependencies attach one to a durable result.
- Teach the compiler to process that directive before adding the activity
  result to the recipe resolution context.
- Preserve included recipe declarations in `inline_resolution.go` metadata and
  have the compiler suspend at the include boundary without creating a node.
- Extend the workflow context with a requirements suspension that writes a
  complete versioned execution-demand snapshot into the existing reschedule
  `Payload`. Preserve the snapshot through all later waits, task handoffs, and
  external task completions.
- Make the checkpoint replay-safe using a deterministic identity and published
  revision; never reapply an earlier patch over a newer payload.
- Extend the c2j JobDB chapter schema for the optional activity-output
  directive. The current-demand payload is distinct from chapter history.

### Submission and listing

- Add optional, versioned initial execution data to `starter.JobMetadata`.
  Keep authored job overrides distinct from a derived recipe-base hint.
- Add current published demand, initial demand, resolution/publication state,
  and attempt allocation fields to `recipejob.RecipeJob` and CLI list JSON.
  Keep the attempt's image reference, manifest digest, and image ID distinct.
- Derive the view from `jobdb.JobSummary.Payload`, submission metadata, and
  the c2j read projection, choosing the newest validated state. Do not treat
  absent data as an empty requirement set. Existing filters and page tokens
  remain unchanged.
- Always emit provenance and publication state in machine JSON. Human output
  may use a compact `REQUIRES` column, but JSON is the stable contract.
- Extend both `c2j list` and `c2j list children`, and their public recipe-job
  listing requests, with an optional execution-compatibility filter. The
  filter uses the same per-field allocation inputs as run commands. A list
  command applies it only with `--compatible-with-execution`; per-field flags
  without that switch are rejected rather than silently ignored. Inherited
  allocation environment variables alone must not silently hide jobs. The
  switch requires at least one compatibility-relevant allocation fact; an
  image ID by itself is only diagnostic.

The filter compares each job's current effective demand with the candidate
allocation: numeric minima must be no greater than supplied usable capacity;
image reference/digest and platform must be compatible. An omitted candidate
field is unknown and cannot satisfy an explicit job requirement. Unresolved,
malformed, or unsupported demand is not reported as compatible. Unresolved
jobs remain visible without the filter and can be included explicitly as
bootstrap candidates (for example with `--include-unresolved`); they are
still labeled unknown, not compatible. A filtered query that encounters
unsupported or malformed demand reports a typed per-item or query diagnostic
instead of silently treating it as compatible or absent. This filter
composes with status, repository, job type, child, and `NextNeed` filters, but
does not claim a lease or guarantee readiness. The executor still preflights
after acquisition.

`--execution-image-id` is retained in the candidate descriptor and listing
diagnostics, but it is not substituted for a recipe's required OCI manifest
digest. If exact search by an observed image ID is needed, expose a separate
allocation-observation filter rather than making compatibility semantics
ambiguous.

Filtering must occur before the *logical* page is returned. A naive filter of
one already-paginated JobDB page would produce short/misleading pages and
incorrect next-page behavior. The c2j read projection should support indexed
filtering where possible; an initial implementation may scan underlying pages
and return an opaque cursor that resumes after the last examined job.

### Executor allocation input

All execution entry points (`c2j run`, targeted `run one`, `run any`, and the
worker loop) assemble one immutable `AllocatedEnvironment` from per-field
arguments and environment variables at process start:

| CLI argument | Environment variable | Actual allocation fact |
| --- | --- | --- |
| `--execution-cpu` | `C2J_EXECUTION_CPU` | Usable CPU capacity, e.g. `2` or `500m` |
| `--execution-memory` | `C2J_EXECUTION_MEMORY` | Usable memory, e.g. `4Gi` |
| `--execution-ephemeral-storage` | `C2J_EXECUTION_EPHEMERAL_STORAGE` | Usable temporary workspace, e.g. `20Gi` |
| `--execution-platform` | `C2J_EXECUTION_PLATFORM` | Actual OCI OS/architecture/variant, e.g. `linux/amd64` |
| `--execution-image` | `C2J_EXECUTION_IMAGE` | Reference used to launch the image, often a tag |
| `--execution-image-digest` | `C2J_EXECUTION_IMAGE_DIGEST` | Resolved OCI manifest digest, when available |
| `--execution-image-id` | `C2J_EXECUTION_IMAGE_ID` | Runtime-reported image/config ID actually running |

Each explicitly supplied argument overrides only its corresponding
environment variable; an omitted field remains unknown. This mirrors c2j's
`--jobdb`/`C2J_JOBDB` precedence without treating project configuration as
proof of the current allocation. The typed options are shared by all run
commands and the list compatibility filter. Names are proposed CLI spelling;
the per-field semantics and source precedence are the contract.

The trusted provisioner supplies values from the allocation it actually
created, including overhead and image resolution. c2j parses and validates
the assembled descriptor once before recipe work. These are process-start
inputs, not author-controlled recipe environment variables, and recipe inputs
must not override them. They are allocation assertions, not security
attestations: c2j may cross-check cgroup limits where available, but does not
infer requested capacity from host totals. The provisioner must not advertise
memory and memory-backed scratch that cannot be used simultaneously.

The image reference and actual identity are independently optional. A tag
does not prove which content ran; a runtime image ID is not necessarily an OCI
manifest digest. If a recipe pins a digest, the actual manifest digest must
be supplied or otherwise verified. Unknown fields cannot satisfy explicit
requirements. A managed run with missing or malformed required allocation
facts reports configuration/incompatibility rather than assuming unlimited
resources. A local unmanaged runner may explicitly supply fields or detect
enforceable limits. The default environment receives the same per-field
inputs, so its first executor can compare actual capacity after loading the
recipe.

### Targeted-run outcomes

Introduce a typed service result and a stable machine-readable event. Suggested
outcomes are:

- `completed`
- `yielded` (ordinary future/child/task-capability suspension)
- `environment_required`
- `contended` (another live lease owns the job)
- `no_work` (untargeted poll only)
- `input_required`
- `failed`

`environment_required` includes the effective demand, its provenance and
revision, this attempt's allocation, and whether a handoff was just
published. It returns promptly and never prompts. A successful reschedule
exits without masquerading as a recipe failure; machine output distinguishes
handoff from completion. An executor that acquires a job with an already
published demand it cannot satisfy must release it and report the mismatch,
not repeatedly rewrite the same demand or spin in a polling loop.

c2j classifies this outcome from its preflight and reschedule result; JobDB's
`JobRunOutcome` need not understand resource requirements. `NextNeed` remains
a work-capability signal. Ordinary wait timeouts must not be applied to an
unchanged environment mismatch.

## Correctness and Failure Handling

### Required invariants

1. After resolving a recipe, c2j compares its effective requirements with the
   injected actual allocation before requirement-dependent work. JobDB may
   grant a lease to an incompatible executor; that executor must not proceed.
2. A default-environment attempt may resolve and pin the recipe, but if its
   allocation is insufficient it must publish the full demand and yield before
   dependent recipe work.
3. Only the current lease may reschedule. Persisting the new opaque payload
   and relinquishing that lease is one JobDB transition.
4. No chapter can be committed under a lease invalidated by a reschedule or
   cancellation.
5. The pinned recipe base is immutable. A later recipe-source change or replay
   cannot overwrite it or accepted job-level overrides.
6. Every reschedule and task completion preserves the current execution
   section unless a newer validated snapshot intentionally replaces it.
7. Replaying a suspension never creates a job, increments the logical revision
   repeatedly, or reapplies an older override.
8. Omitted patch fields and deployment defaults never erase explicit recipe
   base or job-level properties.

### Cancellation race

Requirement suspension is an ordinary JobDB reschedule. Cancellation and
reschedule serialize under existing lease rules. If cancellation wins, the
reschedule fails without publishing a new payload. If reschedule wins,
cancellation still prevents the replacement attempt from continuing. In both
cases the old executor is fenced.

### Unsupported and disallowed requirements

Syntactically invalid recipe declarations, allocation descriptors, payloads,
and suspension patches fail visibly in c2j. The provisioner may reject a
valid but unsupported image, platform, or resource minimum with a typed
outcome. It must not silently weaken the demand or launch a default
environment forever. c2j rechecks what was actually allocated on each
attempt; a provisioner's optimistic match is not sufficient.

## Defaults and Backward Compatibility

- Defaults are applied by the executor/provisioner after overlaying job
  requirements on the recipe base; they are not copied into either persisted
  layer.
- A job requirement wins over the recipe base, and the recipe base wins over a
  deployment default.
- Existing jobs without an execution section use legacy defaults and remain
  leaseable under existing JobDB rules. Missing initial metadata on a new
  deferred job means unresolved, not an empty recipe declaration.
- Existing recipes require no changes.
- Older list clients ignore additive c2j DTO fields. New clients distinguish
  legacy/unspecified from unresolved, unpublished, and unsupported.
- Because JobDB does not match resource/protocol versions, deployments must
  ensure pre-feature c2j workers cannot claim jobs that use this contract.
  Upgrade the worker fleet or use a distinct rollout capability; do not encode
  CPU/memory/image combinations into `NextNeed`.
- Image/platform changes do not alter the pinned recipe snapshot or durable
  history. Failure caused by tools missing from a new image is an ordinary
  execution failure, not permission to resolve different recipe code.

## Open questions

- The required read projection for a deferred recipe that resolves without a
  handoff could be a generic lease-fenced JobDB metadata update or a c2j-owned
  materialized view. Which path provides monotonic, crash-recoverable
  publication and efficient paginated listings without making JobDB interpret
  requirements?
- Should a targeted runner with an already-published incompatible demand
  reschedule the identical payload once to release its lease, or use a generic
  lease-release primitive if JobDB provides one? Either must return promptly
  and avoid a tight reacquisition loop.
- Which generic workflow-payload extension mechanism best preserves unknown
  namespaced fields across remote task handoffs and task completion? JobDB
  need not know the meaning of `c2j.execution`.

## Delivery Plan

### Phase 1: Allocation and payload foundation

- Define the versioned execution-demand model and per-field actual-allocation
  inputs, including distinct image reference, manifest digest, and image ID.
- Add the shared argument/environment parser to every run entry point.
- Ensure JobDB's workflow layer preserves opaque namespaced payload fields
  across every reschedule, task handoff, and task completion; add conformance
  tests for direct, SQLite, toy, and remote runtimes.
- Expose generic lease-scoped payload reading and suspension to c2j's job
  context; no resource-specific JobDB API is introduced.
- Gate rollout so pre-feature workers cannot claim constrained recipe jobs.

### Phase 2: Recipe preflight and visibility

- Add recipe syntax/validation/schema support and optional initial metadata.
- Check actual allocation after loading the pinned recipe. Continue if
  sufficient; otherwise publish full demand and yield the same job.
- Build the c2j read projection and derive listing/targeted-run outcomes from
  it, metadata, payload, and current attempt data, with explicit
  unresolved/unsupported states.
- Add opt-in compatibility filtering to `list`, `list children`, and the
  public recipe-job listing service without breaking pagination.
- Preserve current requirements on explicit restart.

Until phase 3 compiler support exists, reject a non-empty execution
declaration on an inline include target rather than silently discarding it.

### Phase 3: Runtime suspension

- Add the activity-output suspension directive and operation-dependency helper.
- Teach the compiler to suspend after caching an activity result and to pass
  the checkpoint on replay.
- Preserve inline recipe declarations and suspend at their compiler boundary.
- Add end-to-end crash, cancellation, replay, and replacement-executor tests.

## Test Plan

### c2j unit tests

- quantity equivalence, positivity, precision, suffix, overflow, platform, and
  image validation;
- YAML/JSON round trip for all four root recipe forms and recipes with no
  declaration;
- generated recipe schema coverage and unknown/unsupported versions;
- job-requirement overlay, partial merge, and no-clear semantics;
- per-field CLI-over-environment precedence, mixed sources, omitted/malformed
  values, trust boundaries, and actual-versus-requested capacity;
- image tag, manifest digest, and runtime image ID remain distinct; a runtime
  ID alone does not satisfy a pinned-manifest requirement;
- list compatibility uses numeric minima, image/platform compatibility,
  unknown-field exclusion, explicit unresolved-bootstrap inclusion, opt-in
  environment behavior, and correct pagination after filtering;
- payload snapshot merge/preservation and deterministic checkpoint replay;
- inline execution reaches exactly one deterministic suspension site before an
  included body;
- public and CLI list DTOs distinguish unresolved, unpublished, unspecified,
  specified, malformed retrieval, and unsupported versions.

### JobDB conformance tests

Run the same cases against direct, SQLite, toy, and remote runtimes:

- submission metadata and reschedule payload survive storage, remote
  transport, active/archive movement, and listing;
- unknown namespaced payload fields survive time waits, dependency waits,
  task-worker handoffs, and external task completion;
- rescheduling atomically publishes the complete payload and releases only the
  current lease; old-lease chapter writes fail;
- cancellation races do not resurrect work;
- `NextNeed` and existing readiness behavior are unchanged by the opaque
  execution section.

### c2j integration tests

- known declaration in metadata lets the provisioner choose an initial
  environment, but the executor still checks the actual allocation;
- run commands accept each allocation field by argument or environment, with
  per-field argument precedence; list and list-children compatibility filters
  use the same interpretation without filtering implicitly from inherited env;
- absent metadata causes default-environment startup; a compatible default
  continues without rescheduling;
- deferred resolution happens inside that executor, persists its pinned
  snapshot, then yields only when its allocation is insufficient;
- the provisioner never loads or advances the recipe;
- changing the source recipe after resolution does not change requirements or
  recipe logic on resume;
- a 2 GiB executor records durable manifest work, suspends for 16 GiB, and the
  same job resumes on a 16 GiB executor with prior artifacts/results;
- an already-large allocation crosses a fresh suspension attempt and the job
  requirement survives a later crash;
- a crash after caching the activity output but before suspension replays the
  directive without rerunning the activity or looping;
- a child job uses its own recipe base and does not inherit its parent's job
  requirements;
- explicit restart seeds the new job from the prior current demand rather
  than reverting to the original submission hint;
- missing managed allocation injection fails clearly; an unmanaged local run
  with unknown capacity never pretends to satisfy an explicit minimum;
- an incompatible executor with already-published demand releases promptly
  without repeatedly rewriting or busy-looping;
- machine outcomes distinguish completion, ordinary suspension, environment
  demand, contention/no work, input demand, and actual failure.

## Alternatives Rejected

### Make JobDB match execution resources

This would avoid incompatible leases but requires JobDB to understand resource
units, image/platform compatibility, and actual allocations. It is not needed
for safety if every c2j executor preflights before dependent work, and it would
not remove the need for the provisioner to choose an environment. It remains a
possible later scheduling optimization if incompatible lease churn is observed.

### Put all current demand in submission metadata

Metadata is useful for initial provisioning, but it is immutable and cannot
represent a recipe resolved later or a runtime override. A full current
snapshot in each reschedule payload carries those changes.

### Encode requirements in `NextNeed`

Exact capability strings cannot express numeric minimums or “more is okay.”
They also conflate work capability, including human-facing needs, with
environment compatibility.

### Have the provisioner resolve recipes before launching an executor

This moves recipe execution/pinning responsibility into the provisioner and
requires it to access recipe sources. Starting a default c2j executor when
metadata is insufficient keeps the provisioner a placement component; c2j
itself resolves the recipe and yields only if necessary.

### Store only a patch in the reschedule payload

The payload can be replaced by later reschedules, including task handoffs. A
patch alone cannot tell a provisioner the complete current demand or let a
replacement executor reconstruct it without replaying all prior history.

### Submit a replacement or restart job

That changes identity and complicates lineage, artifacts, cancellation, and
replay. A requirement transition is continuation of the current job and should
use the current lease as the fencing authority.

### Treat the host as having unlimited resources

This silently violates explicit requirements. Unknown capacity is compatible
with an unspecified field, not with a declared minimum.
