# Changing and testing safely

Start with the owning package and its contract before editing. Keep each rule in
one existing owner. Public promises belong in [LIBRARY.md](../LIBRARY.md),
[SERVER.md](../SERVER.md), [SPEC.md](../SPEC.md), or the focused model/auth guide.

## Test selection

Maintain **small unit tests only**. Test a function or local rule directly, using
in-memory inputs or small temporary files when file handling is the unit under
test. A fake model does not make a full agent run a unit test.

Do not add runtime/database fixtures, fake login servers, subscriptions waiting
for turns, subprocess/executable tests, browser automation, crash/restart tests,
or script integration suites. There is no separate slow or integration suite.
Do not introduce interfaces, mocks or test-only production settings merely to
recreate those scenarios under another name.

Tests must earn their place: target costly, plausible mistakes, not every feature
or permutation. Reuse existing checks. Deletion does not require a replacement.
Do not put benchmarks or timing assertions inside unit tests. Production
storage/durability and lifecycle contracts remain unchanged by this policy.

**Deliberate coverage limit:** unit tests do not establish end-to-end execution,
persistence across restart, process cleanup, browser flows or live provider
compatibility. Do not describe a passing unit suite as evidence for those flows.
Manual use or separately authorized investigation may reveal those regressions;
never use real credentials, paid inference or live services as an automatic
fallback validation step.

## Change recipes

### Tools and hooks

Read [`Tool`](../../tools.go), [`registerTools`](../../runtime.go), and the shared
call/settlement path in [`tools.go`](../../tools.go) and
[`execution_tools.go`](../../execution_tools.go). Add tools through `Config.Tools`
and reuse the executor; do not add a second dispatcher or HTTP code-upload route.
The file-tool validation and output-budget units are in
[`tools_test.go`](../../tools_test.go).

Hooks use the typed stages in [`hooks.go`](../../hooks.go),
[`hooks_dispatch.go`](../../hooks_dispatch.go), and
[`hooks_history.go`](../../hooks_history.go). Preserve append-only source-labelled
history and cancellation boundaries. The host supplies any approval UI. See the
[callback contract](../LIBRARY.md#lifecycle-hooks-and-host-extensions).

### Configuration and model management

Validation, cloning, defaults and pointer-based overrides live in
[`config.go`](../../config.go) and [`overrides.go`](../../overrides.go). Follow
resource preparation in [`config_prepared.go`](../../config_prepared.go) and
publication in [`reload.go`](../../reload.go). Preserve the distinction between
omitted values and explicit empty/false/zero/list overrides. Schema changes
belong in ordered [`migrations.go`](../../migrations.go); do not rewrite old
history to guess intent.

Discovery and provider mapping are owned by [`provider_target.go`](../../provider_target.go),
[`discovery.go`](../../discovery.go), [`discovery_metadata.go`](../../discovery_metadata.go),
and the Responses/Messages codecs. Keep provider-reported facts separate from
Pi references in [`model_reference.go`](../../model_reference.go) and
[MODEL-METADATA.md](../MODEL-METADATA.md). Parser units live in
[`discovery_metadata_test.go`](../../discovery_metadata_test.go).

For handoff or catalog changes, read [`model_switch.go`](../../model_switch.go),
[`cmd/catalog.go`](../../cmd/catalog.go), and
[MODEL-INSTALLATION.md](../MODEL-INSTALLATION.md). Reuse `UpdateModelsAndSave` for
model-only publication and `ReloadAndSave` for all-idle runtime changes. The
catalog tests exercise candidate construction directly, not running agents.

### Subagents, HTTP and browser clients

Read [SUBAGENTS.md](../SUBAGENTS.md). Reuse ordinary session controls, overrides
and history; do not add a second executor, notification inbox or coordination
system. Preserve child ownership, stable identities and the permanent one-level
role even after parent deletion.

HTTP handlers in [`server/server.go`](../../server/server.go) and specialized
route files delegate to the library. Reuse strict decoding, stable error mapping
and mutation request IDs, not handler-owned state. The JSON decoder has a direct
unit test in [`server/server_test.go`](../../server/server_test.go).

The local example's UI modules call `/v1`; [`example/http.go`](../../example/http.go)
owns browser-login bootstrap, model registration and acknowledged Resume.
Keep async state/errors scoped to the right session/account. The application
remains an example host, not a browser regression-test harness.

### Authentication

Read [AUTH-MODELS.md](../AUTH-MODELS.md). Coordination lives in
[`auth.go`](../../auth.go), persistence in [`auth_store.go`](../../auth_store.go),
OAuth/token handling in [`auth_openai.go`](../../auth_openai.go), and identity
validation in [`auth_oidc.go`](../../auth_oidc.go). Logout, receipts and transfers
remain in their existing owners. Keep credentials out of conversation storage,
errors and logs; preserve redirect/origin restrictions and uncertainty reporting.
Unit tests cover selected endpoint, error-classification and redaction rules;
they do not certify provider acceptance or account lifecycle behavior.

## Useful checks

From the repository root:

```sh
# Run tests even when earlier results are cached.
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
gofumpt -l .

# Check syntax without running installers or changing services.
bash -n scripts/install.sh scripts/run.sh
```

Build checks can target `./cmd` and `./example` using task-owned temporary output
paths. A successful build is not a runtime smoke test. Remove owned build/cache
artifacts afterward, but retain downloaded dependencies and toolchains.

## Local host and script boundaries

[`scripts/install.sh`](../../scripts/install.sh) and
[`scripts/run.sh`](../../scripts/run.sh) are operator helpers, not test fixtures
or service managers. Do not run them as validation. The installer builds the
binary and copies a config only if absent; the run helper may start a process or
change a named Tailscale Service route. Neither installs reboot supervision.

Named Services require explicit operator choices, appropriate host identity and
separate client access grants. Read the
[prerequisites and operator boundary](../SERVER.md#named-tailscale-services)
before editing these helpers. Unit tests and shell syntax checks do not validate
live Tailscale behavior, and do not authorize installation or route changes.
