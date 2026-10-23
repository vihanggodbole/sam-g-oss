# Model installation and changes within a chat

## One installed catalog, not a second registry

The stock server stores installed model definitions in the existing `models`
object of its JSON config (`~/.sam-g/config.json`, or `-config`). The runtime
receives that configuration normally. There is no additional catalog database,
background refresh, startup discovery, or copied conversation store.

Go `DiscoverModels` remains stateless and never installs anything. The stock host
keeps only the latest explicit discovery proposal per **provider alias + account
label** in memory so a browser can install exactly what it just fetched. Every
Discover click still makes a new catalog GET. The installed configuration, not
that temporary proposal, is restored after restart.

## Browser flow and HTTP contract

1. `GET /v1/providers/{provider}/models?account=work` returns `models` and, when
   the host supports installation, an opaque `snapshot_id`.
2. Optionally choose a positive **local context budget**. If omitted (or set to
   zero), the stock host uses each model's provider-reported context capacity. If
   a model's capacity is not reported, it uses a 128,000-token fallback. Then
   explicitly install:

   ```http
   PUT /v1/providers/{provider}/models
   Content-Type: application/json

   {"account":"work","snapshot_id":"...","context_tokens":16000,"confirm_empty":false}
   ```

3. The response reports `installed`, `usable`, and `removed` counts. Refresh
   `GET /v1/models` to see the configured entries. No model request is started.

`GET /v1/models` also advertises `catalog_installation` and `model_switching`.
Embedded HTTP hosts may supply `server.CatalogHost`; without one, discovery still
works but installation returns `unsupported`. The stock executable supplies it.

The snapshot PUT is a replacement, not an append operation:

- Replace only entries marked with `catalog_account` for that provider/account.
  A present empty account identifies API-key discovery; absent/null means manual.
- Use stable aliases `catalog/<escaped-provider>/<escaped-account>/<escaped-id>`.
  Manual alias collisions are rejected, never overwritten.
- Add/update returned IDs and remove missing IDs. Other accounts/providers and
  manually configured aliases stay untouched, as do unknown host configuration
  fields and unknown fields on unrelated/manual model entries.
- Keep every returned entry, including unavailable/unverified entries. Mark these
  `unavailable`, exclude them from executable choices, and display the reason.
  Select Responses when explicitly reported, otherwise supported Copilot Opus 5.5 Messages when reported. Missing/false support is not fabricated; Messages requires a reported maximum output. Unsupported Messages profiles remain unavailable.
- With no chosen budget, use each model's provider-reported context capacity;
  use 128,000 tokens only when that capacity is unknown. With a chosen budget,
  use it capped by provider-reported capacity when supplied. Pi reference values
  are not used as execution settings. Copy exact reported effort declarations. The exact Opus 5.5 native profile declares summarized/omitted thinking display separately from provider facts; Pi output limits are never installed.
  When discovered `reasoning_summaries` is missing/null, retain an existing
  `reasoning_summaries: true` on the same managed alias only if provider, account
  and model ID still match, discovery selects Responses, and the old entry uses
  Responses (including the legacy empty `api`). Explicit provider false overrides
  that declaration. New IDs, other scopes, manual aliases and incompatible
  transports do not supply a fallback. This preserves a local declaration, not
  proof of provider support; the exact Opus Messages profile above is unchanged.
  Generic API-key catalogs usually omit capabilities; verify/configure those
  models manually rather than infer support.
- Require `confirm_empty: true` for a successful empty catalog. Failed discovery
  never clears installed entries. There is no background or timed cleanup.

A newer lookup invalidates an older proposal for the same scope, even if the new
lookup fails. Other file changes/reload or a process restart require fresh
proposals. A repeated identical successful PUT in the same process returns its
original result without another publication. A stale/restarted proposal is rejected,
never replayed over a newer catalog. After a lost acknowledgement, inspect the
installed list or rediscover. This host action does not claim the library's durable
request-ID receipt contract.

Messages uses the existing output reserve as a hard requested response cap; each main/summarizer reserve must fit its verified output bound. Repair invalid settings rather than expecting an automatic clamp. See [native settings and history](LIBRARY.md#native-messages-through-copilot).

## Safe publication and operator edits

Installation publishes **only model definitions for new work**. No chat needs to
be idle, and there is no affected-chat scan, waiting queue or automatic watcher.
A running turn retains its main and compaction definitions in memory, including
budgets and capabilities, through lazy preparation, tools, later automatic
compaction and retries. Manual compaction also retains its admitted definitions.
This does not shield work from real provider/account failures.

`Runtime.UpdateModels(ctx, models)` replaces the model map without changing
providers, defaults, session settings, prepared prompts/skills, tools, hooks or
client resources. It validates definitions, not every existing chat. For example,
you can install a catalog that removes a chat's summarizer while that chat is
running: its current turn keeps the old summarizer; its next turn must repair
that setting. Missing definitions, incompatible reasoning options/budgets and
bound identity changes fail through the ordinary controls; no model/account or
billing fallback and no automatic settings rewrite occur. If pending guidance
cannot start a follow-up, the completed answer stays saved and the guidance stays
queued with a visible pause reason explaining the repair.

`Runtime.UpdateModelsAndSave(ctx, models, save)` adds host persistence. It
validates first, runs the host's save outside the runtime mutex, then publishes
under that mutex. Work and reads can continue on the old catalog until publication;
new work admitted before publication captures the old definitions. Other
configuration preparation cannot interleave. Cancellation is honored before
saving; once save starts, publication finishes despite browser/caller cancellation.
An explicit runtime Close still wins. A real save/publication failure seals
admission before releasing configuration ownership, then closes the runtime: the
file may contain the new models. Inspect storage and restart; do not claim an
unchanged file or continue with uncertain configuration.

There is no historical catalog registry or persisted executable configuration
snapshot. Restart/recovery loads the latest configuration, preserving saved
results/history, the main binding, account and billing rules. Missing or
incompatible latest definitions fail clearly; restore the configuration or
repair settings/use explicit model handoff before starting new work. An interrupted
operation is not promised its old executable environment after restart.

Shared defaults still undergo startup validation; model-only publication does
not validate them. For example, the installed `coding` model may still exist but
no longer declare `high`, while `defaults.model` is `coding` and
`defaults.reasoning_effort` is `high`. Installation succeeds and active work keeps
its captured definition, but on the next process start `Open` rejects that shared
default before the stock server can listen. The process exits with that startup
error, so its API and health endpoint are unavailable. This is not something the
browser can repair while the server is down. Edit
`defaults.reasoning_effort` in the stock server's `~/.sam-g/config.json` (or the
file passed with `-config`) to an effort supported by the selected model, or to
`""` to use the model/provider default, then restart sam-g. There is no
old-catalog fallback. A missing model alias is handled separately: it remains
unavailable/readable and does not trigger this option-validation failure.
Incompatible settings saved on an individual chat do not fail this shared-default
check: the server starts and the chat stays inspectable, but new work is rejected
with a repair reason. Recovery or a queued follow-up that cannot run pauses the
chat; a rejected new-turn request does not itself change the pause flag. After
the chat is idle, repair its settings through the chat controls; persisted choices
and history are retained.

Whole-runtime `Reload`/`ReloadAndSave` remain all-sessions-idle operations for
changes to providers, prompts, defaults, tools, hooks or client resources. Model
installation does not call them.

The stock host patches only model entries, writes a private sibling file, syncs,
renames, then syncs the directory. SIGHUP and installation share one file authority.
Detected external edits reject installation and require reload/rediscovery. Do not
edit the JSON concurrently with installation: ordinary editors do not participate
in the host lock. Automatic installation requires a regular config file, not a
symlink/FIFO. Read-only discovery still works with other host configurations.

## Missing models do not delete chats

A saved model/default selection can outlive its catalog entry. Reload/reopen keeps
those sessions and their histories readable. The session response exposes
`model_error` when its main model is unavailable. New work cannot use that missing
model; there is no fallback to another provider, model, account or billing mode.
New sessions must still select a usable configured model.

Ordinary config edits cannot redirect a used chat's model or account. The first
accepted input pins both selections into session overrides. Old used chats pin
their last saved choices when reopened, before new defaults are applied; omitting
the overrides cannot silently resume model/account inheritance. Renaming/repointing a
still-present bound alias cannot redirect the chat: its next work reports a
conflict (catalog-only publication itself does not scan/reject existing chats). Effective API is part of this identity: changing an alias from Responses to Messages requires the same explicit handoff. To change the model explicitly:

```go
settings, err := rt.SwitchModel(ctx, sam.SwitchModelRequest{
    RequestID: "unique-handoff-id", SessionID: sessionID,
    Model: "new-configured-alias", Account: "work", // empty for API keys
})
```

HTTP clients use `POST /v1/sessions/{id}/model` with `Idempotency-Key` and a JSON
body containing `model` and `account`. Only the target session must be fully idle:
no suspended executions, stopping workers or immediate follow-ups. Other sessions
may keep working. This is a normal durable receipt: retry the identical request
with the same key after a lost
acknowledgement, even after restart. An old receipt cannot undo a later switch.

The operation saves the new binding, explicit model/account overrides, a visible
model-change note, `session.model_changed` and `config.changed` events, and its
receipt together. Reasoning effort/summary requests reset to provider defaults.
Other settings—including compaction model/account—are retained and validated.
If an independently configured compaction model disappeared, repair that setting
first. Ordinary settings remain editable when the main model is missing or its
binding is already incompatible after reopen; unrelated repairs never authorize
that old binding to execute. Newly introduced binding redirection still rejects.
Account-scoped installed aliases require their owning account. Switching to
another account's catalog entry uses the same explicit handoff. Manual aliases
also use this operation for account-only changes: there is no alternate ordinary
account-edit path with different history rules. Browser clients should preserve
handoff ownership while it is pending, including when rebuilding settings forms.
Avoid silently discarding drafts or applying late responses to another chat;
Stop and navigation can remain independently available.

## History across the handoff

No messages, outputs, turn snapshots, or tool results are rewritten or removed.
A history-sequence boundary marks the handoff. After a switch,
older tool calls/results and exposed reasoning summaries are projected as labeled,
read-only assistant text for future requests; encrypted reasoning, native signatures and redacted blocks are not forwarded.
Ordinary message text and roles and the installed compaction summary remain context.
Unknown older provider-specific payloads stay stored but are omitted from future
requests, rather than forwarding opaque data across the handoff.
New turns use the new model's normal wire protocol. Switching back does not revive
opaque items from an earlier model/account generation.

A switch authorizes sending conversation context to the selected provider/account;
clients should obtain confirmation before submitting it. It is not a promise that every provider accepts
every old message or that a smaller model can fit the context. Normal provider and
context-limit errors remain visible; no silent role conversion, truncation, model
fallback, tool execution or inference occurs during the switch itself.
