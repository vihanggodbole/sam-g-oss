# sam-g: lean Go agent runtime

The product contract for sam-g. Exact Go and HTTP APIs live in [LIBRARY.md](LIBRARY.md) and [SERVER.md](SERVER.md); see the [overview](README.md#current-limitations) for current limitations.

## Goal

Build a small Go library and remote service for persistent, general-purpose agent sessions. Ship six tools: `read`, `write`, `edit`, `bash`, `history`, and `subagents`. No bundled UI, approval workflow, or large tool catalog.

The primary user is one person running trusted clients and remote machines connected through Tailscale. Target Linux and macOS with Go 1.27. Use the Go standard library except for a pure-Go SQLite driver and its dependencies. Code lives in `sam-g/`; documentation lives in `sam-g/docs/`.

See [DESIGN.md](DESIGN.md) for the code structure and architecture trade-offs.

### Primary scenario

From a MacBook, start a task against a workspace on a remote machine. Close the laptop. The remote agent continues calling models and using tools. Reconnect later to replay missed output, inspect changes, steer the work, or interrupt the current turn. Continue the same conversation afterward.

## Components

- **Go library:** session and turn lifecycle, model integration, tool execution, events, and persistence. Usable directly by a Go application without running an HTTP service.
- **Remote service:** a thin host for the same library, exposing HTTP commands and Server-Sent Events (SSE). It owns running tasks independently of client connections.
- **SQLite storage:** history, session metadata, turn state, steering messages, and replayable events on the host machine's local disk.
- **Workspace:** ordinary files on that machine. Source code and tool-created files are not stored in SQLite.

Use a service manager to keep the remote service running and restart it after failure. Do not require a MacBook process, terminal connection, or Node.js runtime to stay alive.

### Library/server boundary

Keep the reusable library and server adapter in separate Go packages, with a separate server executable. Dependencies point from the server to the library, never the reverse.

- **Library owns behavior:** agent execution, session concurrency, steering, cancellation, compaction, SQLite persistence/recovery, request deduplication, and typed replayable events. Expose these through Go APIs without HTTP request/response types or SSE framing.
- **Server owns transport and hosting:** parse HTTP requests, call library APIs, map errors to responses, encode events as SSE, and manage startup/shutdown. Do not duplicate agent or session logic in handlers.
- Starting a library runtime does not open a listening socket, install process signal handlers, or start an HTTP server. The host explicitly opens and closes the runtime and supplies configuration.
- The host supplies the runtime lifetime; individual client requests and event subscriptions do not own running turns. Embedded Go apps get the same execution and recovery behavior without a server.

Use one durable six-action executor for fresh and resumed turns and manual/automatic compaction. Operation acceptance, result/next-action transitions and completion/follow-up handoffs are atomic. Keep their eligibility rules explicit without duplicating execution/persistence logic or introducing a generic command framework.

Maintain small unit tests only; see the [test-selection policy](maintainers/changing-and-testing.md#test-selection). Whole-system behavior is not covered by an automated integration suite.

## Sessions and turns

A session has a stable ID, absolute workspace path, configuration, and persistent conversation history. A turn starts with a user prompt and includes all model requests and tool calls needed to reach a final answer.

- Allow one active turn per session. Reject a second ordinary prompt while a turn is active; use steering instead.
- Support independent sessions concurrently without a library-imposed active-session limit or global session queue.
- Persist accepted prompts before acknowledging them.
- Track turns as `running`, `suspended`, `completed`, `cancelled`, `failed`, or legacy `interrupted`. Suspended work keeps its original IDs and has no end time.
- Support creating, listing, inspecting, continuing, and deleting idle sessions. Reject parent deletion while owned child work is unfinished; a UI may offer to stop work first. Preserve child histories and their permanent non-delegating role when a parent is deleted.
- Do not impose an implicit turn deadline or model-step cap. A turn runs until completion, explicit interruption, or error. Hosts may enforce their own budgets through cancellation. An oversized tool result fails that tool call, not the whole turn; the model can choose a narrower operation.

### Steering

Accept additional user guidance while a turn is running. Persist and acknowledge it immediately, then apply it in order before the next model request. Steering does not abort the current model request or tool operation.

Emit separate accepted and applied events. If the turn completes normally before accepted guidance is applied, automatically start a new turn with the pending guidance in arrival order. Serialize steering acceptance and turn completion so a racing message is either applied to the current turn or durably scheduled for the next, never lost or applied twice. A steering request arriving after normal completion starts a new turn when the session is idle.

Explicit interruption or terminal failure must not trigger this automatic follow-up. Clean shutdown may save a completed response and its accepted follow-up atomically, but never launches new follow-up effects. Suspended recovery preserves the guidance pause choice. After terminal interruption/failure, retain unapplied guidance visibly as paused until the user explicitly continues or clears it. Support listing and clearing all pending guidance, not editing or removing individual messages. Clearing affects only guidance still pending when the command is accepted; it does not undo applied guidance or interrupt work.

### Interrupting

Interrupt always means stop the selected session's current work and pause pending guidance. There is no turn-targeted interruption mode: if a follow-up has started when the interrupt is accepted, stop that follow-up. If the session is already idle, leave it idle and pause any pending work. It never exits the Go host/server process, closes the runtime, deletes the session, or interrupts unrelated sessions. Owned child sessions and their descendants are stopped/paused together with the parent. Stop scheduling work and cancel that session's model requests and active tool calls. Preserve completed effects and partial output without pretending unfinished tool calls succeeded.

Cancelling a `bash` call signals its owned subprocess group where possible (SIGTERM, up to 2 s for the whole group, then SIGKILL); this is tool cleanup, not termination of the agent host process. If exit observation fails, the tool reports `process_observation_failed`, sends no further signals after ownership becomes uncertain, and does not claim cleanup succeeded. Cancellation cannot undo file changes or external side effects, and detached subprocesses may outlive it. Keep the session usable for another prompt after cancellation settles, and pause pending steering so interruption cannot immediately restart work.

## Models and thinking

Use streaming OpenAI-compatible Responses endpoints (`/v1/responses`), with configurable base URL, model, and credential source. OpenAI's Responses API is the primary target; live validation against it is still pending. Copilot Opus 5.5 additionally supports native Anthropic Messages under an explicit per-model API. Empty API remains Responses; unknown or unsupported combinations fail. See [native Messages](LIBRARY.md#native-messages-through-copilot) for output caps, thinking, faithful roles, privacy and completion. Chat Completions is not a v1 target. Do not assume every Responses-compatible endpoint supports identical optional features.

Support multiple named providers and models in one JSON configuration file. Each model entry references a provider and declares its API model ID, context budget, and supported reasoning options. Sessions select a configured model by name; compaction may select a different configured model, including one on another provider. List this configured catalog through Go and HTTP APIs. Explicit standalone/runtime provider discovery queries a fresh upstream catalog without caching, inference, configuration mutation or alias installation. Unknown provider capabilities stay unknown. Exact-ID Pi metadata may be returned separately as a versioned advisory reference, never as provider facts or a fallback availability catalog; see [metadata sources](MODEL-METADATA.md). Configured execution budgets remain host choices.

A main-model change is accepted through `UpdateConfig` only when the target session is fully idle and has not accepted a conversation message. The first accepted prompt or steering message locks that session's model selection and resolved provider/model/API identity, even if the message is later cleared or history is compacted. An explicit `SwitchModel` handoff can select a new configured model/account for future work when that session is fully idle, including stopping workers and immediate follow-ups; unrelated sessions may keep working. It preserves stored history and records the new binding and a portable-history boundary atomically; see [model installation and handoff](MODEL-INSTALLATION.md). Ordinary override edits do not bypass the binding. Configuration edits or restarts must not silently redirect an existing session to another model or endpoint. The main login account also requires explicit SwitchModel after the chat is used. Model/account overrides become pinned at first binding (legacy choices pin from saved configuration on reopen), so later defaults or omission cannot redirect a used chat. Compaction settings remain ordinarily configurable while the target session is fully idle.

- Decode Responses SSE events for text, reasoning summaries, function-call arguments, usage, and completed/failed/incomplete responses. Correlate output items and function results by their IDs; never execute partial function arguments.
- Persist enough input/output items, including required opaque reasoning data when supplied, to reconstruct subsequent requests without relying solely on provider-side response IDs. Do not expose opaque reasoning data as visible thinking.
- Allow reasoning-effort configuration where supported.
- Stream reasoning or reasoning summaries only when exposed by the provider. Do not invent or require hidden chain-of-thought.
- Report unavailable reasoning output explicitly. Reject unsupported requested options rather than silently ignoring them.
- Preserve provider-required conversation fields needed for subsequent requests.
- Record usage when supplied by the provider.

Reserve model attempts durably before credential/provider I/O, allowing at most three retries after the initial attempt across all restarts for retryable failures such as temporary network errors, rate limiting, and transient server errors. Honor a valid `Retry-After` header on 429 responses; waits must be interruptible. Do not retry permanent errors, user cancellation, or completed tool effects. This applies to main-agent and compaction requests, not whole turns. Without a valid Retry-After, wait 1, 2, and 4 seconds before successive retries. If answer text or reasoning-summary content from the request has already been persisted/published, a stream failure stops the turn instead of retrying or resetting the partial answer; preserve it for inspection and require explicit continuation.

Credentials come from host configuration/environment or from subscription logins (below), never from committed configuration or persisted event payloads. Redact credentials from diagnostics. Use one concrete HTTP/auth owner with a closed Responses/Messages codec dispatch for all configured providers, with endpoint, credentials, model, and supported options supplied as configuration. Do not build provider factories, a second executor, or middleware chains for hypothetical future APIs.

### Subscription login and accounts

Scenario: complete ChatGPT browser authorization on the laptop, move its selected protected registration to the stopped VM host, then let the VM refresh on demand. The laptop's loopback callback is not the VM's. The full implemented protocol, local commands, public API and evidence limits are in [AUTH-MODELS.md](AUTH-MODELS.md).

- OpenAI authentication uses direct SIWC. It uses dynamic public-client registration with the real app name/stable host ID, PKCE/OIDC, a host-owned literal 127.0.0.1 listener, issued-client reuse, verified identity and explicit plan-consent intent. The core launches no browser/listener. Copilot retains GitHub device login and token exchange; API-key inference stays separate.
- One AuthManager-owned protected SQLite database owns accounts, registration/credentials, plan facts and operation receipts. It is separate from conversation storage and exclusively owned. Several runtimes may share the manager. Published handles follow committed writes; uncertain credential persistence fail-stops rather than using stale cached credentials.
- Login transients stay in memory; durable receipts contain no code/verifier/state/nonce/authorization URL. Accepted callback exchange/polling survives waiter disconnect. Cancel/logout/Close join owned work. Browser attempts have no invented timeout; Copilot honors polling intervals/slow_down/expiry. Reopen cancels unfinished logins instead of replaying them.
- Expired credentials renew automatically on demand, per-account, with atomic rotation. Actual SIWC rotating refresh records durable uncertainty before I/O; validated credentials commit before resolving it. A definite token-endpoint rejection clears only the current attempt's marker, not older uncertainty; downstream validation failure is not such a rejection. Copilot's saved-GitHub-token exchange is nonrotating and does not use that marker. OpenAI access metadata remains opaque; refreshed ID tokens preserve verified identity. Unknown earliest_refresh_at scheduling semantics are not guessed. SIWC does not force refresh on every 401; Copilot retains its existing bounded forced-refresh cycle.
- Logout commits unusability and a pending receipt before remote revocation, joins refresh/login work, clears local tokens even when remote cleanup is unconfirmed, and retains registration/host ID/plan block. Maximum three revocation attempts use Retry-After or 1/2-second fallback. Stock hosts impose ten seconds; direct Go callers choose a deadline. Reopen finishes accepted retirement locally without a remote queue. Copilot reports local-only not_applicable. Old receipts cannot affect newer logins.
- Exact SIWC usage-limit errors atomically block new plan inference attempts across runtimes sharing the manager, including retry/compaction. In-flight responses may finish. The block survives restart/refresh/routine reauthorization. Catalog/status reads remain usable. Explicit AllowPlanUsageRetry clears only the local block, not consent/quota or a turn. ResetAt stays unknown without a documented field for that exact limit; generic Retry-After is not reset proof.
- Local ExportAccount/ImportAccount and commands move one protected registration, never revoke it, preserve destination host identity and reject stale/active overwrites using transfer receipts. Interrupted export never reactivates the source automatically. No cross-machine atomicity, HTTP token export, SSH/tunnel service, sync framework or arbitrary-program credential import is promised.
- Each live turn pins its account generation through steps/tools/compaction; restart deliberately repins current login without resetting attempt budgets. Used-chat account changes require the same explicit SwitchModel handoff as model changes. No account rotation or API-key fallback; saved account selections cannot silently become API-key billing.
- Original history, opaque reasoning and hook roles remain unchanged. A provider rejecting required history fails with history_incompatible and pauses. SIWC uses only public Responses, full input, store:false/stream:true, allowed fields and the sam tool namespace. Validated dispatch targets/unavailable decisions persist atomically with completed output, so restart never turns other.read into local read. Only a complete validated response (Responses response.completed or Messages message_stop with valid terminal state) authorizes tools.
- Credentials belong only in protected auth storage/bundles, never conversation SQLite/events/hooks/logs/status. Same-user tools can read that storage. Logical deletion does not erase historical SQLite pages/filesystem backups. Public errors preserve safe structured provider facts without raw bodies. Live provider/client eligibility and serving acceptance remain separately unverified.

## Tools and execution policy

All six tools are enabled by default. Configuration may disable individual tools. Disabled tools are neither advertised to the model nor executable through dispatch. There are no built-in approval prompts or permission modes. Go hosts may install BeforeTool callbacks to await their own approval policy/UI.

| Tool | Contract |
| --- | --- |
| `read` | Read a text file, with optional line range. Fail descriptively if the selected output exceeds the limit. |
| `write` | Create or replace a text file, creating missing parent directories. |
| `edit` | Apply exact text replacements. Require unique, non-overlapping matches; validate every replacement before changing the file. |
| `bash` | Run a command using Bash in the session workspace, streaming stdout/stderr and reporting exit status. Support timeout and cancellation. An explicit background mode launches a long-lived process (for example a dev server) with its output appended to a required log file. |
| `history` | Search (Go regular expression, 10 matching entries per page, newest first) or read (by sequence range or around one entry) the caller's or an owned child's visible history/events, including entries a compaction summary replaced. Read-only. |
| `subagents` | List model/provider/thinking choices; start, status, steer and cancel one-level child sessions. Children never receive native delegation authority. See [the complete contract](SUBAGENTS.md). |

Resolve relative paths against the session workspace. This is a working-directory convention, **not a sandbox**: tools have the service user's operating-system access. Disabling a file tool does not prevent enabled `bash` from accessing files.

Allow parallel execution of tool calls returned in the same model response, with configurable per-session tool concurrency (default: four). Each session runs one batch at a time, so the limit applies to running calls in that batch across all tool names, not separately to each tool type. Calls may complete out of order; stream events with call IDs as they occur, then supply all results in stable model-call order before the next model request. Interruption cancels active calls and prevents queued calls from starting.

Use one runtime-wide read/write lock for built-in file tools: reads may overlap; writes and edits run exclusively, including validation and application. Do not build per-path locks or a conflict scheduler. This deliberately serializes mutations even to unrelated files. Bash and custom tools do not use this lock and may still run concurrently; it is not workspace isolation or dependency detection. Hosts can set concurrency to one when ordering matters.

Apply one configurable output limit per tool call, defaulting to 256 KiB (262,144 bytes). For `bash`, count stdout and stderr together. Exceeding the limit fails that call with `tool_output_limit`, the configured limit, and actionable advice: select a smaller line range, filter output, or produce a concise summary. Do not return a truncated result as success, automatically retry, or save an overflow log file. The agent may use the error to choose its next action.

Enforce the limit during capture, not after buffering unlimited output. Cancel an overflowing command's owned subprocess group where possible. The error must say that prior side effects may have occurred; output overflow is not rollback. Already streamed output up to the limit may remain in session history, but no further tool output is saved or published. Other parallel calls continue unless the session is explicitly interrupted.

Provide a small Go tool interface so hosts can add or replace tools; do not build a plugin system. Validate each host registration at Open and unified reload (non-empty name, usable object JSON schema, non-nil function). Skip an invalid one with a structured startup diagnostic instead of failing startup; an invalid replacement disables that tool name rather than falling back to the built-in. Do not recover arbitrary tool panics.

Tool arguments are checked concretely before any mutation: missing or null required text (`write.content`, `edit.new_text`) fails, while explicit empty strings remain valid. Exact-edit uniqueness counts overlapping occurrences. File tools support ordinary files and UTF-8 text only.

### Background processes

Scenario: the user asks the agent to start `npm run dev`, keep working, and point them at the preview. The agent calls `bash` with `background: true`, the command `npm run dev` itself (no trailing `&`), and an explicit `log_file`. The command starts in its own process group with stdin disconnected and stdout/stderr appended to the log file. The call returns a short launch result with the PID and the resolved log path as JSON metadata, not a claim that the server is ready or succeeded; the agent checks readiness with ordinary commands and reads the log with existing tools.

- **Lifetime.** The launched command is the managed process. It survives the launching call, the end of the turn, and interruption of session work. It ends when that command exits on its own (whatever it left running in its group is then killed at once and the leader is reaped promptly), or when its session is deleted or the runtime shuts down. Deletion and shutdown send SIGTERM to the whole group, give every member up to 2 s (even if the leader exits first), then SIGKILL what is left and reap the leader. Session deletion never deletes workspace or log files. A command such as `npm run dev &` that backgrounds its server and exits therefore stops that server; background mode is the outer backgrounding.
- **Ownership.** One owner per launch, in memory, for processes this runtime started. The leader is not reaped until the owner's last group signal, so the group ID cannot be reused while it may be signalled; nothing signals it afterwards. Deletion and shutdown join cleanup already in progress. If exit observation fails (including `ECHILD`/`ESRCH` where the OS cannot confirm a still-owned child), the owner sends no further signals, reports `process_observation_failed`, and does not claim cleanup succeeded; a command may remain running. All bash process-observation failures remain in runtime diagnostics and make deletion/`Close` return that error; foreground bash also returns a failed tool result. The host must not ignore `SIGCHLD` or enable automatic child reaping; sam does not install or change host signal handlers. After a crash, processes are not restarted, adopted, or signalled; never signal a PID read from storage, since PIDs are reused. Processes that deliberately leave the group (`setsid`, daemonizing) are not owned.
- **Launch result.** The launch result is the saved tool output, visible in history and events after reconnect or restart, and is subject to the ordinary combined output limit. Before starting, the runtime checks that the whole result, with room for the largest PID, fits that limit; if not, nothing starts and the call fails with advice to use a shorter path or raise `tool_output_limit` while the session is idle. If the result cannot be saved after the process started (a storage failure), that new process is stopped; earlier acknowledged processes keep running, and effects that already happened are not undone. A crash between start and saving leaves an untracked process and an uncertain tool call.
- **Log file.** Required; there is no temporary fallback. Relative paths resolve against the workspace; symlinks and outside paths are allowed (not a sandbox). Existing parent symlinks of a new file are resolved, and the result reports the real path. The opened file must be an ordinary file; opening does not block on a FIFO. Log contents stay in the file: not copied into SQLite, not streamed, not capped, rotated, or archived. These checks are not protection against concurrent filesystem tampering.
- A dev server's `localhost` is the remote machine's. Users forward ports with their existing SSH/Tailscale setup; the runtime manages no network exposure or service installation.
- If an already-acknowledged background launch later cannot be observed safely, the launch acknowledgement is unchanged, but `Diagnostics()`/`GET /v1/diagnostics` reports the lifecycle error. Deletion and shutdown return `process_observation_failed`; after ownership becomes uncertain sam deliberately does not signal the stored PID/PGID. The caller must inspect the machine and handle any surviving command.

`nohup` alone does not leave a process group, so foreground `bash` still stops it with the rest of the group; background mode is the supported way to keep a process.

### History tool

Scenario: hours into a task, older conversation was compacted. The agent needs the exact callback URL the user gave at the start, which the summary paraphrased. It calls `history` with `{"action":"search","query":"(?i)callback.*url"}`, gets entries with sequence numbers and short excerpts, then `{"action":"read","around_seq":124,"before":3,"after":3}` to see the original message and its context.

- Read-only access to the current session or a direct owned child selected by optional `subagent_id`; ownership uses runtime-supplied `ToolCall.SessionID`. No arbitrary parent identity or SQL is accepted.
- Optional `view` selects saved `history` (default) or diagnostic/progress `events`. Both reuse visible projections, bounded scans, output limits and range validation. Event search matches ordinary event JSON; reads return Event envelopes. Seq numbers and cursors are view-specific. See [native subagents](SUBAGENTS.md).
- Searches visible text: user/assistant messages, refusals, reasoning summaries, tool calls (name and arguments), tool results, and runtime notes. Compaction never removes access. Encrypted reasoning, provider-private data, and credentials are never searched or returned. Background logs stay files; their recorded launch results are searchable. The history tool's own calls and results are omitted from search, so searches do not rediscover their own output; read returns them like any other entry.
- `search` takes `query` (Go `regexp`/RE2 syntax, compiled once per request; invalid patterns fail descriptively) and optional `cursor`. There is no `limit` argument (it is rejected): a page always holds at most 10 matching **entries**, each once, newest first, with its sequence number, turn ID, kind/type/role, category, and a short excerpt. `next_cursor` is returned only when an eleventh match exists. Pages continue below the previous page, so entries added after the first page never reshuffle later pages; a fresh search includes them. Cursors are plain local state, checked against the session, view and query; malformed or mismatched cursors fail clearly.
- `read` takes `from_seq` and `to_seq` (inclusive), or `around_seq` with optional `before`/`after` entry counts (default 0); mixing modes or invalid ranges fails. It returns the same visible entries as the history API, including earlier history lookups, and `before`/`after` count those entries. A read larger than the tool output limit fails with advice to narrow the range; nothing is truncated or archived. Excerpts are previews; reads return originals.
- SQLite only retrieves rows. Search scans small batches newest first, releasing rows between batches and checking cancellation, until it finds an eleventh match (to decide `next_cursor`) or reaches the start of history, and keeps only the page's results. It holds one batch of rows at a time, not a byte budget: one very large entry still takes its size in memory. There is no search index, cache, or SQLite regex extension.
- It is an ordinary tool call (events, persistence, output limit, disabling); searching is not a session mutation.

## System instructions

Provide a short default system prompt describing the agent and enabled tools. It tells the agent to consult original history (search, then read context) when a compaction summary lacks an exact detail, rather than guessing. Hosts may append instructions as text and/or through an explicitly configured UTF-8 Markdown file, such as `system_append.md`.

Compose instructions in order: default prompt, host text, append-file contents, workspace-root AGENTS.md (CLAUDE.md fallback only when AGENTS is absent). Prepare files through the common configuration helper at Open, new workspace/session setup, explicit unified reload and per-session prompt reload; never reread them in turns, compaction or inspection. For configured append/compaction files, new setup reuses installed text for previously prepared paths and reads only newly introduced paths. Workspace instructions and skill catalogs are discovered afresh at setup and then frozen per session. Preparation errors reject setup/reload synchronously and preserve installed state. Save initial installed instruction/settings on each turn as historical facts. Inspection uses the current worker's prepared main settings/base, or installed base when no request is prepared (including suspended work), never old turn settings or private summarizer instructions; its estimate includes retained history/summary/protected user/tools but excludes uncommitted pending guidance.

Discover workspace-root instructions and global/workspace skills, plus explicitly configured extra skill directories, using the [resource contract](LIBRARY.md#workspace-instructions-and-skills). No ancestor instruction traversal or automatic remote prompts. Skills are a YAML-frontmatter catalog with bodies read on demand, not executable plugins or authority grants. Stable instructions and native tools remain separate fields; the catalog is leading developer input for Responses and ordered leading system blocks for Messages. Provider-internal token ordering cannot be guaranteed. Expose the installed snapshot through config/context and configuration events for client setup/reload displays, never synthetic conversation notices.

## Context compaction

Support automatic compaction before a model request approaches its context limit, plus manual compaction only while the session is idle. Reject manual requests during active work with a clear conflict: interrupt the session or wait before compacting. Do not queue manual compaction. Configure the context limit, trigger threshold, output-token reserve, and recent-history budget. Use explicitly configured model context budgets and conservative token estimates. Default the automatic trigger to 90% of the input budget after output reserve, and the retained recent-history budget to 20% of that input budget. Automatic compaction may be disabled.

Allow compaction to select a different named model from the configuration, resolving its provider endpoint, credential reference, supported reasoning options, and context budget, so a cheaper model can summarize older history. Default to the session's provider/model when no override is set. Compaction never enables tools. Explicitly selecting a different provider authorizes sending the selected history to that provider; do not silently switch providers on failure.

Supply a default compaction prompt; allow replacement through inline text or an explicitly configured UTF-8 Markdown file. Reject configuring both overrides. Snapshot the effective installed prompt and provider/model for each compaction. Files were prepared at setup/reload, never independently reread by a compaction worker. An unreadable configured file rejects preparation, preserving the previously installed prompt. The default prompt preserves the task, constraints, decisions, changed files, known tool outcomes, and remaining work. Record compaction usage separately from main-agent usage.

- Automatic compaction runs only at a safe boundary between model/tool steps, never during a running tool. Manual compaction starts from idle as a standalone operation. Steering and interruption remain available while compaction is active. Manual compaction never clears a session's pause state. After success, pending steering starts a turn only if the session is unpaused; otherwise it remains paused. Failure or interruption never starts a turn.
- Allow compaction of completed model/tool steps within the current turn, not only earlier turns. Keep current system instructions, pending guidance, and recent history outside the summary. Retain at least the recent-history token target in complete items, expanding backwards for tool/reasoning groups and whole native Messages responses. Keep the genuine user request preceding the earliest retained exchange separately when outside that suffix; do not keep the entire intervening long tool loop. Install that exact user reference with the summary and retain it until the next successful compaction, even after new guidance arrives. Never split tool calls from their results or lose unresolved tool-call state.
- Make one logical summarization request per compaction operation, with the shared transient-failure retry policy and at most one accepted summary. Convert selected history to a portable summary input rather than forwarding another provider's opaque protocol items. If the input exceeds the configured compaction model's context budget, fail descriptively and recommend a larger-context model. Do not split, recursively summarize, or automatically fall back to another model. Verify the resulting summary reduces context and fits the main model's budget; otherwise fail without replacing the previous context. Do not require reduction below the automatic trigger or another success threshold, and do not retry an inadequate completed summary.
- Atomically save the summary, replaced history range, and protected-user reference. Include the expanded tail and protected user in fit checks; if they cannot fit, fail rather than silently drop them. Preserve original history and events in SQLite for inspection and replay; compaction is not database cleanup. Existing summaries migrate by freezing the legacy context visible at migration time, not reconstructing unavailable installation-time state (see DESIGN.md).
- Emit compaction started/completed/failed events. Cancellation or failure leaves the previous context selection intact. Any terminal automatic compaction failure (non-retryable or after exhausting allowed transport retries) stops the turn with a descriptive error and pauses pending guidance, even if the old context still fits. Require explicit continuation; do not start a new summarization attempt on subsequent steps of the same turn. Manual compaction failure returns the session to idle with pending guidance paused.

## Lifecycle hooks

Go hosts register independent typed `Hook[I,O]{ID, Handle}` records in `Config.Hooks`: BeforeTurn and BeforeRequest return plain text; BeforeTool explicitly allows/denies; BeforeCompaction explicitly allows with optional appended summarization instructions or denies. Invalid registrations fail Open before database ownership and reject unified reload. Multiple handlers run serially with detached snapshots, no priority/order guarantee, and stop at the first deny/error/invalid return. Other sessions/calls may overlap. No handlers preserves existing execution; empty callbacks do not add a budget gate. Hosts own UI, credentials, resources, cancellation cooperation and callback side effects; there is no approval service, plugin loader or grant cache.

All accepted model-visible contributions are actual append-only `extension` history with source `{stage,handler_id,operation_id,step,attempt?}` and exact saved wire JSON. BeforeTurn uses labelled developer instructions; BeforeRequest labelled user-role application data (not genuine human input); BeforeCompaction labelled developer instructions scoped to its summarization operation. Full labels are in [the extension-history reference](LIBRARY.md#extension-history-and-authority). No silent role fallback, transient-only text, changing system prefix, deduplication, output cache or replay of old callbacks. Repeated text appends repeatedly. History/search/portable summaries expose truthful source; inspection counts actual retained contributions without running hooks. Provider initiator and protected-user selection use genuine canonical sources, not wire roles or replayed protected users.

BeforeTurn runs until initial preparation acceptance, whose durable flag also records empty success. Unaccepted private candidates may change after restart. BeforeRequest runs once per new conversational preparation (including after process loss), not again during frozen live retries after normal compaction/guidance preparation, followed by one final guidance drain. Nonempty new contributions must fit the whole input (base/tools/summary/protected user/history/guidance/all candidates) under existing estimates/budgets. Reject oversized candidates without saving/clipping/retrying them, with contributor IDs and actionable context_limit. Accept history/source/audit references atomically before sending and freeze payload/initiator for transport/auth retries. Accepted entries remain after later failures, even without delivery; successful unused candidates do not become false callback failures.

BeforeTool waits while planned, after availability checks, before tool.started. Every handler must explicitly allow; zero/contradictory decisions fail. Denial saves a tool_denied result, no duplicate user note. Callback failure latches the originating hook_failed, cancels a batch child context, stops queue admission and joins started calls without cancelling the session context just to stop siblings. Completed effects, success after cancellation and acknowledged background launches survive. Intent is serialized with Interrupt, then ctx is checked before Run; known skipped execution is not uncertainty. Arbitrary host code cannot be forcibly killed or guaranteed effect-free after that check.

BeforeCompaction runs only after ordinary preparation/no-work checks with a frozen cut/model/account/prompt. Denial stops/pauses automatic or manual work, never silently skips. All-allow additions are saved after the frozen cut and used by the summarizer without copying them into its base prompt. Input-fit and resulting-main-fit include contributions; main-fit also reserves pending BeforeTurn text. Accepted entries survive request/validation/install failure; the previous installed selection remains. Manual compaction does not call BeforeTurn. Compaction denials are private operation errors, not transient model notes.

One diagnostic hook.result contains reached decisions, accepted history references, direct stage/handler failure or mixed-budget contributor IDs, not repeated content/timing traces. Empty text success creates nothing. Required writes use fail-stop; callback errors/reasons must be public-safe. Inspection, command/event replay and live retries never invoke hooks. Opted-in process recovery uses current callbacks: accepted BeforeTurn stays done; unfinished BeforeTurn, request/compaction preparation and uncertain tool guards may repeat. See [the library hook reference](LIBRARY.md#lifecycle-hooks-and-host-extensions) for exact public contracts and limitations.

## Human-owned file viewing

Provide a read-only library operation and thin HTTP route for an explicit human
file open, including links in saved chat messages. Resolve relative paths using
the owning chat's workspace on the connected backend; accept absolute paths
outside it and ordinary symlinks. This is trusted-owner OS access, not a sandbox:
readable sensitive files are allowed when deliberately opened, and contents are
not redacted status metadata. Preserve existing Host/Origin/CORS protections.

Read regular UTF-8 text only, reject binary control bytes and special files
without blocking on FIFOs, and bound capture with a per-open `limit_bytes` option,
defaulting to **8 MiB**, entirely independent of model tool limits. Zero/omitted
uses the backend default; positive integer bytes select the budget; negative,
noninteger and overflowing values fail clearly before I/O. No hidden absolute
product cap is added beyond representable/operable values. This is a full-file
viewer: no paging/load-more. Oversized reads fail with no partial content or
archive; changing the viewer control alone does not read, and explicitly reopening
with a larger limit never changes agent settings or needs idleness/reload/restart.
Markdown is rendered safely with original Source available; JSON formatting
preserves original tokens (including large numbers and duplicate keys). Format
failure or expansion beyond the selected render budget visibly warns and retains
complete Source. Text/logs/YAML/TOML/code remain readable monospace source; HTML
is source only, never executable. No file mutation, model calls, tool execution, history/events
or activity changes. Work/Stop do not wait for an idle chat or locks across disk
I/O. Concurrent file edits are not transactional snapshots. Clients open only on
user action, render inert text/Markdown without embedded resource fetching,
never send contents to inference, and keep async reads scoped to connection,
chat and requested file. See [library details](LIBRARY.md#human-owned-file-viewing)
and [HTTP contract](SERVER.md#read-only-files).

## External control and streaming

Expose equivalent library operations and remote endpoints for:

- Session creation, listing, inspection, renaming, and idle-session deletion.
- Starting a turn, steering it, interrupting session work, and requesting context compaction.
- Listing or clearing all pending steering, and explicitly continuing after interruption or failure. No per-message editing or deletion.
- Reading/exporting history and streaming/replaying session events from a cursor, including tool inputs, results, and retained output.
- Inspecting active turns/tools, compaction state, effective system instructions, context estimates, and per-turn/session usage, with compaction usage distinguished.
- Reading/updating session configuration: provider/model, reasoning options, appended instructions, enabled tools, tool concurrency, and compaction provider/model/prompt/settings.
- Listing the named providers/models and declared capabilities from configuration, plus available tool names and schemas. A separate provider discovery read returns current upstream IDs without changing this installed catalog.
- Starting, inspecting, and cancelling subscription logins; listing logged-in accounts; logging out. Never returning tokens or private device codes.

Apply runtime reload only while **every session in the runtime is fully idle**, and session-override replacement only while **the target session is fully idle**; otherwise return a conflict without queueing or interrupting work. Include stopping workers and immediate follow-up handoffs in the busy check. Ordinary override-based main model/provider changes additionally require a session with no accepted messages. Used sessions instead require explicit `SwitchModel`, preserving canonical history and projecting earlier protocol-specific items as read-only text for future requests. Missing catalog entries do not prevent inspection/reload of stored sessions, but cannot execute. Other idle-session settings, including supported reasoning options and compaction model selection, remain configurable. Configuration responses never reveal credentials. Remote configuration selects host-configured credential references, not secret values. Tool implementation registration remains a Go host API, not a remote code-upload endpoint. Expose server health/readiness separately from agent operations.

One host-local typed `Runtime.Reload(ctx, Config)` validates/prepares the full replacement outside runtime/SQL locks, then atomically publishes it while admission is excluded. It covers shared prompt contents, provider/model settings and tool implementations. `UpdateConfig` instead shares the target-only preparation path with `ReloadSession`, replacing typed explicit overrides and refreshing only that chat's resources. It leaves other chats and the shared setup cache untouched, does not exclude ordinary execution admission during file reads, and rechecks target idleness and model/account binding before publication. Absent/null override fields inherit; explicit empty/false/zero/list values remain explicit until removed. Shared defaults update existing inherited settings; legacy full configs migrate conservatively to explicit choices without guessing original intent or changing history. GET returns effective settings and overrides separately. Bad preparation preserves installed state. No-op reload preserves model-visible text/tool order; actual changes may affect caching.

`UpdateModels`/`UpdateModelsAndSave` publish only model definitions for new work,
without an idle requirement or an affected-chat scan. Running turns and manual
compactions capture just their main/summarizer definitions in memory, retaining
budgets/capabilities through lazy preparation and retries. No prompts, skills,
session settings, providers, tools, hooks or client resources are replaced.
Validate definitions on publication; validate latest selections/options/budgets
when admitting new work. Installation may leave an existing chat needing repair;
never redirect its identity/account/billing or rewrite settings to fit. Preserve
completed results and queue guidance if its next follow-up cannot run. Recovery
uses latest configuration, not persisted executable snapshots or a historical
registry. Save outside the state lock before coherent publication; cancellation
before save aborts, after save begins does not; real save/publication failure
seals admission and closes the runtime honestly. See [the full contract](MODEL-INSTALLATION.md#safe-publication-and-operator-edits).

`ReloadSession` separately rereads one fully idle session's configured append and compaction files, without changing settings/history or the shared path cache. Other sessions may run and retain their installed prompts. Read outside state/SQL locks and recheck target idleness before publishing; failure preserves old text. Commit the new prompts, `config.changed` event and deduplicated request receipt together. An accepted retry must not reread files. This is not host-config reload and cannot replace the compiled-in default prompt.

The executable calls the same reload operation on SIGHUP; there is no remote code loader or separate tool/hook reload endpoint. Database relocation needs Close/Open. Replaced HTTPClient/AuthManager/tool resources remain host-owned and are never closed by reload; acknowledged background processes survive. Bound-model/account safeguards remain. The same replacement includes [typed host hooks](LIBRARY.md#lifecycle-hooks-and-host-extensions).

Expose structured startup diagnostics and asynchronous process-lifecycle failures through a Go accessor and a thin HTTP endpoint. Keep credentials out of diagnostics.

### Presentation categories

Classify every user-facing event, history item, and diagnostic into one of three presentation categories: normal output, diagnostics/warnings, and errors. Keep the specific event kinds, codes, and scope; the category only tells a UI how to show the entry. It is not the turn's execution outcome: streamed output does not prove the turn succeeded, and a recoverable tool error goes back to the model without necessarily failing the turn. Answers, reasoning, and ordinary tool output are output; retry, compaction, interruption, and recovery notices are diagnostics; tool, provider, and configuration failures and model refusals are errors. A model refusal keeps its own kind and code (`response.refusal`, `model_refusal`), is shown and saved, is never a blank successful answer, and is never retried as a transport failure.

Use stable session/turn IDs and increasing per-session event sequence numbers. Events cover text, optional reasoning, tool start/output/result, steering state, compaction, configuration changes, model-request retries, usage, and turn lifecycle. Batch small deltas for storage efficiency. Commit events before publishing them.

SSE reconnects replay events after the supplied cursor. Clients deduplicate by sequence number. Attached subscriptions read bounded event pages from SQLite and poll about every 100 ms when caught up; no separate wake-up notification mechanism. Polling stops with the subscription, and no subscribers means no polling. A slow or disconnected subscriber must not block the agent or cause unbounded buffering; disconnect stalled network subscribers and let them replay. Persist events until session deletion in v1.

Mutating remote requests use client-generated request IDs so retrying after a dropped connection does not create duplicate turns or steering messages.

Client connection lifetime must never be the cancellation context for an agent turn. Cancellation is an explicit operation.

## Persistence and recovery

Use SQLite transactions and versioned schema migrations. Make the database file path configurable, independent of the workspace. Keep it on local disk; do not share it over a network filesystem or sync a live database between machines.

For v1, one runtime process owns a database and can run many sessions. Enforce exclusive runtime ownership for that database file and fail clearly if another runtime tries to open it for operation; a different data directory must not bypass this check. Multiple clients share sessions through the owning service, not by independently opening its database. Independent runtime processes use separate database files.

Shutting a runtime down is one operation that every close caller joins: seal admission, cancel/join workers while suspending eligible executions, stop owned background processes, then close SQLite and release ownership. Close is not a durable user stop; Interrupt saves stop intent before signalling and never auto-resumes. A caller whose own deadline expires gets a clear incomplete-close error while that operation continues. Normally ownership is released after cleanup; if exit observation fails and process ownership becomes uncertain, `Close` returns `process_observation_failed`, does not signal further, and releases storage only after the owner has relinquished the group. That is reported as incomplete cleanup, never success. A later close call waits for the same result.

SQLite supports multiple local processes, but sharing active sessions also requires execution ownership, command routing, and recovery coordination. That multi-process execution model is deferred, rather than implied by allowing a configurable database path.

Persist history and tool lifecycle boundaries as work progresses, not just at turn completion. Every required write of accepted work goes through the same fail-stop path: a failure stops admission and scheduling, cancels active work, and surfaces an error; readiness reports it. Lock contention while accepting a new command is different: nothing was accepted, so return `storage_busy` (HTTP 503), keep the runtime healthy, and let the caller retry the same request ID. Write transactions take SQLite's write lock when they begin (`BEGIN IMMEDIATE`), so a command waits up to 5 s for another connection's lock before reporting contention.

Open the configured database path through an escaped SQLite URI so the file SQLite opens is the file the ownership lock protects, even with `?`, `#`, or `%` in its name. Refuse a database file with multiple hard links. Read-only queries use the caller's context; accepted work never does.

After restart, Open loads latest configuration and reconciles progress without launching work. Explicit overrides and main-model binding survive. `Runtime.Resume(ctx)` admits all suspended executions with the same IDs, acknowledging that **external tool/hook effects may repeat, twice or more**. Cancellation before admission prevents worker launch; a successful admission commit transfers work to the runtime. Resume does not guarantee a strict return deadline: cancellation may wait for an in-progress SQLite writer-lock wait to finish or reach its existing five-second busy timeout. Stock `server.resume_on_start` defaults false; enabling it logs this warning and calls Resume after listener setup. It is startup-only, not SIGHUP-mutable, and there is no HTTP Resume route. Without opt-in, inspect work or Interrupt it before a deliberate new Continue. Independent idle sessions remain usable; suspended work blocks same-session new work/deletion and global reload.

One private executions row owns the six next actions: prepare/request/batch/compact_prepare/compact_request/stop. A single model-progress record retains four total reserved attempts, retry deadlines and the one 401 refresh cycle. Memory-only prepared requests keep live retries frozen; recovery prepares again with latest settings/hooks. Completed results and next actions share transactions, including final answer/follow-up and summary installation/parent handoff. Saved stop wins every late transition while preserving known data. Sessions.phase is derived from execution plus worker ownership, not stored separately.

Use one shared unfinished-attempt settlement for crash, clean Close and terminal stop/failure. Stop/drain live delta capture first, then atomically end the attempt, add at most one note from committed main text/refusal/reasoning deltas, clear in-flight state and save the next/stop action. Repeated Open creates no duplicate note/end event; completed responses get no partial note and summary text stays private. Events expose Step/Attempt so replacement streams are distinct.

Known saved tool outcomes never rerun. On recovery, unfinished replay-unsafe calls (including ones that never started) receive normal paired `recovery_skipped` results: `not_executed` for never-started calls, `uncertain` for unsaved possible effects. Built-in bash/write/edit set `Tool.ReplayUnsafe`; custom tools opt in. The restriction saved at planning or a stricter current definition prevents replay, without preserving an old executable configuration. Other unfinished calls use current availability/BeforeTool and original arguments, with warnings about earlier uncertainty. The same batch executor settles those calls and presents all results to the next ordinary model request; no human checkpoint or alternate recovery conversation is introduced. Fresh model-issued unsafe calls remain allowed. See [recovery](LIBRARY.md#recovery) for policy and history references.

Nil Tool.Run means known success even after cancellation. Other ordinary errors mean known failure; cancellation caused by shutdown must wrap ctx.Err/context.Cause for errors.Is identification to leave invoked work uncertain. Explicit overflow/process-observation failures remain known. Replay-allowed tools and hooks may repeat; no exactly-once guarantee or arbitrary repeat cap exists. Saved PIDs are never adopted/signalled; unfinished built-in background launches are skipped, not relaunched.

BeforeTurn acceptance (including empty success) survives restart. Lost request/compaction preparation and unfinished BeforeTurn can repeat with different current callback output; old accepted text is immutable canonical history and genuinely new identical contributions append again. Auto compaction has one durable opportunity per model step, including a decision not to compact; a pending summary keeps ID/budget and may reselect, while an installed summary is never repeated because private candidates were lost. Whole-turn account pins survive live steps/tools/compaction; genuine reopen repins latest login without resetting attempts or refresh allowance.

Startup conservatively interrupts unfinished records without recovery checkpoints; it never guesses their next action. Completed history, receipts and known outcomes remain. See [the recovery API](LIBRARY.md#recovery) and [execution ownership](DESIGN.md#5-persistence-and-recovery) for the current contract.

## Deployment boundary

Single-user, trusted-machine deployment only. Bind to loopback by default, reachable over an SSH tunnel through Tailscale. Explicit binding to a Tailscale address is allowed when tailnet access rules restrict access to the user's clients. The executable refuses any other listen address; the library opens no listener.

Limits: HTTP request bodies 8 MiB (configurable under server settings); provider SSE events 32 MiB, bounding the assembled event (configurable runtime setting; oversize fails descriptively without retry); SSE write deadline 30 s (configurable); header read 10 s; heartbeat 15 s; shutdown grace 10 s; bounded pagination (sessions/turns 200, history/events 500, larger requests clamped). Provider diagnostics are redacted before being shortened, and shortening is marked. Zero numeric settings mean defaults; output reserves that leave no usable context are rejected.

Anyone able to control the service effectively has shell access as its OS user. Tailscale access controls are the v1 access boundary, not an agent permission system. Authentication for public hosting and multi-user isolation are out of scope.

## Out of scope

A production chat/TUI application (the local `example/` is an interactive runtime example, not a production multi-user application), Swift bindings, ACP, MCP, cross-machine delegation, skill package installation/slash commands, browser tools, semantic search, multi-user hosting, and multiple runtime processes operating the same database. Subscription-backed chats support an explicit idle model/account handoff; account pools or automatic fallback, concurrent multi-process/multi-machine auth-store sharing, arbitrary-program credential import, automated browser control and HTTP secret transfer remain out of scope.

## Acceptance criteria

1. A Go app can create a session, stream a tool-using turn, and continue it after reopening storage.
2. A remote turn continues after the client disconnects; reconnecting replays missed events in order.
3. Steering is acknowledged durably and applied at the next model-request boundary, or automatically starts a follow-up turn after normal completion, including completion/steering races.
4. Interruption stops only that session's current work, pauses pending guidance, and leaves the host process, other sessions, and the interrupted session usable.
5. Disabled tools cannot execute; edit validation failure leaves the target unchanged.
6. Restarting preserves suspended operations without launching them. Explicit Resume/server startup opt-in may repeat unknown effects through current policy, never saved outcomes; Interrupt remains terminal.
7. Appended system instructions and supported reasoning options reach a Responses-compatible endpoint correctly; fragmented/interleaved events, function results, failures, and required reasoning items round-trip correctly.
8. Duplicate request IDs do not create duplicate work; slow clients do not stall runs.
9. Automatic and explicit compaction honor custom prompts and separate provider/model settings, preserve original history, and survive restart. Failed or cancelled compaction never installs a partial summary.
10. A configurable database path works independently of the workspace; a second runtime cannot operate the same database while its owner is running.
11. Tool calls can overlap and results remain associated with their call IDs. Built-in reads may overlap; writes/edits hold one runtime-wide exclusive lock. Interruption cancels active/queued work correctly.
12. Expanded control operations behave consistently through Go and HTTP APIs; configuration changes respect their stated boundaries, including model-only catalog publication alongside active work, and never expose credentials.
13. Output beyond 256 KiB fails only the producing tool call with descriptive guidance, stops further capture, and never appears as a successful truncated result. The model can continue with a narrower operation.
14. One configuration file can declare multiple named models/providers; different sessions and compaction can select different entries through the same client and captured per-model codec without catalog discovery. Compaction input that cannot fit fails clearly without chunking or additional summarization requests.
15. Manual compaction during active work returns a conflict and schedules nothing. Clearing pending steering removes all still-pending guidance without altering applied history or stopping active work.
16. Terminal automatic compaction failure stops the turn without a new summarization attempt; retryable transport failures use the shared retry budget. Completed steps from a single long turn can be compacted while retaining the latest user request and valid tool-call/result groups.
17. Manual compaction preserves the pause state on success; only unpaused pending steering starts follow-up work. Failure/interruption never starts a turn.
18. The first accepted conversation message locks the main model, including across restart, config alias changes, message clearing, and compaction. Compaction-model changes through `UpdateConfig` remain allowed when the target session is fully idle.
19. One all-sessions-idle reload applies shared defaults, prompt contents and tool/model configuration coherently to existing/new sessions while preserving explicit overrides. Override removal resumes inheritance. Invalid candidates retain previous installed state; workers and inspection never reread prompt files. Legacy migration preserves saved choices/history.
20. Retryable model errors allow at most three retries after the initial request, honor 429 Retry-After or use 1/2/4-second delays, and remain interruptible without replaying completed tools. Permanent errors and stream failures after published answer content are not retried. More than four sessions can run without a library session-capacity rejection.
21. After compaction and restart, the agent can find an exact earlier detail with `history` search (Go regex, at most 10 entries per page, stable cursor pages, own calls excluded from search but readable, no encrypted data) and read its original context by sequence number; an oversized read fails with advice to narrow it.

22. SIWC browser authorization validates callback/PKCE/RS256 identity/scopes and issued-client reuse; Copilot device login honors pending/slow_down/expiry. Status exposes no credentials and browser transients never enter receipts. Accounts survive restart in auth SQLite.
23. Concurrent expired requests share one refresh while other accounts progress. Rotation commits before use; logout/export/Close cancel and join owned work without reactivation. Interrupted sign-out/export remains locally unusable, reports remote/transfer uncertainty and never revokes a later login.
24. When the target session is fully idle, `SwitchModel` can explicitly switch its account after its model is locked; a running turn and its retries keep their account; only Copilot's 401 cycle forces refresh once; a rejected saved history fails with `history_incompatible` and pauses with history intact. API-key providers behave as before.

25. Standalone discovery is stateless, account-explicit and bounded; plan blocks gate every new inference attempt across shared runtimes and survive restart; explicit retry receipts cannot clear later blocks. Protected moves and import deduplication preserve registrations and history without silently redirecting endpoints.

26. All four host hooks honor detached multiple registration, first-stop decisions, append-only exact attributed history, combined acceptance/fit, frozen live retries, latest-callback recovery with durable initial acceptance, cancellation/known effects, source-based initiator and all-idle replacement. Thin HTTP forwarding shows history/source/audit/context without a second hook engine.

These are product requirements, not a mandate to maintain one automated test per scenario. The unit-only testing policy does not weaken these behavioral contracts.

## Compaction and prompt-continuity examples

These examples describe current behavior, not a future implementation plan. Preserve ordinary append-only conversation behavior; do not add clever reuse or deduplication machinery.

### Keep the recent conversation, with N treated as a minimum

```text
Before:
[system prompt + tool definitions] [append_system]
[older conversation] [user_message] [assistant]

After:
[system prompt + tool definitions] [append_system]
[summary of older conversation] [user_message] [assistant]
```

If the most recent N tokens contain only the assistant message because that message is N + 1 tokens, keep the **whole assistant message and the preceding user message**. Retain N + some tokens. N means **“save at least the last N tokens”**, not exactly N and not at most N.

Use the library's conservative token estimate, not a claim that it equals the provider's tokenizer. N measures recent conversation content, excluding the system/tool prefix and replacement summary. Keep whole messages and expand backwards for required tool-call/result and reasoning groups. If less than N is available, retain what is available; do not fabricate padding.

### The very first user request can have an older middle

There does not need to be a completed earlier user turn. One request can generate a long series of model/tool steps before its final answer:

```text
User: implement this feature             ← the first user message
model/tool steps 1 … 80                  ← older middle
model/tool steps 81 … 100                ← recent tail
Assistant: final answer, when finished
```

Assuming the recent tail meets N after whole-group expansion, compaction may happen **during that first turn**, before the final answer:

```text
[system prompt + tool definitions] [append_system]
[summary of older/intervening work]
[protected original user: implement this feature]
[recent complete model/tool steps]
```

The summary can use the original request as context, but the exact original user entry is also retained separately. Neither its age nor being the first message disqualifies it. Compaction does not wait for a second user message.

### The long-tool-loop edge case: retain the user plus recent tail

Suppose the conversation is:

```text
User: implement this feature
100 model/tool steps
Assistant: latest response
```

When the last N tokens contain only the final few steps, retain the initiating
user message plus the recent N-token portion and summarize the intervening older
steps. Do not retain the whole turn just to keep its initiating request: that
would make long tasks impossible to compact.

The resulting main-model input is:

```text
[system prompt + tool definitions] [append_system]
[summary of older/intervening work]
[protected user: implement this feature]
[recent complete model/tool steps covering at least N estimated tokens]
[latest assistant response, if not already included in that recent portion]
```

The assistant response is part of the recent suffix, not an extra duplicate. If the necessary user message is already in that suffix, do not insert a second copy. Preserving a user outside the suffix adds its size on top of N; boundary expansion may add more. Include all retained content in fit checks. If the result cannot fit, fail clearly rather than violate the minimum, split a tool group, silently remove the protected user, or retry summaries indefinitely.

### Fix the retained selection at compaction, not at every request

1. Walk backwards over complete model-visible history items until the recent suffix reaches at least estimated N (using projected sizes, so non-encrypted reasoning that is not replayed contributes zero). Expand backwards for intact tool/reasoning groups, never forwards just to fit under N.
2. Identify the real user/steering message preceding the earliest retained exchange when needed. Extension messages using a user-shaped wire format are not human anchors. Keep a real user already in the suffix once; otherwise retain a reference to that exact original item separately.
3. Summarize eligible older/intervening work, carrying forward the previous summary as appropriate. Original database history stays intact. Do not pull every intervening tool step back into the retained suffix just to reach the user.
4. Validate and install the summary together with the fixed cut and protected-user reference. Fit validation and later model input use the same selection. A nullable history-sequence reference on the compaction record avoids copying the user text or introducing a cache ledger.
5. Use that selection until the **next successful compaction**. New messages append after it. Do not pick the globally newest user on every request and remove the previously retained user when new guidance arrives. Failed/cancelled compaction leaves the previous installed selection intact.

Do not regress to this disappearing-request behavior:

```text
Wrong, without another compaction:
[summary] [old user request]
    becomes
[summary] [assistant response] [new user request]

Required:
[summary] [old user request]
    becomes
[summary] [old user request] [assistant response] [new user request]
```

### Hooks append exactly what they return, including repeats

First turn:

```text
User: hi
Extension: sup
Assistant: response
```

Next turn, if the extension fires again and adds another `sup`:

```text
User: hi
Extension: sup
Assistant: response
User: next question
Extension: sup
Assistant: next response
```

**Both copies of `sup` remain.** If the extension returns nothing on the second invocation, only the first `sup` remains, still included in history. Never remove that first contribution merely because the handler did not return it again.

Do not compare, reuse, replace, collapse, move or deduplicate contributions. Do not skip a handler because it previously returned the same string. Deciding whether to repeat something is the extension's responsibility. sam-g saves accepted model-visible output in conversation order and reuses that saved conversation as ordinary chat history.

```text
[stable system prompt + tool definitions] [stable configured append_system]
[conversation, including every accepted extension contribution in order]
```

Hook text is not inserted into a freshly rewritten leading system block each turn. Roles and source labels preserve its meaning; they are not a cache-reuse or deduplication mechanism.

### Expected cache changes are allowed

Deliberately reloading changed base instructions or tool definitions genuinely changes the next request; that is fine. Actual compaction deliberately replaces older context; that is also fine. A no-op reload, a normal next message, a tool result, a restart or a handler returning nothing must not independently rewrite/remove the earlier model-visible prefix.

Prefix continuity makes provider caching possible; it does not guarantee hits across models, accounts, routes, eviction windows or provider policies. Removing a later item can reduce reuse from that point without invalidating an earlier unchanged prefix. Verify request continuity offline; do not claim measured cache savings without separately authorized provider evidence.
