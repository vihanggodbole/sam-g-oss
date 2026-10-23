# Architecture and flows

`sam-g` is a reusable Go runtime with optional hosts. The dependency direction is
inward: `cmd` and `server` call the root `sam` package; the library opens no
listener and installs no process signal handlers. A host owns runtime lifetime.
The [design guide](../DESIGN.md) explains why these boundaries are small; this
page is a navigation map, not a type reference.

## Ownership map

- **Admission and execution:** [`control.go`](../../control.go) owns public
  turn/steer/interrupt mutations; [`execution.go`](../../execution.go) dispatches
  the saved action; [`execution_store.go`](../../execution_store.go) stores it.
  Start at `Runtime.StartTurn`, `Runtime.Interrupt`, and `executeSession`.
- **Durable records:** [`session.go`](../../session.go) owns session operations;
  [`store.go`](../../store.go) owns transaction, receipt, history, and event
  helpers; [`migrations.go`](../../migrations.go) evolves SQLite. Public replay
  is in [`history.go`](../../history.go) and [`events.go`](../../events.go).
- **Tools and compaction:** [`tools.go`](../../tools.go) and
  [`execution_tools.go`](../../execution_tools.go) share dispatch and outcome
  settlement. Built-ins live in [`tool_files.go`](../../tool_files.go),
  [`tool_bash.go`](../../tool_bash.go), and
  [`tool_history.go`](../../tool_history.go). Compaction selection and install
  live in [`compact.go`](../../compact.go) and
  [`execution_compact.go`](../../execution_compact.go).
- **Native subagents:** [`subagents.go`](../../subagents.go) owns atomic child
  creation/settings and guarded deletion; [`tool_subagents.go`](../../tool_subagents.go)
  exposes model choices and lifecycle controls. [`subagent_completion.go`](../../subagent_completion.go)
  delivers restart-safe notices through ordinary steering. Child history/events
  use [`tool_history_views.go`](../../tool_history_views.go), not a log subsystem.
- **Bash process ownership:** [`tool_bash.go`](../../tool_bash.go) manages
  commands; [`process_unix.go`](../../process_unix.go) and
  [`internal/proc`](../../internal/proc) observe and signal platform process groups.
- **Hooks:** [`hooks.go`](../../hooks.go) defines typed contracts;
  [`hooks_dispatch.go`](../../hooks_dispatch.go),
  [`hooks_request.go`](../../hooks_request.go), and
  [`hooks_history.go`](../../hooks_history.go) own dispatch, provenance, and
  append-only history.
- **Configuration and handoffs:** [`config.go`](../../config.go),
  [`config_prepared.go`](../../config_prepared.go), and
  [`reload.go`](../../reload.go) prepare and publish config;
  [`models_update.go`](../../models_update.go) publishes only models, and
  [`work_models.go`](../../work_models.go) captures active-work definitions.
  Binding and portable history projection live in [`model_switch.go`](../../model_switch.go),
  [`turn.go`](../../turn.go), and [`history.go`](../../history.go).
- **Provider requests and discovery:** [`responses.go`](../../responses.go) is
  the shared HTTP/auth client and Responses codec; [`provider_target.go`](../../provider_target.go)
  owns endpoint/credential selection; [`discovery.go`](../../discovery.go) and
  [`discovery_metadata.go`](../../discovery_metadata.go) parse catalog facts;
  [`model_reference.go`](../../model_reference.go) holds separately sourced
  reference metadata.
- **Authentication:** [`auth_store.go`](../../auth_store.go) owns the distinct
  auth database; [`auth.go`](../../auth.go) owns account/refresh coordination.
  Browser login, Copilot, logout/receipts, and transfer are in
  [`auth_openai.go`](../../auth_openai.go),
  [`auth_providers.go`](../../auth_providers.go),
  [`auth_oidc.go`](../../auth_oidc.go),
  [`auth_accounts.go`](../../auth_accounts.go),
  [`auth_receipts.go`](../../auth_receipts.go), and
  [`auth_transfer.go`](../../auth_transfer.go).
- **HTTP and SSE:** [`server/server.go`](../../server/server.go) registers and
  translates routes; [`server/browser.go`](../../server/browser.go) checks
  origin/Host; [`server/stream.go`](../../server/stream.go) frames events.
  Auth and catalog adapters live in [`server/auth.go`](../../server/auth.go)
  and [`server/catalog.go`](../../server/catalog.go).
- **Hosts:** [`cmd/main.go`](../../cmd/main.go) owns executable startup,
  signals, and shutdown; [`cmd/catalog.go`](../../cmd/catalog.go) owns model
  snapshot installation. [`example/main.go`](../../example/main.go) and
  [`example/http.go`](../../example/http.go) host the local browser example;
  UI code is split across `example/app.js`, `accounts.js`, `settings.js`,
  `live.js`, and `inspector.js`.

For exact routes and payloads use [SERVER.md](../SERVER.md); for execution,
recovery, tool, hook, and auth contracts use [LIBRARY.md](../LIBRARY.md) and
[AUTH-MODELS.md](../AUTH-MODELS.md). External browser clients consume the standard
HTTP/SSE API; their source and deployment are outside this module.

## Durable execution and state

Conversation SQLite is the authority for accepted commands and acknowledgements,
sessions, history, tool-call outcomes, compaction selections, execution progress,
and numbered events. `migrations.go` evolves that schema. `store.go` owns shared
transaction helpers, dedupe, and append operations; `execution_store.go` owns the
one saved next-action record per active session. Session phase is derived from
that record and live worker ownership, not a competing durable phase field.

Credentials, account registrations, plan blocks, and auth receipts live in a
separate, exclusively owned `AuthManager` database. Workspace files and Bash
logs remain ordinary files; they are not copied into the conversation database.
See [the storage boundary](../DESIGN.md#1-code-boundaries) and
[credential ownership](../AUTH-MODELS.md#one-auth-store-separate-from-conversations).

### Flow: accept a turn, execute tools, show the result

1. `POST /v1/sessions/{id}/turns` is registered in `server/server.go`. The
   handler decodes one strict JSON body and forwards the `Idempotency-Key` to
   `Runtime.StartTurn`; it does not create a parallel job record.
2. `control.go` serializes admission with the runtime transition mutex.
   `session.go:mutateFresh` checks the request receipt first and writes the
   receipt in the same admission transaction as `turn.go:startTurn`. That
   transaction binds the selected model if needed, saves the turn and prompt,
   applies pending steering, and records the first execution action. Only after
   commit does `Runtime.launch` start the worker. A retry with the same key and
   arguments returns the saved acknowledgement; changing arguments conflicts.
3. `execution.go:executeSession` dispatches the saved action. Preparation in
   `execution_prepare.go` drains guidance, runs hooks and builds an immutable
   live request. `execution_model.go` durably reserves each provider attempt
   before credential or HTTP I/O. `responses.go` and `messages_stream.go` require a complete validated
   result before committing canonical output and planning tool calls. Streamed
   deltas are saved as provisional events; failed attempts may retain a labelled
   partial-output note, not completed provider items.
4. The completed response, its validated local tool-dispatch target, and the
   next action are committed together. `tools.go:runCall` checks availability
   and host hooks, then saves the started intent and `tool.started` before
   calling host code. Output events are committed as they are captured. A saved
   result is assembled into history in original model-call order before the next
   request.
5. `store.go:appendEvent` inserts events in the same SQLite transaction as the
   state transition they describe. The event is visible after commit. A client
   watches through `server/stream.go`; SSE replay and follow are backed by
   `Runtime.Subscribe`, with a cursor, not an in-memory output queue.

A turn acknowledgement means the first transition was saved, not that the turn
finished. Provider/tool failures after acceptance are durable events and turn
state. See [the detailed lifecycle contract](../LIBRARY.md#operations).

### Flow: disconnect and reconnect without cancelling work

A client disconnect closes its HTTP subscription, not the `Runtime` worker.
The server turns `Last-Event-ID` or `?after=` into a library event cursor; the
library replays committed rows and then polls for new ones. Reconnects are at
least once, so clients deduplicate by session and sequence. Use `Interrupt` to
stop work; closing SSE only stops watching.

### Flow: discover and install an account-scoped catalog

`GET /v1/providers/{provider}/models` calls runtime discovery through
[`server/catalog.go`](../../server/catalog.go). Discovery uses one explicit
provider catalog GET and does not mutate aliases. The stock executable's
[`catalogHost`](../../cmd/catalog.go) keeps only the latest in-memory proposal
per provider/account and records the current config-file digest.

`PUT` validates that proposal, builds a replacement for only the matching
`catalog_account` entries, and preserves manual aliases, other accounts, and
unrelated JSON fields. It then calls `Runtime.UpdateModelsAndSave`: validate
only model definitions; the host atomically saves its JSON file; the runtime publishes only the model map
without waiting for idle chats or refreshing settings/prompts. Active work keeps
its captured main/summarizer definitions; new work uses latest definitions after
publication. The snapshot is not a durable request receipt and expires on
reload/restart or a newer discovery. See
[model installation](../MODEL-INSTALLATION.md).

### Flow: delegate and receive the result

A root's `subagents.start` uses a stable request ID derived from session/turn/call.
`StartSubagent` applies ordinary session overrides, narrows tool permissions and
commits the child, initial turn and receipt together before launch. The child is
an ordinary executor, but its permanent role excludes further native delegation.
At quiescence, the latest terminal turn and notification marker identify delivery;
`acceptSteering` saves the parent's small `[notification]` message and any follow-up
atomically. Open repairs the terminal-save/notice gap without launching inference.
History/events remain in the same database and are read through visible projections.
Parent deletion checks unfinished owned work under the same transition lock; the
browser's stop-then-delete convenience cannot bypass that guard.

## Runtime extension points

- **Tools:** `Config.Tools` adds or replaces named implementations. Registration
  and dispatch stay in the shared execution path. There is no remote code upload;
  [tool contracts](../LIBRARY.md#tools) explain validation, output capture, and
  uncertainty.
- **Hooks:** `Config.Hooks` adds host-owned typed callbacks at the existing
  lifecycle stages. Accepted text is append-only attributed history; the
  library owns timing, cancellation, fitting, and persistence. The host owns any
  approval UI. See [hook contracts](../LIBRARY.md#lifecycle-hooks-and-host-extensions).
- **Model/provider:** all configured providers use the one executor with Responses/Messages codecs.
  `discovery.go` parses explicit upstream model catalogs; `model_reference.go`
  holds separate versioned advisory data. The stock host's catalog installation
  is a JSON-config operation, not a second runtime registry.
- **HTTP:** handlers translate into existing library APIs. The executable adds
  host-only listener, reload, shutdown, and catalog installation behavior; the
  example adds local registration and explicit Resume setup. Neither owns an
  alternate session engine.

For a new capability, extend its current owner first. A new route or browser
control should be a thin call into that owner, not a second persistence or
execution path. The recipe guide covers exact starting points.
