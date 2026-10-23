# Native subagents

Scenario: a parent asks a backend reviewer and a test reviewer to work in parallel.
Each reviewer is an ordinary durable session on the **same Runtime/server**, with
its own history, steering, tools and events. Both can use the same workspace. No
worktrees, workspace creation, file reservations, merge management, edit ordering,
or collision prevention are added. The caller owns the consequences of shared
edits. Existing built-in file-operation correctness locks remain unchanged.

## Tool and selections

There is one `subagents` tool, offered only to root sessions:

- `{"action":"models"}` lists configured aliases, their providers and supported
  thinking efforts, plus safe account labels/status. It does not call a provider.

- `{"action":"start","name":"backend-review","prompt":"Review the backend"}`
  atomically creates the child and accepts its first turn, then immediately returns
  `{"session_id":"ses_…","turn_id":"turn_…"}`. It does not wait for completion.
- `{"action":"status"}` lists direct owned children. Add `subagent_id` to inspect
  one. Results include the ordinary session snapshot (phase, pause, model binding,
  workspace, model error, active work and `last_event_seq`), selected account,
  latest turn status/reason/error and `last_history_seq`. A previous saved result
  is not proof that current running/suspended work is finished.
- `{"action":"steer","subagent_id":"ses_…","text":"Check error paths too"}`
  uses ordinary steering: guidance waits for the next model request and does not
  rewrite an in-flight request. Idle/unpaused children can start a new turn.
- `{"action":"cancel","subagent_id":"ses_…"}` durably interrupts that child's
  current work, including its owned descendants, and pauses guidance. Siblings and
  unrelated sessions continue. Accepted cancellation is not proof cleanup ended;
  inspect status/events. Saved history and known effects are retained.

`start` optionally accepts `workspace` and `config`, using the existing
`SessionOverrides` fields rather than a separate settings system. For example:

```json
{"action":"start","name":"review","prompt":"Review the changes","config":{"model":"catalog/copilot/work/gpt-6-astra","account":"work","reasoning_effort":"medium"}}
```

The installed alias selects the provider—there is no arbitrary provider-URL
routing path. Use `models` to inspect supported choices rather than guessing.
When choosing a different model, inherited reasoning effort/summary requests and
output reserve reset to provider/built-in defaults **before** explicit overrides
are applied. Other omitted settings inherit the parent; independent compaction
model/account or instruction choices can also be overridden through `config`.
Unsupported explicit thinking efforts fail rather than silently downgrade.

Disabled tools are additive at creation: a child cannot enable tools its parent
disabled. `subagents` itself is always disabled and omitted from child prompts/tool
schemas. The library rejects nested starts and ordinary attempts to re-enable it.
This permanent child role survives restart and parent deletion. These are native
API/tool rules, not an OS sandbox: Bash and host APIs remain trusted-user authority.

A different `config.model` requires explicit `config.account`: a login account,
or `""` for an API-key model. There is no provider/account/billing fallback. Existing
workspace paths must be absolute directories; omitted workspace uses the parent.
Child history starts with its task, not a copied parent conversation. Include the
relevant context in the task. Configuration preparation can return the ordinary
transient conflict; retry the same request after it finishes.

## History and progress

Use **the existing `history` tool**, not a separate logs action:

```json
{"action":"search","subagent_id":"ses_…","query":"(?i)result|error"}
{"action":"read","subagent_id":"ses_…","from_seq":1,"to_seq":8}
{"action":"search","subagent_id":"ses_…","view":"events","query":"model.retry|turn.failed"}
{"action":"read","subagent_id":"ses_…","view":"events","around_seq":24,"before":2,"after":2}
```

Omitting `subagent_id` reads the caller's own session. A supplied ID must be a
**direct child of runtime-supplied `ToolCall.SessionID`**, not an arbitrary session,
sibling or grandchild. Caller arguments cannot supply a parent identity or SQL.

`view` defaults to `history`: the same visible saved history as the library API,
including compacted entries but never opaque/encrypted reasoning. `events` uses
the ordinary visible Event envelopes, including live progress, retry timing,
provider failures and uncertain tool attempts; it does not expose raw provider
wire output. Event search matches the envelope's JSON; event reads return
`{"entries":[Event…]}`. History reads return `HistoryItem` entries in that envelope.

Both views share bounded scans, 10-match newest-first search pages, Go/RE2 queries,
inclusive `from_seq`/`to_seq` reads and existing `around_seq`/neighbor counts. Search
skips history-tool calls/results; reads include them. Each view has its own seq
space. Cursors belong to the selected session, view and exact query; switching any
requires a fresh search. Ranges extending past the end return available entries;
a nonexistent `around_seq` fails. Event reads snapshot their upper bound before
writing output, so a tool never follows its own new output indefinitely. Oversized
reads fail under the ordinary output limit; narrow the range. No log archive,
search index or SQL interface is added.

## Completion, stopping and restart

A child outcome is terminal when its latest turn ended and its executor has no
remaining work after ordinary steering follow-up handoff. A completed response
with queued follow-up guidance is **not** a completion notice. This boundary is
serialized with steering acceptance. Guidance accepted after quiescence is new
work and may produce its own later notice. A child session can be used repeatedly.
Children cannot delegate further, so there is no recursive task-group join to
coordinate. Manual compaction alone does not create another task-completion notice.

At quiescence, the runtime saves small ordinary steering text on the parent:

> [notification] Subagent backend-review completed. Session: ses_…. Read its history for the result and events for diagnostic details.

Failure/cancellation notices name the child and terminal outcome too. Full output
and diagnostics stay in child history/events. This is ordinary steering text,
not a new message-source/provenance scheme or notification inbox.

- Active parent: queue the notice for a safe model boundary.
- Idle, unpaused parent: accept one ordinary steering turn.
- Paused/stopped parent: retain the notice without restarting it.
- Suspended parent: retain it with the saved execution; Resume admits that original
  work, not another parallel parent worker.
- Unavailable parent model: retain pending text without trying another model.
  Repair the selection using ordinary model controls, then explicitly Continue.
  Authentication/provider failures after acceptance use normal failed-turn/pause
  behavior; the notice remains in saved history.

`Interrupt(parent)` pauses/stops the entire owned descendant tree in one
transaction before signalling workers. Idle parents can still own active children,
so Stop remains useful while the parent's own phase is idle. `Close` instead
suspends eligible parent/child executions; shutdown is not user cancellation.
Browser disconnection stops neither. Acknowledged background Bash processes keep
their existing separate lifetime rules; Interrupt does not undo external effects.

Child creation, first-turn acceptance and the normal request receipt commit
**together**. The tool derives its stable request identity from caller session,
turn and call IDs, excluding retry attempts. Restart replay rejoins the existing
child even if the original tool result was never saved. General unknown tool
attempts still expose the existing warning about possible earlier effects.

Completion uses `sessions.parent_id`, a latest-notified-turn marker, and existing
request receipts/steering transactions. A separate immutable `is_subagent` flag
preserves the child's non-delegating role when the live parent link is cleared. The marker, notice, receipt and
optional parent turn commit together. Recovery checks quiescent children to close
the crash gap after terminal save; it does not duplicate a saved notice/follow-up.
Newly accepted recovery follow-ups stay suspended until the existing `Resume`
policy admits them. Open makes no inference calls. Unavailable children with
unapplied unpaused guidance retain it and do not falsely report completion.

The library rejects parent deletion with `children_busy` while any owned child
has unfinished work, including suspension or a worker still settling. A client can
offer **Stop children, then delete** by requesting interruption, waiting, and
retrying the same guarded deletion. Leaving a view should stop waiting, not undo
accepted cancellation. No browser-only check can bypass the library's atomic guard.

Once deletion is allowed it removes only the parent; child histories remain with
no live parent link and retain `is_subagent: true`. Detached children still cannot
delegate or re-enable that tool. Cancelling agents does not undo edits or alter the
existing lifetime rules of already-acknowledged background Bash servers. No file
coordination, automatic workspace cleanup or cross-machine delegation is added.

## Library, HTTP and browser

- Go: `StartSubagent(ctx, StartSubagentRequest)` → ordinary `TurnAck`;
  `Subagents(ctx, parentID)` → `[]Subagent`; `Session.ParentID` exposes live linkage
  and `Session.IsSubagent` preserves the permanent child role.
- HTTP: `POST /v1/sessions/{parent}/subagents` accepts
  `{name?, prompt, workspace?, config?}` with `Idempotency-Key`, returns
  202 + `TurnAck`. `GET` on that path returns `{"subagents":[…]}`.
- Thereafter use existing session, history, events, steering and interrupt APIs on
  the child's session ID. The trusted-user server is not a multi-user ACL system.
- Browser clients can show each child's phase/latest outcome, model, account,
  workspace and errors using these same APIs. Keep history, guidance and
  cancellation controls scoped to the child's session. Drafts and late replies
  should remain with their originating connection/session rather than whichever
  chat happens to be selected when a response arrives.
