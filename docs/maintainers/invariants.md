# Invariants to preserve

These are the boundaries that make a small persistent runtime safe to change.
Use the linked contracts for complete behavior and edge cases; these notes point
to the code that enforces them.

## Execution and durability

### Admission is a saved transition, not a background goroutine

- One session has one active execution/worker. Accepted mutations serialize on
  `Runtime.mu`; `session.go:mutateFresh` deduplicates the request and commits its
  acknowledgement with the state change. `control.go:begin` rejects new turns
  while the session has unfinished work. Different sessions can progress
  concurrently.
- `StartTurn`/`Steer`/`Interrupt` request contexts do not own accepted work.
  Commit first, then launch or signal the runtime-owned worker. If an admission
  write times out on SQLite contention, nothing was accepted (`storage_busy`);
  retry the same request ID. A later required write failure is fail-stop:
  stop admission rather than continue effects without records.
- Events are rows, not best-effort notifications. `appendEvent` writes inside
  the caller's transaction; subscribers only read committed rows. Replay is
  at-least-once, so consumers deduplicate by `(session_id, seq)`.

Enforcement: [`session.go`](../../session.go), [`store.go`](../../store.go),
[`control.go`](../../control.go), [`runtime.go`](../../runtime.go),
[`events.go`](../../events.go). Contract: [request IDs and mutations](../LIBRARY.md#mutations-and-request-ids),
[storage failure vs contention](../LIBRARY.md#storage-contention-versus-storage-failure).

### Cancellation has a commitment point; it cannot undo effects

`Interrupt` first commits a stop action and paused state under the runtime
transition lock, then cancels the worker. For a tool, `tools.go:runCall` checks
availability and hooks, then commits `started` plus `tool.started` before
invoking `Tool.Run`. It checks cancellation again before entering host code.
Cancellation before `Run` is a known `not_executed` result. Once `Run` starts,
file writes, shell commands, callbacks, and remote effects cannot be rolled
back.

- A recorded success or ordinary failure is a **known outcome**, even if
  cancellation races its persistence; keep and report it.
- Only an invoked call interrupted by shutdown/process loss without a saved
  result becomes **uncertain**. Preserve the warning and original call identity.
  Recovery skips unfinished replay-unsafe calls, even never-started ones, and
  saves truthful results through the normal outcome owner. The saved restriction
  or stricter current definition wins. `Resume` runs remaining replay-allowed
  calls through the normal batch executor, then the agent receives all paired
  results and can issue fresh calls. Saved outcomes never rerun.
- A host tool that returns identifiable cancellation can be uncertain on
  shutdown; a nil return is known success. Do not treat arbitrary cancellation
  or a planned-but-never-invoked call as proof that effects happened.
- Background process IDs are not durable ownership. Never adopt or signal a PID
  recovered from history; the OS may have reused it.

Enforcement: [`control.go`](../../control.go), [`tools.go`](../../tools.go),
[`execution_tools.go`](../../execution_tools.go),
[`execution.go`](../../execution.go), [`tool_bash.go`](../../tool_bash.go).

### Publish only a prepared and durable configuration

Global configuration replacement uses an all-sessions-idle reservation. Suspended executions, stopping workers, and immediate
follow-ups are busy too. `reload.go` validates and reads prompt files outside
runtime/SQLite locks while excluding new admission; on failure, the installed
view stays in place. It commits installed session settings before publishing
prepared maps/tools/prompts. Prompt-file contents are frozen until setup or an
explicit reload.

Per-chat `UpdateConfig` and `ReloadSession` share a detached prompt view with an empty
file cache, rechecks target idleness at publication, and commits only that chat's
settings/prompts/event/receipt. If a first turn binds during preparation, the
update cannot redirect or unpin that model/account. This path holds configuration
preparation ownership, not global
admission, during file I/O. Other sessions and the shared setup cache stay intact;
no worker rereads files. See `reload_session.go`.

The stock model installer has an extra host-file boundary. `cmd/catalog.go`
uses `UpdateModelsAndSave`: validate model definitions, atomically replace the
JSON file, then publish only `cfg.Models` under the transition lock. No chat idle
check or session/prompt write is involved. Workers capture just the main and
summary definitions at admission (`work_models.go`); lazy preparation must not
resolve against the installed model map. Do not replace the embedded
`preparedConfig` while workers run: unrelated config fields are read without
state locks. Other configuration preparation shares `configMu`; work, reads and
Stop can continue with the old catalog until publication. Once the save callback begins,
caller cancellation cannot abort publication. A save/publication failure seals
admission and closes the runtime because the file may already contain the new
config. External edits do not join this lock; detect and reject them rather than
silently merge.

Enforcement: [`reload.go`](../../reload.go), [`models_update.go`](../../models_update.go),
[`config_prepared.go`](../../config_prepared.go),
[`cmd/catalog.go`](../../cmd/catalog.go), [`cmd/reload.go`](../../cmd/reload.go).

### One-level subagents share session ownership

Children use the same executor/storage/auth path; never add a second job queue.
Creation and first-turn acceptance share a receipt. Completion notices use ordinary
steering with a `[notification]` prefix, not a new source-attribution system. Their
marker/receipt/queue write is atomic, and recovery never duplicates parent work.

`is_subagent` is a permanent role, independent of a live `parent_id` link. Child
prompts/configuration cannot enable the delegation tool, and StartSubagent rejects
child parents even after detachment/restart. Creation-time Config uses
SessionOverrides; a new
model resets inherited reasoning/reserve before applying explicit choices. Tools
may only be further restricted relative to the parent.

DeleteSession refuses unfinished child work (including suspension/settlement)
inside its transaction. Only the web app orchestrates stop, wait and retry. Allowed
parent deletion preserves child history and role. No file reservations, worktrees,
merge management or shared-workspace isolation are supplied.

Enforcement: [`subagents.go`](../../subagents.go),
[`subagent_completion.go`](../../subagent_completion.go),
[`config_prepared.go`](../../config_prepared.go), [`session.go`](../../session.go).

### Chat-list activity is written beside conversation records

`sessions.activity_at`/`tree_activity_at` are sort keys, not another history.
`store.go:touchActivity` runs in the same transaction as `appendHistory`,
`turn.go:startTurn` and `control.go:acceptSteering`; it also raises the direct
parent's tree time. Do not call it for renames, settings, pauses or reads, and
never move it backwards: keyset cursors in `session_list.go` rely on rows only
moving up. List queries must stay index-driven pages of `SessionSummary`, never
a `sessionView` per stored session.

Contract: [chat lists](../LIBRARY.md#chat-lists-and-activity).

## Configuration and model choices

### Model and account choices do not drift with defaults

The first accepted user prompt **or steering message** binds model alias,
provider, endpoint, upstream ID and effective API; it also pins the session's model/account
selection into overrides. A live turn pins its account generation across model
steps, tools, retries, and compaction. Editing shared defaults or removing an
override cannot change a used chat's selected account label, and redefining an
alias cannot silently redirect its binding. A label is not a permanently pinned
credential or human identity: explicit reauthorization under that label replaces
its login. A live turn cannot switch generations; later work/recovery resolves
the current login. Reauthorization alone does not create a portable-history handoff.

A used session changes model or account only through `Runtime.SwitchModel` at
the target session's fully-idle boundary (including saved executions and live
workers); unrelated sessions may keep working. It records a monotonic history
boundary and a visible handoff note without rewriting canonical history. Earlier tool calls,
results, and exposed reasoning summaries become labeled read-only assistant
text for future requests; encrypted/opaque and unknown provider items do not
cross the boundary. Switching back never revives old protocol payloads.

Enforcement: [`turn.go`](../../turn.go), [`config.go`](../../config.go),
[`config_prepared.go`](../../config_prepared.go),
[`model_switch.go`](../../model_switch.go), [`history.go`](../../history.go).

Contract: [model binding](../MODEL-INSTALLATION.md#missing-models-do-not-delete-chats)
and [handoff history](../MODEL-INSTALLATION.md#history-across-the-handoff).

### Catalog replacement is scoped and explicit

Discovery does not install aliases, choose a context budget, or start inference.
The stock host requires the latest in-memory snapshot for the exact provider and
account; installing it replaces only that account's managed model entries. An
omitted budget uses each model's provider-reported context capacity, or 128,000
when unknown; an explicit budget is a shared cap, still limited by reported
capacity.
Manual entries, other providers/accounts, unknown host config, and unrelated
model settings survive. Empty results require confirmation. Unavailable or
unverified entries are not made executable by guessing Responses support or
using advisory Pi metadata as provider facts.

Enforcement: [`discovery.go`](../../discovery.go),
[`model_reference.go`](../../model_reference.go),
[`cmd/catalog.go`](../../cmd/catalog.go), [`server/catalog.go`](../../server/catalog.go).

Contract: [metadata provenance](../MODEL-METADATA.md#provider-facts-versus-a-reference)
and [installation](../MODEL-INSTALLATION.md#browser-flow-and-http-contract).

## Authentication and provider protocols

### Credentials and refresh state have one owner

`AuthManager` owns a separate private SQLite database, protected directories
and files, and its own operation receipts. It is not the conversation database.
Credential changes and receipts commit before in-memory account handles are
published. Token values are private implementation state: do not add them to
session history, events, hooks, logs, normal API/status results, or error bodies.
Protected export bundles are still secrets; same-OS-user tools can read them,
and logical deletion is not forensic erasure.

A selected account reference includes its login generation. SIWC refresh is
coordinated per account; before a rotating refresh request, persist uncertainty.
Save a validated replacement before using it or resolving that marker. A known
definite token-endpoint rejection differs from a lost/ambiguous response or a
post-issuance validation error. Preserve that distinction through logout,
transfer, and reopen; never fall back to stale tokens. Logout commits local
unusability before remote revocation and reports unconfirmed revocation honestly.

Enforcement: [`auth_store.go`](../../auth_store.go),
[`auth.go`](../../auth.go), [`auth_receipts.go`](../../auth_receipts.go),
[`auth_accounts.go`](../../auth_accounts.go), [`auth_transfer.go`](../../auth_transfer.go),
[`migrations.go`](../../migrations.go).
Contract: [auth storage](../AUTH-MODELS.md#one-auth-store-separate-from-conversations),
[refresh uncertainty](../AUTH-MODELS.md#automatic-on-demand-refresh).

### A provider's auth mode is also a protocol boundary

Do not reduce provider support to a different URL or credential header:

- API-key providers and subscription accounts use the same executor (Messages is initially Copilot Opus 5.5 only),
  but subscription credentials are resolved through the matching account.
  A saved account never silently becomes API-key billing.
- SIWC is pinned to public `https://api.openai.com/v1` Responses. It does not
  use the old Codex backend, arbitrary proxy, Chat Completions, or explicit
  system-message items. New SIWC tool calls use the `sam` namespace and
  `strict:false`.
- Copilot is device-login based and sends its pinned compatibility headers only
  on Copilot requests. Its configured or token-derived API origin must be HTTPS
  `githubcopilot.com` or a subdomain; redirects are refused. The compatibility
  profile is observed integration evidence, not proof of live entitlement.
- Tool dispatch is authorized from the completed response that offered the
  tools. Persist the validated namespaced target or explicit unavailable
  decision with that response; recovery must not reinterpret old wire names
  using today's config.
- Copilot `X-Initiator` comes from canonical `user`/`steering` versus
  assistant/tool history, not the last wire role. Native completion notices use
  ordinary steering semantics; their text prefix is not an authorization boundary. BeforeRequest hook data uses a
  user-shaped protocol item but is explicitly application data, never human
  provenance or a protected-user anchor.

Enforcement: [`provider_target.go`](../../provider_target.go),
[`responses.go`](../../responses.go), [`turn.go`](../../turn.go),
[`hooks_request.go`](../../hooks_request.go),
[`hooks_history.go`](../../hooks_history.go),
[`auth_providers.go`](../../auth_providers.go),
[`auth_openai.go`](../../auth_openai.go),
[`auth_oidc.go`](../../auth_oidc.go).

Contract: [protocol boundaries](../AUTH-MODELS.md#inference-and-restart-safe-tool-identity).

## HTTP and host boundaries

The library owns agent behavior; `server` owns JSON validation, HTTP status
mapping, origin/Host checks, and SSE framing. CORS is not API authentication:
an allowed origin can control the full coding API. Origin allowlists are exact;
trust is checked against the actual local listener where available, and
`X-Forwarded-*` headers do not establish trust. Keep the executable on loopback
or a Tailscale address and rely on trusted SSH/Tailscale access controls.

Enforcement: [`server/server.go`](../../server/server.go),
[`server/browser.go`](../../server/browser.go),
[`server/stream.go`](../../server/stream.go),
[`cmd/main.go`](../../cmd/main.go).
Contract: [browser access and CORS](../SERVER.md#browser-access-and-cors).

The install/run scripts are host helpers, not service managers. They are
interactive by default. Installation builds a local binary and copies the
example config only if absent; it does not start the server. Optional Tailscale
Service setup needs a distinct explicit service name and admin-defined Service,
tagged host identity, HTTPS enabled, host approval as needed, and a separate
access grant. Helpers do not retag devices, create admin resources or grants,
use Funnel, edit app config/origins, configure reboot supervision, or supervise
sam-g after reboot. See [script boundaries](changing-and-testing.md#local-host-and-script-boundaries).
