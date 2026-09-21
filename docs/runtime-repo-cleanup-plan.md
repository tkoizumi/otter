# Runtime repository cleanup plan

## Objective

Make this repository the source checkout for people changing, testing, and
releasing the Otter runtime. People building integrations should install Otter
and work in their own project; they should not need this checkout, its Makefile,
or its Python connector libraries.

The final repository has no top-level `integrations/`, `examples/`, or vendor
library tree. Small integration programs remain where needed to test runtime
contracts, as package-local fixtures or files generated in temporary directories.

This is a repository-boundary change. Preserve the installed product's commands,
manifest format, SDK, configuration defaults, deployment behavior, and data
compatibility. In particular, keep `otter init`, `validate`, `prepare`, `release`,
`start`, and `deploy`. Supporting integration projects is part of the runtime;
hosting those projects in its source repository is not required.

The integration work is already preserved in Git. Preservation is complete and
is not a prerequisite or work item in this plan. Cleanup can proceed directly
in this repository; no destination setup or external-project validation is needed.

## Current findings

| Location | Current responsibility | Planned disposition |
| --- | --- | --- |
| `cmd/`, `internal/`, `migrations/` | Runtime, CLI, persistence, and tests | Keep. |
| `sdk/` | Embedded Python runtime SDK and its tests | Keep. |
| `integrations/shopify-to-salesforce/` | Customer sync application, configuration, tests, and probe tooling | Remove from this repository. |
| `integrations/shopify-product-to-salesforce-product/` | Product sync application, schemas, queries, and tests | Remove from this repository. |
| `lib/python/` | Vendor clients, sync helpers, schema tooling, and tests | Remove from this repository. |
| `graphql.config.yml` | Editor configuration for integration queries and schemas | Remove from this repository. |
| `Makefile` | Runtime development mixed with application release, schema, startup, and deployment wrappers | Reduce to runtime development and packaging. |
| `.github/workflows/test.yml`, `.github/workflows/release.yml` | Runtime tests mixed with connector and application suites | Retain runtime validation; remove application suites. |
| `README.md`, `docs/examples.md`, `sdk/python/README.md` | Runtime documentation mixed with integration tutorials and missing example links | Refocus and repair references. |
| `.nvimlog`, `run.log` | Tracked local artifacts | Remove from version control and ignore. |
| `find_runtime.sh` | Local runtime diagnostic helper | Review; retain under `scripts/` only if useful to contributors. |

There is no tracked `examples/` directory today. References to its counter and
customer-sync programs are stale, including references in CLI output and
architecture/operations documentation.

## 1. Remove application-owned source

Remove the tracked contents of both integration directories, all of
`lib/python/`, and `graphql.config.yml`. Remove empty parent directories where
applicable. Their application tests, vendor schemas, query files, lockfiles,
and probe tools leave with them.

Audit runtime dependencies before removal and retain any necessary runtime
coverage using the generic fixtures described below. Update Makefile and CI
references in the same implementation change so deletion does not leave broken
build or test commands.

No copying, archiving, destination repository setup, or integration-project
verification is required. Existing Git history provides the preserved work.
Leave ignored local environment files, runtime state, identity markers,
databases, virtual environments, and caches untouched. This cleanup does not
migrate running deployments or rewrite Git history.

## 2. Establish the runtime test boundary

Keep existing tests for scheduling, retries, execution, state, identity,
release layout, managed Python, deployment, and SDK behavior.

- Prefer the existing temporary-directory test patterns. Use package-local
  `testdata/` only where a checked-in fixture makes a test easier to understand.
- Fixtures must be small and deterministic, require no vendor credentials, and
  use local fake services if HTTP behavior is necessary.
- Audit whether any runtime coverage depends on the departing applications.
  Replace only that coverage with generic fixtures; do not port business mapping
  tests or create a duplicate example catalog.
- Keep tests for integrations importing sibling shared code. Removing the
  repository's `lib/python/` must not remove support for user-provided libraries
  or change release/deployment layout rules.
- Preserve the generic scaffold and its tests in `internal/cli/init.go` and
  `internal/cli/init_test.go`. The scaffold ships as product functionality.

Add one automated contributor smoke workflow, exposed as `make smoke`, that:

1. Builds and uses this checkout's binary by absolute path.
2. Creates a fresh temporary workspace outside the source checkout and runs
   `otter init` to generate a credential-free integration.
3. Validates and releases it, starts a runtime on a free loopback port, and
   submits a run.
4. Waits with a bounded timeout for success and checks the scaffold's persisted
   state through the CLI.
5. Stops only that workspace's runtime and cleans up its temporary files on
   success or failure, preserving useful failure output in CI.

The workflow must not read this checkout's local environment or connect to an
existing developer runtime. Reuse existing end-to-end coverage where practical.

## 3. Simplify contributor tooling

Update `Makefile`:

- Keep `help`, `build`, `test`, `test-go`, `test-python`, `lint`, `fmt`, `tidy`,
  `cross`, and packaging helpers; add the isolated `smoke` target.
- Make `test-python` run only the embedded runtime SDK suite.
- Remove integration-specific variables, test discovery, schema commands, and
  `release`, `release-all`, and `release-list` wrappers. Binary publishing stays
  in the existing release pipeline.
- Remove `start`, `start-detached`, `stop`, and `restart` wrappers that assume
  this checkout is an application workspace. Document manual debugging in an
  external temporary workspace instead.
- Remove application deployment and tunnel wrappers, including destructive
  deployment targets. Keep the actual `otter deploy` implementation and tests.
- Change `clean` to remove declared build artifacts only. It must not stop a
  runtime or delete `.otter/data`.
- Scope Python bytecode cleanup to the embedded SDK rather than walking every
  directory. Apply the same scope to the GoReleaser pre-build hook.
- Remove stale comments, unused variables, and duplicate `.PHONY` declarations.

Remove the tracked logs and add narrow ignore rules for them. Rewrite ignore
comments that imply credentials or application deployments belong in this
checkout. Retain protections for accidental environment files and runtime data.
Do not delete ignored local files during repository cleanup.

## 4. Refocus documentation and CLI guidance

Rewrite `README.md` around the runtime contributor:

1. What Otter does and what this repository owns.
2. A short install pointer for people who only want to use Otter, explicitly
   saying they do not need to clone this repository.
3. Contributor prerequisites, building, tests, linting, and the smoke workflow.
4. A correct source-tree map and links to architecture and runtime contracts.
5. Release and packaging responsibilities.

Add `CONTRIBUTING.md` with the supported development loop, subsystem locations,
fixture conventions, external-workspace debugging, and the policy that vendor
clients, mappings, and real integrations belong in separate projects. Derive
tool prerequisites from `go.mod` and CI rather than guessing versions.

For existing documentation:

- Retain API, manifest, SDK, identity, managed Python, security, operations, and
  deployment references as versioned product contracts. Their commands should
  work with installed binaries in a user's own workspace.
- Remove long integration-authoring tutorials and `docs/examples.md` from this
  repository. Retain only concise explanations needed for runtime contracts;
  no tutorial migration or separate documentation project is required.
- Remove the bundled-example walkthroughs and missing-file links. Use short,
  generic snippets where they clarify runtime behavior.
- Update `docs/deploy.md` and README deployment guidance to use the installed
  CLI rather than repository Makefile wrappers.
- Update `sdk/python/README.md`, `docs/architecture.md`, `docs/operations.md`,
  and other affected references to remove reliance on absent examples.
- Fix CLI help/output in `internal/cli/cli.go` that points at `./examples` or
  uses a specific bundled Shopify integration as the assumed starting point.
  Update affected assertions if output is tested.
- Review historical implementation plans and label them as historical where
  needed; avoid treating them as current contributor instructions.

Do not globally remove the word `integrations` or every `lib/python` path.
API routes, configuration defaults, and illustrative user-workspace layouts
remain valid. Audit references by meaning, not just by matching their spelling.

## 5. Align CI and release packaging

- Remove connector and application suites from both GitHub workflows in the
  same change that removes their source directories.
- Keep Go tests, SDK tests, static checks, and the existing platform build and
  release-configuration checks. Run the isolated smoke workflow in PR validation
  and before tagged releases publish.
- Ensure both workflows install the Python interpreter required by runtime
  tests and the smoke workflow.
- Make the existing formatting check fail when `gofmt` reports files; printing
  filenames alone does not enforce formatting.
- Check a non-publishing GoReleaser snapshot: retain both binaries, existing
  archive names, checksums, platforms, and the embedded SDK. No vendor
  code, integration configuration, or local artifacts should be packaged.

No release tag, publishing action, Homebrew update, remote deployment, or
unrelated dependency upgrade is needed to implement this plan.

## Delivery sequence

1. **Test isolation:** audit runtime coverage and add or reuse the isolated
   runtime smoke workflow and contributor instructions.
2. **Repository boundary:** remove application source, libraries, and editor
   configuration; simplify Makefile and both CI workflows together so the
   repository remains buildable at the commit boundary.
3. **Documentation and hygiene:** refocus README, finish reference repairs and CLI
   guidance, remove tracked logs, and verify packaging.

Each implementation PR should describe the removed repository responsibilities,
any contributor command changes, and validation results. Runtime databases
require no migration for this change.

## Acceptance criteria

- A fresh clone contains no top-level integration applications, example catalog,
  vendor clients, or vendor schema tooling.
- `make help` describes runtime development and packaging only.
- `make build`, `make test`, `make lint`, and `make smoke` pass from a fresh clone
  with documented prerequisites and no SaaS credentials or local configuration.
- Runtime tests still cover shared-library layouts and existing public commands.
- The smoke workflow leaves no daemon behind and does not create an application
  workspace or persistent runtime state in the source checkout.
- Documentation links resolve; no current instruction depends on missing
  examples, removed Makefile targets, or the removed application source tree.
- Non-publishing release checks pass and retain the existing binary interfaces.
- The final diff contains no unintended changes to ignored local operational
  files, public API behavior, identity semantics, or database migrations.

Run a final tracked-file and reference audit as well as the tests. Record
pre-existing failures separately rather than claiming they were caused or fixed
by this cleanup.
