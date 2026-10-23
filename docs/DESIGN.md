# sam-g code design

This page explains the main boundaries and why they are kept small. The product behavior is in [SPEC.md](SPEC.md), the public Go and HTTP contracts are in [LIBRARY.md](LIBRARY.md) and [SERVER.md](SERVER.md), including [lifecycle-hook details](LIBRARY.md#lifecycle-hooks-and-host-extensions).

## 1. Code boundaries

The main code boundaries are:

- **`sam` (module root):** sessions, model/tool execution, steering, compaction, hooks, SQLite history/events, and recovery. It opens no listener and installs no signal handlers.
- **`server`:** translates HTTP requests into library calls and writes replayable events as Server-Sent Events (SSE). It does not implement agent behavior or edit SQLite directly.
- **`cmd`:** loads configuration, starts the runtime and server, and handles process signals such as SIGHUP and shutdown.
- **`internal/proc`:** small Linux/macOS process-observation helpers used to manage Bash process groups safely.
- **`internal/hostpath`:** shared `~/` expansion for the bundled hosts, not implicit home-directory configuration in the library.

Dependencies point inward: `cmd` uses `server`, `server` uses `sam`, and platform process helpers support the runtime. The public types live in `sam`, so an embedding Go program can use the library without the server.

SQLite stores session state, history, request acknowledgements, and numbered events. Workspace files and Bash logs remain ordinary files on the host. Registrations, credentials, plan limits and auth receipts live in a separate protected SQLite database owned by `AuthManager`. Its three tables (`auth_meta`, `accounts`, `auth_requests`) share transactional publication and exclusive ownership; no auth state is copied into conversation databases. See [authentication/discovery](AUTH-MODELS.md).

## 2. Runtime ownership

A `Runtime` owns one local database at a time. It takes an exclusive sidecar lock before migration or recovery and keeps ownership until workers and owned processes have settled and storage is closed. A second runtime cannot open the same database concurrently.

The database is the durable record. In-memory state holds worker and cancellation handles, not a second recoverable conversation. Each active session has one worker; separate sessions can run concurrently. A client request or event subscription does not own accepted work, so disconnecting stops observation, not the turn.

Short state transitions and required database writes are coordinated. Model requests, tools, host callbacks, and prompt-file preparation run outside runtime/SQLite locks. This keeps the state changes ordered without holding storage locks across network calls, shell commands, or a human approval wait.

## 3. Configuration and reload

Providers and models are named configuration entries. The runtime uses one HTTP/auth client with a closed Responses/Messages codec boundary, not two executors. Native private blocks belong to existing canonical history items and the existing visible/portable projection; there is no second transcript table. Explicit standalone discovery shares provider normalization and credential resolution, but makes one catalog GET without persistence/cache/retries/config mutation. Provider facts and versioned advisory Pi metadata are separate fields; references never substitute a model list or overwrite account-reported capabilities. Configured model aliases/budgets remain separate. The first accepted prompt or steering message binds a session to its main model identity. An explicit SwitchModel transaction changes model/account selection for future work while preserving canonical history. The first binding pins both choices in overrides, including legacy saved choices on reopen; shared-default changes cannot redirect used chats. One saved history boundary makes older protocol-specific items portable; there is no second conversation or historical model registry. Ordinary reload never redirects a binding. Missing catalog entries keep chats readable but cannot run.

`Runtime.Reload` replaces shared configuration while **every session is fully idle**; `UpdateConfig` and `ReloadSession` use a **target-session-only idle boundary**. Busy updates fail rather than queue or interrupt work. Preparation runs outside state/SQLite locks, then publication rechecks the boundary and commits atomically. Failed preparation leaves the installed configuration in place. See [the configuration contract](LIBRARY.md#unified-configuration-reload).

Session overrides preserve intent: omitted fields inherit shared defaults, while explicit empty, false, zero, or empty-list values remain choices until removed. Prompt files are read during setup or explicit reload, then their installed contents are reused by turns, compaction, and inspection. The stock executable exposes reload with SIGHUP; there is no HTTP code-upload route; per-chat prompt refresh uses `ReloadSession`. See [the configuration contract](LIBRARY.md#unified-configuration-reload).

The stock host installs account-scoped discovery snapshots into that same JSON `models` map. Managed provenance distinguishes them from manual entries; no parallel catalog file/database is added. It serializes SIGHUP and file writes, and uses `UpdateModelsAndSave` to validate definitions, atomically save, and publish only the model map without idle-chat coordination. Workers capture just their main/summarizer definitions under the admission lock, so lazy preparation and retries never resolve against a later catalog. The prepared owner and unrelated config fields stay stable while workers run. No prompts/settings/resources are refreshed. Once saving starts, browser cancellation cannot abort publication; real failures seal admission and close the runtime. Recovery uses latest configuration. See [installation and model handoffs](MODEL-INSTALLATION.md).

## 4. Turns, tools, and events

One session executor reads a saved next action: `prepare`, `request`, `batch`, `compact_prepare`, `compact_request`, or `stop`. New and recovered work use this same dispatcher. The selected codec and shared transport send one attempt; the executor owns durable reservation/retry timing. Worker memory holds immutable request bodies and whole-turn/compaction account pins, never a second persisted transcript or executable-config snapshot. Tool calls from one response may run concurrently within the session limit; their results return to the model in call order. The built-in file tools share one runtime-wide read/write lock, while Bash and host tools remain outside that lock.

Events are committed to SQLite before subscribers can read them. Subscribers replay after a sequence number and then follow new events from bounded pages. A slow or disconnected client cannot block the worker. Delivery across reconnects is at least once, so clients deduplicate by session and sequence.

Tool authority belongs to the runtime's operating-system user. A workspace is a working directory, not a sandbox. The output cap fails an oversized tool call with advice; it does not save an overflow archive or undo earlier effects. Background Bash commands are owned while the runtime is alive, but readiness and port forwarding remain the host's responsibility. Exact tool and process contracts are in [LIBRARY.md](LIBRARY.md#tools).

Human file viewing uses `Runtime.ReadFile` and a thin HTTP adapter, not model-tool
dispatch or a web-host proxy. It reuses workspace path resolution and bounded
capture with an independent per-open budget, without file locks, persistent
preferences, config changes, registries or inference.
It is an explicitly requested owner-level read, including sensitive text if
requested, not redacted metadata. See [the file contract](LIBRARY.md#human-owned-file-viewing).

## 5. Persistence and recovery

The runtime records accepted commands and acknowledgements, tool start/result boundaries, model outputs, and events in short transactions. A required write failure stops new work rather than continuing effects without durable records. Storage contention before a command is accepted is different: the command returns `storage_busy`, and the same request ID can be retried.

One execution row per session records work and attempt state. Tool calls retain their original namespace and prepared-request dispatch authorization; restart never derives authorization from current provider configuration. Phase comes from execution state and live worker ownership; joining workers still block idle admission. Unfinished records without a recovery checkpoint are conservatively interrupted.

Open reconciles abandoned attempts and loads latest configuration, but starts no work. Resume explicitly admits suspended rows and acknowledges repeated external effects. Saved known tool outcomes never rerun. Reconciliation saves skipped outcomes for unfinished replay-unsafe calls; the ordinary batch executor runs the remaining replay-allowed calls and presents all paired results to the model. A saved restriction bit or stricter current tool definition prevents replay; no tool versions or separate recovery engine are stored. See [recovery policy](LIBRARY.md#recovery). One progress record preserves four provider slots, retry deadlines and one 401 refresh cycle. Shared settlement quotes only committed partial main deltas, exactly once per unfinished attempt. Summary text stays private.

Every outcome and next action commit together, including final-answer/follow-up acceptance and summary installation/parent advancement. Saved stop defeats every late transition while retaining known output. Close suspends eligible work; Interrupt saves terminal stop before signalling. No old-config manifests, callback ledger, parallel recovery engine or task framework is needed. Background processes are not adopted because saved PIDs may have been reused; an unacknowledged built-in Bash launch is skipped with uncertainty under the recovery policy.

### Native child sessions

[Subagents](SUBAGENTS.md) add a live parent link, a last-notified-turn marker,
and a permanent child-role flag. The flag survives parent deletion so detachment
cannot grant delegation authority.
Creation, first-turn acceptance and the normal request receipt commit together;
replayed tool calls derive the same request identity. Child turns use the existing
executor, tools and AuthManager. No job engine, scheduler or history store exists
beside sessions. At quiescence (after ordinary steering handoff), the latest
terminal turn determines the notice identity. The marker, existing steering
receipt, small `[notification]` text and optional parent turn commit together.
Only a newly accepted parent worker is launched after commit. Open closes any
terminal-save/notice gap without launching effects; Resume uses ordinary saved
executions. Stop traverses owned descendants in one transaction; shutdown does
not use that terminal-stop path. No source/provenance fields, second inbox or
cross-agent editing coordination are added. Native delegation is one level: child
prompts omit the tool and the library rejects nested creation/re-enabling. The
library guards parent deletion while child work is unfinished; the web's optional
stop-then-delete flow orchestrates existing operations, not an implicit runtime cascade.

## 6. Compaction and prompt continuity

Compaction creates a summary for future model requests; it does not delete original history. Selection, summary, protected user request, and recent whole tool/result groups are checked together before installation. The recent-history target is a minimum, and the exact protected user reference stays fixed until the next successful compaction. See [the retention examples](SPEC.md#compaction-and-prompt-continuity-examples), including compaction within the first user turn, and [the compaction API](LIBRARY.md#compaction).

## 7. Host extensions

Go hosts may register typed lifecycle hooks and custom tools. The host owns callback code, approval UI, and callback side effects; the runtime owns when callbacks run and how accepted text, decisions, and failures are recorded. Hooks share the same all-sessions-idle reload as the rest of configuration. Their ordering, persistence, cancellation, and compaction rules are in [the library hook reference](LIBRARY.md#lifecycle-hooks-and-host-extensions).

## Browser host ownership

The plain-JavaScript example keeps one disposable selected view: session identity, saved configuration, one override draft, pending session actions, rendered-history identity and live previews. Account/catalog/shared-model work is separate, and Stop has independent ownership. Switching discards unsent composers/settings, not the runtime's saved steering. Tool previews retire against actually rendered canonical turn/call results, because individual tool settlement precedes batch history assembly. Terminal chat failures replay from the existing durable events; other messages capture their originating section and account/session before async work. No second durable history/error store or notification framework is needed.

The host registers model configuration, then the browser creates sessions and sends mutations directly through `/v1`; there is no private existing-chat preflight or duplicate session setup. The host model mutex serializes candidate construction, model-only file persistence and publication only. Immutable host reads, login and Resume use their existing owners. `models.json` remains host-owned and loaded before Open. Like the stock server, the example uses `UpdateModelsAndSave` while chats may run, with fail-close after an uncertain save/publication failure. See [catalog-only publication](MODEL-INSTALLATION.md#safe-publication-and-operator-edits).

External browser clients can use the standard HTTP/SSE API without embedding the runtime. Each backend owns its own accounts, sessions and history; a client's connection profiles or display preferences do not merge those stores. The bundled example instead hosts the runtime and page together, with its own stores and host-only registration/Resume actions.

## 8. Server and deployment boundary

The optional server is a transport adapter for one trusted user, not a public multi-user service. The executable defaults to loopback and permits only loopback or Tailscale listen addresses. SSH/Tailscale controls access; there is no built-in API authentication or approval UI. An embedding application chooses its own exposure boundary.

On shutdown, the executable stops accepting requests and closes the runtime. The runtime cancels and settles session work, stops owned background processes where it can confirm ownership, and releases the database only after cleanup. If process ownership becomes uncertain, it reports that uncertainty instead of claiming cleanup succeeded. See [deployment and recovery](SERVER.md#shutdown-and-recovery).
