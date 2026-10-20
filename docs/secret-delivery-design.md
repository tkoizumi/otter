# Secret delivery: workload identity and the host broker

Status: **design**, partially implemented. Date: 2026-10-15.

`internal/secrets/workload.go` and `internal/secrets/broker.go` implement the
client half. The broker, the Secrets Manager integration and the cloud
write-through are not built yet.

Governing decisions: **D2** (secrets are delivered at execution time, never
embedded in artifacts), **D4** (values are entered in Otter Cloud and written
through, never held), **CA-15** (backend is AWS Secrets Manager) and **CL-13**
(amended for the write-through input path). See
[decisions.md](../../otter-platform/docs/decisions.md) and
[cloud-alpha-readiness.md](cloud-alpha-readiness.md).

## The problem this solves

A pooled tenant needs its job's secrets. Three constraints collide:

1. A release artifact must contain **zero secrets** (`D2`).
2. The control plane must never hold, display, or proxy a client credential
   (`CL-13`), and at execution time must not be in the path at all.
3. A compromised tenant must not be able to obtain a sibling's secrets **on the
   same host** (`D2`'s audience requirement).

The existing `secrets.Provider` seam is what makes this a swap rather than a
rewrite: the executor already asks a provider for names, and the manifest and
release already carry **names only**.

## The three parties

| Party | Holds | Never holds |
| --- | --- | --- |
| Tenant `otterd` | Its **broker credential**, in its environment | AWS credentials; any other runtime's credential |
| Host broker | `runtime_id → broker credential`, and the AWS credential (instance role) | — |
| Otter Cloud control plane | Secret **names**, the job→runtime assignment, and (transiently, on write-through) a value being entered | Any stored or returned value; the broker credential |

**The broker credential is host-local and is *not* the control-plane bootstrap
secret.** This matters: the control plane stores only `sha256(bootstrap_secret)`
for a runtime, so if the broker verified tokens with that secret the control
plane would still not hold a usable key — but if it were ever handed the
plaintext to make verification convenient, the control plane could mint a token
and read every secret. Keeping a separate, host-local credential means the
control plane is *cryptographically unable* to read a secret, which is what
`CL-13`'s done-when asks for.

## Namespace

Secrets live in AWS Secrets Manager at:

```
otter/{runtime_id}/_tenant/{NAME}    the tenant's shared values
otter/{runtime_id}/{job_id}/{NAME}   one job's own, which override them
```

The **runtime id is the prefix**, so cross-tenant access is impossible by
construction rather than by a check that could be forgotten: an authenticated
runtime can only ever address its own subtree. The job id is the second segment,
which gives `D2`'s job binding.

**Two tiers, and why.** A runtime's shared credentials live once under `_tenant`,
and a job's own namespace overrides any name it declares. Before this, every job
held its own copy, so rotating one credential across forty jobs was forty writes,
and every job id had to be known and named. The job segment buys job-to-job
isolation, which for one client's own jobs is a weak boundary; the tenant boundary
is the one that matters, and it is unchanged — the tenant segment derives from the
token's runtime id, never from the request.

The trade is that every job on a tenant can read the tenant's shared values, so a
compromised job has a wider read than before. That is bounded by the tenant, and
accepted, because the alternative — fanning a write out per job — leaves copies
that an out-of-band rotation diverges silently.

`_tenant` is reserved rather than merely conventional: `validSegment` refuses a
leading underscore for every identity segment, so no job can be named into the
shared tier and be served it as its own.

**Why per-name and not one document per job.** A document would make setting a
single name a read-modify-write, so the control plane's write path would need
`secretsmanager:GetSecretValue` — the ability to read back every secret it is
trusted to write. Per-name secrets let the control plane hold a **write-only**
backend credential: `PutSecretValue` granted, `GetSecretValue` denied. The
control plane then cannot read a client secret *even if its code is changed to
try*, which is what makes `CL-13`'s done-when — "a designed property rather than
an accident" — true rather than aspirational. It also removes the lost-update
race two concurrent single-name writes would otherwise have.

The cost is one lookup per name instead of one per job. The broker absorbs it:
`secretbroker.Store` still returns a job's whole document, and the Secrets
Manager adapter assembles it by listing the job's prefix and batch-getting what
it finds (the API takes 20 names per call, comfortably above a job's declared
set). The seam does not change; only the adapter's shape does.

## The exchange

The runtime mints a short-lived token and presents it to the host broker.

```http
POST /v1/secrets/exchange
Content-Type: application/json

{"runtime_id": "rt_abc", "token": "<payload>.<mac>"}
```

```json
200 OK
{"secrets": {"SHOPIFY_CLIENT_ID": "…", "CLICKHOUSE_USER": "…"}}
```

| Status | Meaning | Runtime behaviour |
| --- | --- | --- |
| `200` | The job's document | Serve the declared names from it |
| `401` | Token failed to verify | Fail the run loudly |
| `403` | Audience mismatch, or the job is not this runtime's | Fail the run loudly |
| `5xx` | Broker or backend unavailable | Fail the run loudly |

A refused exchange is deliberately **not** `ErrNotFound`. A run that cannot
prove who it is must not look like a job that forgot to declare a variable.

### The broker's checks, in order

1. Look up the broker credential by the request's `runtime_id`.
2. Verify the token with it — signature, then expiry.
3. Require `token.runtime_id == request.runtime_id`. Neither field is trusted
   alone: the claim selects the key, the token proves possession of it.
4. Require the token's `job_id` to be a job of that runtime.
5. Read `otter/{runtime_id}/{job_id}` and return it.

## The workload token

`v1` format: `base64url(payload) + "." + base64url(HMAC-SHA256(payload))`, keyed
by the broker credential.

```json
{"v": 1, "rt": "rt_abc", "job": "shopify-orders-to-clickhouse", "exp": 1760000000}
```

- **Both ids are inside the signature**, so a token minted for one job cannot be
  widened to another by editing the payload.
- **Expiry is exclusive**: a token is rejected at exactly `exp`.
- **Version is signed**, so an older format cannot be replayed at a newer
  verifier.
- **Comparison is constant time**, so the MAC cannot be recovered byte by byte.
- Default TTL is 2 minutes — long enough that one run's resolution needs a
  single exchange, short enough that a leaked token is not a standing credential.

## Per-run semantics, and rotation

`BrokerProvider` is built **per run** and fetches the job's document once,
caching it for that run. Two consequences, both asserted in tests:

- **Rotation without a release** (`D2`): a value rotated in Secrets Manager is
  picked up by the next run, because the next run builds a new provider. No
  release, no redeploy, no daemon restart.
- **A stable run**: a run does not observe a value changing underneath it
  halfway through, because it resolves once.

A provider cached across runs would silently pin old values. Any wiring must
construct it at the run site —
[`workers.go:260`](../internal/daemon/workers.go) is where secrets are resolved
today, and it currently passes the daemon-wide provider, which cannot know which
job is running.

## What is implemented today

- `MintWorkloadToken` / `VerifyWorkloadToken`, with audience, expiry, tamper and
  sibling-runtime rejection.
- `BrokerProvider`, with one exchange per document, per-run caching, blank-value
  and missing-key handling, refusal handling, and rotation across runs.
- **Per-run wiring.** `Daemon.secretsForRun` builds a `BrokerProvider` bound to
  `(runtime_id, run.JobID)` for each run, and falls back to the daemon-wide
  provider when no broker is configured, so an ordinary deployment is unchanged.
- **Configuration.** `OTTER_SECRET_BROKER_URL`, `OTTER_RUNTIME_ID` and
  `OTTER_BROKER_CREDENTIAL`, with startup refusal when the broker is
  half-configured — and the credential is never echoed in an error.
- **The host broker** (`internal/secretbroker`): the exchange handler, the
  single namespace builder, refusal semantics, and in-memory
  `Credentials`/`Store` for the host and for tests.
- **The cloud write-through logic**
  (`otter-platform/cloud/lib/secrets/write-through.ts`): name validation against
  the job's declared set, the write-only namespace, and the
  audit-without-disclosure record. The invariants are asserted by
  `otter-platform/cloud/tests/secrets-write-through.test.mjs`, including a scan
  of everything the module returns and audits for the value.
- **The rotation drill** (`scripts/drill/secret-rotation.sh`), extended rather
  than replaced. Its header had said that when hot rotation landed, "step 2 must
  be inverted deliberately rather than quietly passing" — so it now asserts BOTH
  truths: the environment provider applies a rotation by restart, and a broker
  applies it to the next run with no restart and no reload. Each has a sabotage
  mode (`reload-applies`, `stale-broker`) so either assertion can be shown to go
  red, and `operations.md` was corrected where it stated the restart rule
  unconditionally.
- **The dashboard panel** (`app/app/jobs/[jobId]/secrets-panel.tsx`), rendered in
  the job page's settings tab. It offers a field per declared name and a Save or
  Rotate button, clears the field on success, and shows only the name and when it
  was last set. There is no reveal control — not because one was left out, but
  because nothing behind it can return a value.
- **What the panel may claim about a secret's state** is its own invariant, in
  `lib/secrets/panel-state.ts`. It may say "saved now" for a value saved in this
  browser session and nothing else: because no value can be read back, the control
  plane cannot know whether one exists, so an empty field may not be labelled "not
  set". It said exactly that until this was caught — the ordinary case, a secret
  configured long ago or by someone else, was rendered as the one state the
  backend is built never to be able to observe. The field now says it is
  write-only, and the panel points at the diagnostic that does exist: a run that
  fails for a missing secret names it.
- **The route a value enters through**
  (`app/api/jobs/[jobId]/secrets/[name]/route.ts`) and its binding
  (`lib/secrets/writer-access.ts`). The route is **session-only** — a `deploy`
  token must not be able to set a client's credentials — validates the name
  against the job's declared set, and returns `{ name, updatedAt }` with nothing
  derived from the value. The binding treats a partially configured store as
  ABSENT rather than building a writer with an empty key, and its 503 names the
  missing variables without echoing any credential that was supplied.
- **The write-only AWS binding and the shared SigV4 signer**
  (`otter-platform/cloud/lib/secrets/aws-writer.ts` and `lib/aws/sigv4.ts`). The
  writer calls `PutSecretValue` and `CreateSecret` and never a read; because
  Secrets Manager has no upsert, it puts first and creates only when the backend
  says the secret is missing. The canonicalisation moved into `lib/aws/sigv4.ts`
  so the agent's bootstrap **verifier** and this **signer** cannot disagree about
  which bytes are covered — the cross-language conformance vector the Go suite
  emits did not change, which is what proves the move was behaviour-preserving.
- **The shared SigV4 signer** (`internal/awsv4`), extracted from
  `internal/agent` so the broker's AWS calls and the agent's bootstrap assertion
  sign with one implementation. The extraction is behaviour-preserving: the
  cross-language conformance vector the cloud suite verifies did not change.
- **The Secrets Manager read client** (`internal/awssecrets`): JSON-API calls
  signed with `internal/awsv4`, paginated `ListSecrets`, chunked
  `BatchGetSecretValue`, and `DocumentStore`, which assembles a job's document
  from the per-name secrets under its namespace. Tested against a fake endpoint
  that enforces the headers the real service requires.
- **The credential source** (`internal/awscreds`): IMDSv2 retrieval and a
  caching `Source` that refreshes before expiry and falls back to a still-valid
  credential when a refresh fails. The agent's bootstrap now reads through it
  too, so there is one IMDS implementation rather than two.
- **The host's serving pieces and the command** (`internal/secretbroker` and
  `otter pool broker`): a `0600`-enforced, self-reloading credential file, the
  tenant-registry parser that yields the gateway addresses to bind, a
  multi-listener `Serve` that shuts down cleanly, and the command that wires the
  registry, the credentials, the instance role and the Secrets Manager client
  together.
- **The credential, minted and delivered** (`secretbroker.EnsureCredentialFile`
  and `pool.Execute`): the agent mints one credential per runtime, records it in
  the file the broker reads, and hands the same value to the provisioner, which
  passes it to the container through an **env file** rather than `-e` so it is
  never on a docker command line. Minting is idempotent, so a re-provision cannot
  leave the tenant and the broker disagreeing, and a dry run mints nothing.
- **`CA-13`'s rotation property, asserted end to end**
  (`internal/secretbroker/rotation_test.go`): a real runtime client resolves
  through the real broker handler and the real Secrets Manager client against a
  fake backend; the value is rotated; the **next** run resolves the new value
  while a run already in flight keeps the one it resolved. The broker is never
  restarted, and no release is involved at any point. The test also asserts the
  backend was genuinely re-read, so a provider that cached across runs could not
  pass it by accident.
- Tests: `internal/secrets/{workload,broker}_test.go`,
  `internal/secretbroker/broker_test.go` (including the real runtime client
  against the real handler over a socket), `internal/daemon/secrets_broker_test.go`
  (a real run resolving through a fake broker and asserting the identity the
  broker verified), `internal/awsv4`, `internal/awscreds`, `internal/awssecrets`,
  and the config tests.

Three defects were found while doing this, each by a test that did not exist
before it:


- `AWSInstanceRole.Identity` re-wrapped every credential-fetch failure as
  `ErrBootstrapUnavailable`, discarding the refusal the rest of the package
  classifies correctly. A host with no role attached would have been retried
  forever instead of reported as the configuration fault it is. The wrap now
  preserves the classification. It is fixed; the agent had no test for this
  path at all until `internal/agent/aws_test.go` was added.
- `provision-tenant.sh` passed `OTTER_SECRET_BROKER_URL` to the container
  **unconditionally**, but `otterd` refuses to start when that URL is set without
  `OTTER_BROKER_CREDENTIAL`. A tenant provisioned without a broker — which is what
  the hand-provisioning runbook does — would have failed to come up, and nothing
  would have said why until somebody read the container log. Both variables are
  now gated on the credential, and `hosting/pool/tests/launch-contract.test.sh`
  asserts the no-credential case; reverting the fix turns two assertions red.

## Not implemented

- **Applying the write-only IAM policy.** It is written down, with the exact
  `aws iam` commands and a two-command probe that proves each half
  ([`otter-platform/docs/secrets-writer-iam.md`](../../otter-platform/docs/secrets-writer-iam.md)),
  but it is not attached to any deployment. Until it is, "the control plane
  cannot read a client secret" is a promise the code keeps rather than a property
  the account enforces.
- **A proof against the real AWS path and a real pooled container.** The
  rotation property is now proven twice — at the seam by
  `internal/secretbroker/rotation_test.go`, and against a **real daemon** by
  `scripts/drill/secret-rotation.sh`, which stands up the broker and a loopback
  IMDS + Secrets Manager and asserts that a rotation reaches the next run with no
  restart. What neither covers is the real IAM policy, the real network path, and
  a tenant inside a gVisor container rather than on the host's loopback. Those
  still need a host, and they are what a reviewer should still ask for.
- **Whether `--env-file` should also carry the bootstrap secret.** The broker
  credential avoids the docker command line; `OTTER_AGENT_BOOTSTRAP_SECRET`
  still travels as `-e`, which is a pre-existing exposure. It is recorded here so
  it is a decision rather than an oversight.

## The control plane's own AWS credential

The write-through runs in a Cloudflare Worker, which cannot assume an EC2
instance role, so it holds an IAM access key as a Worker secret. That is worth
stating plainly, because it is the one place this design asks for a long-lived
credential:

- **Write-only limits the damage, but does not remove it.** Denying
  `GetSecretValue` means a compromised Worker cannot read a client secret. It can
  still *overwrite* one, which is an integrity and availability problem rather
  than a disclosure one — a job would start failing, or run with a wrong
  credential, and nothing would say why.
- **The policy should therefore be conditioned on the namespace**, so the key can
  only touch `otter/*`, and scoped to `PutSecretValue` and `CreateSecret` on that
  prefix. A key that can write anywhere in the account is a much larger thing
  than this design needs.
- **A session credential would be better.** `AssumeRole` with a short expiry and
  a session policy narrows the window, at the cost of a role the Worker can
  assume. That is the upgrade path if the key's lifetime becomes the concern;
  it is not required for the alpha, but it should be a recorded decision rather
  than an omission.

## Running the broker

`otter pool broker` wires the pieces: the tenant registry gives it the gateway
addresses to bind, the credentials file gives it one credential per runtime, and
`internal/awscreds` + `internal/awssecrets` give it the read of Secrets Manager
under the host's instance role. The permission that role needs -- exactly two
actions, not a blank cheque -- is written down with the commands to attach it and a
probe in `otter-platform/docs/secrets-broker-iam.md`, and `internal/secretbroker`
pins the action set so a call added later cannot silently need a policy nobody
attached.

On a pooled host it runs as `hosting/pool/otter-broker.service`, which passes **no
flags** -- the registry and credentials paths are already this command's defaults,
so a flag in the unit would be a second place for the same fact to drift. It runs
as root because the credentials file is `0600` and root-owned, and it is a path
rather than a `LoadCredential` snapshot because it is re-read on change (see
below). The unit is pinned by `hosting/pool/tests/broker-unit.test.sh`.

Two behaviours worth knowing:

- **It listens on the tenant gateways, never a wildcard.** A wildcard bind on an
  EC2 host would also expose the broker on the public interface, where the only
  thing protecting it would be a security group somebody has to remember to
  check. With no gateways and no explicit `--listen`, it refuses to start rather
  than falling back to every interface.
- **The credentials file is re-read when it changes**, so a tenant provisioned
  after the broker started does not need the broker restarted. The file must be
  `0600`: it holds a credential for every tenant on the host, so a
  group-readable copy is every tenant's secrets one `cat` away. That is enforced
  before the contents are parsed, and no value from it is ever logged.

## Reaching the broker from a tenant

A tenant container gets an ordinary address on its own `100.64.<index>.0/24`
network with the host at `100.64.<index>.1`. The rendered egress rules
(`hosting/resolver/render-tenant-egress.sh`) deny `169.254/16`, `10/8`,
`172.16/12`, `192.168/16` and `127/8` — but **not `100.64.0.0/10`**, which is the
space the pool's own tenants occupy. Two consequences:

- Tenant → its own gateway is not blocked by those rules, so a host-side broker
  is reachable without an egress change. (Strictly that traffic is destined for
  the host itself, so it is INPUT rather than FORWARD, and those rules never
  applied to it.)
- Sibling-subnet traffic is not *explicitly* denied either, so isolation between
  tenants rests on Docker's bridge isolation rather than on the rendered rules.
  That is a check to run by connect, not by reading the rules — see the backlog
  entry in `otter-platform/docs/hosted-experience-tasks.md`.

## Acceptance

Status of each, so a reviewer can see what is proven and what is still claimed:

| Acceptance | State |
| --- | --- |
| A job resolves its declared secrets with no release change | **Proven** at the seam, and against a real daemon by `scripts/drill/secret-rotation.sh` |
| A token minted for runtime A is refused by a broker holding only runtime B's key | **Proven** — the real client against the real handler |
| Rotating a value changes what the **next** run resolves, with no release and no restart | **Proven** twice: across the client/broker/backend and against a real daemon. Both sabotage modes turn the drill red |
| A run keeps the value it resolved when a rotation lands mid-run | **Proven** |
| A tenant **container** reaches a broker on its own gateway and runs on the secret it returns | **Proven** by `hosting/pool/tests/broker-boundary.test.sh`: a real tenant container on a tenant network, handed its credential by env file, whose exchange the broker verified by HMAC against that credential, and whose job then ran on the value. The same test runs the path again with a credential the broker does not know, and asserts the refusal and the failed run — so the first half cannot be asserting nothing |
| The control plane cannot read a value, from its store, its logs or any API response | **Asserted, not yet enforced.** Nothing reads a value: `tests/secrets-aws-writer.test.mjs` drives both write paths and fails on a read, and the value never leaves the request body. But the design's claim is stronger than "the code does not read" — it is that reading is *impossible even if the code is changed*, and that rests entirely on the policy. `tests/secrets-writer-policy.test.mjs` now pins the policy to the code in both directions: the `Allow` set must be exactly the actions the writer presents (an action the policy omits is AccessDenied on the first Save in production with every other test green; an action it grants that nothing uses is a permission nobody needs, held by the credential that can overwrite client secrets), the write must stay scoped to `otter/*`, and every way to read — single-value, batch, and both list calls — must stay in the explicit `Deny`. **What remains is application**: nothing has attached the policy to an account, which is the deploy step in `otter-platform/docs/secrets-writer-iam.md` |
