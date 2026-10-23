# sam-g server

`sam-g` is a small HTTP service that hosts the sam agent library. It lets you start and watch agent work on a remote machine from any client. All agent behavior (turns, tools, steering, compaction, storage, and recovery) lives in the library. See [LIBRARY.md](LIBRARY.md). The server translates HTTP into library calls and streams events as Server-Sent Events (SSE).

## A short scenario

1. On a Linux box, `sam-g` runs under systemd and listens on `127.0.0.1:8080`.
2. From your laptop, you open an SSH tunnel over Tailscale, create a session for `/home/me/project`, and start a turn: "run the tests and fix failures".
3. You watch `GET /v1/sessions/{id}/events`: model text, tool calls, and command output arrive live.
4. You close the laptop. The request ends, but the turn keeps running on the server.
5. Later you reconnect with `Last-Event-ID: 42`. The server replays events 43 onward, then keeps following.
6. You steer ("also update the changelog"), or interrupt, then continue.
7. You ask for a preview. The agent starts `npm run dev` as a background process with a log file, checks it with `curl`, and tells you the port. You forward that port from your laptop (see [Previewing a dev server](#previewing-a-dev-server)).

For a small local page with ChatGPT sign-in and chat, start with [`go run ./example`](../example/README.md). It embeds this same HTTP API and serves the page on one origin. The stock executable below remains an API-only host.

## Build and run

Requires Go 1.27. The server runs on Linux and macOS, and needs `bash` for the
bash tool. From a repository checkout, use [`scripts/install.sh`](README.md#quick-start)
to build the binary and create the example config, then edit that config and
start the server with `scripts/run.sh`. The run helper supports `--bg` and the
opt-in `--tailscale-service [NAME]` flow; see the [quick start](README.md#quick-start)
for confirmation, per-machine naming, admin prerequisites, and safety limits.

The executable itself stays in the foreground, logs to stderr, and never
daemonizes. To build for a remote machine from a Mac:
`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o sam-g-linux ./cmd`. The SQLite
driver is pure Go, so no C toolchain is needed.

## Configuration

`~/.sam-g/config.json` holds the library configuration plus a `server` section. Create it before starting; `-config /other/file.json` overrides this location. Omitted `database_path` uses `~/.sam-g/sessions.db`; an enabled `auth` section defaults its database to `~/.sam-g/auth/auth.db`. The host expands leading `~/` in the config filename and these database paths, while other explicit relative paths keep their working-directory meaning. Embedded library callers still supply explicit paths. Moving an existing installation requires stopping its owners first and moving the whole stores with their sidecars, never copying a live credential database. The executable reads it at startup and on SIGHUP for unified runtime reload. See [sam-g.example.json](sam-g.example.json):

```json
{
  "server": { "listen": "127.0.0.1:8080", "max_body_bytes": 8388608, "sse_write_timeout_seconds": 30 },
  "database_path": "~/.sam-g/sessions.db",
  "providers": {
    "openai": { "base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY" }
  },
  "models": {
    "coding":  { "provider": "openai", "model_id": "gpt-4.1", "context_tokens": 128000 },
    "summary": { "provider": "openai", "model_id": "gpt-4.1-mini", "context_tokens": 128000 }
  },
  "defaults": { "model": "coding", "compaction": { "model": "summary" } }
}
```

- `server.resume_on_start` defaults to false. Set it to true and restart to resume saved unfinished operations after listener setup. The server logs an explicit warning: **replay-allowed tools/hooks may repeat effects**; unfinished replay-unsafe calls are skipped with results for the agent, and known saved outcomes never rerun. See [recovery policy](LIBRARY.md#recovery). This is a startup-only choice, not SIGHUP-mutable, and there is no HTTP `/resume` endpoint. A Go host may call `Runtime.Resume(ctx)` directly.
- `server.max_body_bytes` bounds each JSON request body; 0 or absent means 8 MiB. `server.sse_write_timeout_seconds` bounds each SSE write; 0 or absent means 30 s. Restart to change them.
- `server.listen` defaults to `127.0.0.1:8080`. Only loopback addresses (`127.0.0.1`, `::1`, `localhost`) and Tailscale addresses (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) are accepted. `0.0.0.0`, `:8080`, and LAN or public IPs are refused at startup, because the API has no authentication.
- `api_key_env` names an environment variable. The key itself never goes in the file, the database, events, responses, or logs. `GET /v1/providers` reports only `has_credential`. The key is read from the server process's own environment when each model request starts. Editing a shell profile or the service's environment file does not reach a running process: restart the server (for example `systemctl --user restart sam-g`) and the next request uses the new value.
- Every other field is the library's `Config`. The library documentation covers model entries, reasoning options, and session defaults. Declare multiple providers and models under their maps. Sessions and compaction pick them by name and use each captured definition's API. Models expose `api` (`responses`, or initially Copilot Opus 5.5 `anthropic_messages`) and `max_output_tokens`; legacy omitted API means Responses. Discovery reports nullable `supports_messages`/`max_output_tokens` alongside truthful `supports_responses`, plus the selected supported `api`. See [native settings/history and hard caps](LIBRARY.md#native-messages-through-copilot).
- The optional `auth` section enables subscription logins (see [Subscription login](#subscription-login)). Without it, providers with `"auth"` are refused at startup and the `/v1/auth` routes answer `409 unsupported`.

## Subscription login

Scenario: sign in on the laptop whose browser receives the loopback callback, export a protected registration, transfer it securely, and import it into the stopped VM host. The VM then owns on-demand refresh. A VM's `127.0.0.1` is **not** the laptop's loopback. Copilot supports remote device-code login; OpenAI uses direct SIWC with a browser callback.

```json
{
  "auth": {
    "database_path": "~/.sam-g/auth/auth.db",
    "openai": { "agent_name": "sam-g" },
    "github_copilot": {}
  },
  "providers": {
    "chatgpt": { "auth": "openai-chatgpt" },
    "copilot": { "auth": "github-copilot" }
  }
}
```

Configure model aliases/budgets separately; see [subscription example](sam-g.subscription.example.json). OpenAI's base is exactly `https://api.openai.com/v1`. Explicit and token-derived Copilot origins both require HTTPS githubcopilot.com/subdomains. Copilot uses the bundled GitHub Copilot Plugin client ID and Pi-compatible headers by default; `auth.github_copilot.client_id` is an optional override. Arbitrary subscription proxy overrides remain forbidden. See the [compatibility profile](AUTH-MODELS.md#copilot-compatibility-profile) for exact values and intentional differences from Pi. Provider eligibility, entitlement and live inference acceptance are not guaranteed by the offline test suite; see [provider limitations](AUTH-MODELS.md#provider-limitations).

Auth SQLite is separate from conversation SQLite, exclusively owned, with an owner-only directory (0700) and files (0600). Tokens never enter conversation records/events/logs or ordinary API responses. Tools running as the same OS user can read the store; logical clearing is not forensic erasure of old pages/backups/bundles.

Local commands open only AuthManager, not a Runtime. Stop the relevant server first:

```sh
sam-g auth login -config laptop.json -account personal
# Add -enable-plan-usage only for the user's explicit plan-consent request.
sam-g auth export -config laptop.json -account personal -out /private/transfer/account.json
# Transfer over SSH or another host-owned secure channel.
sam-g auth import -config vm.json -account personal -in /private/transfer/account.json
sam-g auth accounts -config vm.json
```

Export is move-only and never revokes the session. Inspect deactivation/transfer status after lost acknowledgement; interrupted exports never reactivate the source automatically. Import rejects active/stale targets and preserves VM identity/plan blocking. Remove transfer copies after confirmation. There are no HTTP export/import routes. The [authentication guide](AUTH-MODELS.md) covers interruption, trust and ownership details.

The ordinary HTTP API exposes reusable login primitives, not a hosted browser callback. A local host binds its listener first and supplies RedirectURI to start; it forwards the full callback through CompleteLogin. Trusted browsers can call these primitives under the [origin policy](#browser-access-and-cors); only the dedicated callback handler accepts OpenAI's cross-site redirect. Stock `auth login` hosts the loopback listener without browser automation. SIWC browser attempts wait until cancellation/Close, while Copilot device attempts honor provider expiry. Accepted exchange/polling survives waiter disconnect; interrupted attempts do not resume through old keys.

Logout returns a structured local/remote result, not an unconditional empty success. The HTTP/CLI host caps remote waiting at ten seconds; accepted local clearing survives cancellation/restart. Review the warning when revocation is unconfirmed. A plan-limit failure gates new requests across runtimes sharing the manager; `retry-plan-usage` explicitly clears only that local block. Unknown reset timestamps remain null/omitted, never guessed from Retry-After.

```sh
B=http://127.0.0.1:8080
curl -fsS "$B/v1/auth/accounts"
curl -fsS "$B/v1/providers/chatgpt/models?account=personal" # one fresh catalog GET
curl -fsS -X DELETE "$B/v1/auth/accounts/personal" -H 'Idempotency-Key: logout-1'
```

**Using an account in a session.** Create a *new* session whose model is a subscription model and whose config names the account:

```sh
curl -fsS -X POST $B/v1/sessions -H 'Idempotency-Key: create-chatgpt-1' \
  -d '{"name":"ChatGPT work","workspace":"/home/me/project","config":{"model":"chatgpt","account":"personal"}}'
```

To switch a used subscription chat's account, wait until **that session** is fully idle and use the same explicit handoff as a model change. Keep the current model alias for an account-only handoff (or choose that account's managed alias):

```sh
curl -fsS -X POST "$B/v1/sessions/$S/model" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: handoff-work-1' \
  -d '{"model":"chatgpt","account":"work"}'
```

For other settings, GET config returns `{effective, overrides, resources}`; PUT replaces only the **override object**, not that envelope. Omitted/null fields resume inheritance, except the model/account of a used chat, which remain pinned. Explicit `""`, `false`, `0`, and `[]` remain choices; numeric zero selects the built-in default.

Ordinary config PUT cannot change a used chat's model binding, including its effective API. Use `POST /v1/sessions/{id}/model` with `Idempotency-Key` and `{"model":"new-alias","account":"work"}` for an explicit handoff, including across providers. History stays intact. See [model installation and switching](MODEL-INSTALLATION.md).

Old conversations are never silently redirected to SIWC. Missing models leave them readable. Explicit handoff preserves their canonical records and projects older protocol-specific items portably; provider-incompatible message roles can still fail rather than being silently converted.

## How settings change

| What | How | Restart? |
| --- | --- | --- |
| Session overrides (reasoning, instructions, tools, account, compaction) | `PUT /v1/sessions/{id}/config` with an override object when **that session is fully idle** | No. Atomically refreshes only that chat's settings and prepared resources; unrelated chats may run. |
| One chat's prepared prompt resources | `POST /v1/sessions/{id}/reload` with Idempotency-Key while **that chat is fully idle** | No. Refreshes only that chat; other chats may run. |
| Login accounts | `POST /v1/auth/logins`, `DELETE /v1/auth/accounts/{name}` | No. Credential refresh is independent of prompt reload. |
| Runtime providers/models/defaults and prompt-file contents | Edit the host config/files, then `kill -HUP <sam-g-pid>` | No. All-idle atomic reload; busy/invalid candidates are rejected in server logs, retaining installed state. |
| Database location, server listen/limits/resume_on_start, auth-manager credential-store/client settings | Edit the JSON file and restart | Yes. These are already-open host infrastructure. |

SIGHUP is the stock executable's host-local entry point to `Runtime.Reload(ctx, Config)`. The executable runs at most one reload preparation at a time; additional SIGHUP signals handled while it is in progress are logged and ignored, not queued. There is no HTTP runtime-config reload/code-upload route or remote function registry. The separate per-chat prompt refresh below only rereads that chat's configured files, workspace instructions and skill catalog. A custom Go host can replace typed tools and Hooks through the same method and pass the runtime to `server.New`. Functions are not JSON-deserialized.

Catalog PUT is different: it publishes only models for new work while chats may
run. Active turns/manual compactions retain admitted definitions; the next turn
may need settings repair. Restart/recovery uses latest configuration. If the new
catalog makes an otherwise-present shared default invalid, a later restart can
fail before the server listens; see the [operator repair and missing-model
distinction](MODEL-INSTALLATION.md#safe-publication-and-operator-edits).

Scenario: edit `append.md` during a turn. That turn, later turns, compaction and context inspection keep the installed text until explicit reload. For a global reload, wait for every session and immediate follow-up to settle, then signal again; busy reloads are not queued. To update just one idle chat, use `POST /v1/sessions/{id}/reload` instead. Preparation does not hold runtime/SQLite locks; reads can see the stable old view. Missing files reject reload/setup synchronously. Reload never stops acknowledged background servers or closes host handles.

Per-chat reload preserves settings, model/account selection, pause state and
history; the next turn/compaction uses the new contents. It publishes a
`config.changed` event. Missing files or a target that is busy at either admission
or publication leave its installed prompts unchanged. Other chats and the shared
setup cache are untouched. An accepted retry with the same key does not reread
files; use a new key for another refresh. This does not reread `config.json` or
the compiled-in `prompts/system.md`. Configure an external
`append_instructions_file` for editable rules; changing the built-in prompt's
source requires rebuilding/restarting. See [per-chat reload](LIBRARY.md#reload-one-chats-prompt-files).

Shared defaults update inherited fields. Once a chat is used, its model/account become explicit pinned choices; omitting them from a settings PUT or changing defaults cannot move the chat. Legacy full configs migrate to explicit fields conservatively; delete fields from the override object to opt into current defaults. History is unchanged. Reload refuses to redirect a bound main model/endpoint. Initial Open/new workspace setup prepares files through the same helper; an already prepared configured append/compaction path stays frozen until reload. Discovered workspace instructions and skills are freshly prepared for each new session.

The main model of a session binds when its first prompt or steering message is accepted. After that, changing `model` or `account` through ordinary config PUT returns `409 conflict`; the explicit handoff endpoint is required for both. Compaction settings remain editable while the target chat is fully idle. If startup config changes the provider from OAuth to API keys, a saved nonempty account is not ignored: main/compaction execution fails with `auth_required`, pauses, and keeps history. Restore OAuth or explicitly choose valid idle settings. A different endpoint/model requires explicit model handoff.

### Prepared chat resources

Config GET/PUT, per-chat reload receipts, and `config.changed` payloads include
`resources` from the installed session snapshot. Context inspection includes the
same snapshot and counts the catalog in its estimate. The fields are `tools`
(enabled names), `instructions` (`kind`, optional `path`), `skills` (`name`,
`description`, resolved `path`, `scope`), optional `diagnostics`, and optional
`skill_catalog` (the leading developer input text; absent when unavailable/empty).
A successful per-chat or host reload emits `reason: "reload"`; ordinary settings
replacement emits `reason: "update"`. Older events need not have a reason.
Clients can show “Chat setup” and “Configuration reloaded” using these resources
without scanning files or adding model messages. Failed preparation leaves old
resources installed; a lost acknowledgement is uncertain until reconnected.

Configure extra roots using top-level `skill_directories`; see the authoritative
[source, precedence, path and snapshot rules](LIBRARY.md#workspace-instructions-and-skills).
These paths and diagnostics describe the server machine, not browser-local files.

## API

Every route is under `/v1`, except the health checks. Request and response bodies are JSON. A request body must be exactly one JSON value (trailing whitespace is fine; any other trailing data is rejected before anything changes), must not contain unknown fields, and is limited to `server.max_body_bytes` (8 MiB by default).

| Route | Purpose | Success |
| --- | --- | --- |
| `GET /healthz` | Process is up | 200 |
| `GET /readyz` | Runtime accepts work (409 during config preparation; 503 while shutting down or after storage failure) | 200 |
| `GET /v1/providers`, `/v1/models`, `/v1/tools` | `{"providers"|"models"|"tools": [...]}`: configured catalog and tool schemas only (built-ins: `read`, `write`, `edit`, `bash`, `history`, `subagents`). No network discovery on these routes. | 200 |
| `GET /v1/providers/{provider}/models?account=work` | `{"models":[...],"snapshot_id":"..."}` (snapshot ID only on installation-capable hosts): one explicit upstream discovery GET, separate from configured aliases; subscription account required, API-key account rejected | 200 |
| `PUT /v1/providers/{provider}/models` | Install latest snapshot for this account for new work, while chats may run (stock host); no prompt/settings reload; body: snapshot_id, account, optional context_tokens (per-model provider capacity, or 128,000 when unreported), confirm_empty. See [contract](MODEL-INSTALLATION.md). | 200 |
| `POST /v1/sessions/{id}/model` | Explicit model/account handoff; only the target session must be fully idle. Preserve history. Requires Idempotency-Key. | 200 |
| `GET /v1/diagnostics` | Startup and asynchronous lifecycle diagnostics, including skipped tool registrations and background bash exit-observation failures: `{"diagnostics": [{category, code, tool, session_id, message}]}` | 200 |
| `POST /v1/sessions` `{name, workspace, config?}` | Create a session | 201 |
| `GET /v1/sessions?after=&limit=` | Full session snapshots in creation order (`next` is the cursor for the next page; limit 0 or over 200 means 200). Prefer the routes below for interactive lists. | 200 |
| `GET /v1/sessions/recent?workspace=&parent=&q=&active=&after=&limit=` | `{"sessions": [summary...], "next": ...}`: compact rows, most recent conversation activity first. `q` searches session names and workspace paths (not messages) across all workspaces; `active=true` lists unfinished work; `parent` lists direct children (optionally only in `workspace`); `workspace` alone lists top-level rows with up to 5 nested `children` and `child_count`. Same limits as sessions. [Semantics](LIBRARY.md#chat-lists-and-activity). | 200 |
| `GET /v1/workspaces?after=&limit=` | `{"workspaces": [{workspace, sessions, active, activity_at, updated_at}], "next": ...}`, most recent activity first; no session rows. Clients without this route (404) are talking to an older server. | 200 |
| `GET /v1/sessions/{id}` | Inspect: phase, pause, active turn/tools, pending steering, usage, optional parent_id, `last_event_seq`, and `replay_after_seq` for reconnecting to current active work | 200 |
| `PATCH /v1/sessions/{id}` `{name}` | Rename | 200 |
| `DELETE /v1/sessions/{id}` | Delete an idle session; `children_busy` if owned children have unfinished work. Their histories are preserved after allowed deletion. | 204 |
| `POST /v1/sessions/{id}/subagents` `{name?, prompt, workspace?, config?}` | Atomically create/start a child, return ordinary `TurnAck`; requires Idempotency-Key. [Selections/recovery](SUBAGENTS.md). | 202 |
| `GET /v1/sessions/{id}/subagents` | `{"subagents":[...]}`: direct children with session, latest turn, account and history/event references | 200 |
| `POST /v1/sessions/{id}/turns` `{prompt}` | Start a turn (idle sessions only) | 202 |
| `GET /v1/sessions/{id}/turns?after=&before=&limit=&summary=` | Forward pages return `{"turns":[...],"next":"..."}`. `before=0` selects the newest page; earlier pages return `older`. `summary=true` omits frozen instructions but keeps status/reason/error, times and usage. Default `summary=false` preserves full turn records. Limit defaults to and is capped at 200. | 200 |
| `POST /v1/sessions/{id}/steering` `{text}` | Add guidance | 202 |
| `GET /v1/sessions/{id}/steering` | Pending guidance: `{"steering": [...]}` | 200 |
| `DELETE /v1/sessions/{id}/steering` | Clear all pending guidance | 200 |
| `POST /v1/sessions/{id}/interrupt` | Stop/pause current work and owned descendants (not unrelated sessions) | 202 |
| `POST /v1/sessions/{id}/continue` `{prompt?}` | Start a new turn on an idle session; cannot bypass a suspended original operation | 202 |
| `POST /v1/sessions/{id}/compact` | Manual compaction (idle only, never queued) | 202 |
| `POST /v1/sessions/{id}/reload` | Reread only this idle chat's configured prompts, workspace instructions and skill catalog; body empty or `{}`, requires Idempotency-Key, returns `{effective, overrides, resources}`. | 200 |
| `GET` / `PUT /v1/sessions/{id}/config` | GET returns `{effective, overrides, resources}`; PUT replaces the override object and refreshes resources for this fully idle chat only | 200 |
| `GET /v1/sessions/{id}/context` | Saved active or installed base instructions (`instructions_state`: `prepared`/`base_only`), budgets and retained-context estimates; excludes uncommitted pending guidance, performs no file reads | 200 |
| `GET /v1/sessions/{id}/file?path=&limit_bytes=` | Explicit trusted-owner regular UTF-8 file read on the backend machine; absolute or chat-workspace-relative path. Returns `{session_id, path, text, bytes, limit_bytes}`; no model call/history mutation. [File contract](#read-only-files). | 200 |
| `GET /v1/sessions/{id}/history?after=&before=&limit=` | Forward pages keep returning `{"items":[...],"next":...}`. Backward pages use an exclusive sequence boundary: `before=0` selects the newest page, then pass `older` to continue toward the beginning. Items in every page stay in ascending canonical order; `older` is omitted when exhausted. Do not combine `after` and `before`. Limit defaults to and is capped at 500. | 200 |
| `GET /v1/sessions/{id}/events` | SSE replay and follow | 200 |
| `POST /v1/auth/logins` `{provider, account, redirect_uri?, enable_plan_usage?}` | Start a browser/device login; OpenAI host supplies its exact bound loopback URI | 202 |
| `POST /v1/auth/logins/{id}/complete` `{callback_url}` | Complete the one-shot browser callback; LoginID is its completion identity, no additional Idempotency-Key | 200 |
| `GET /v1/auth/logins`, `GET /v1/auth/logins/{id}` | Retained non-secret login states (including recovered interrupted logins); the list is `{"logins": [...]}` | 200 |
| `DELETE /v1/auth/logins/{id}` | Cancel and join a starting/pending login | 200 |
| `GET /v1/auth/accounts` | `{"accounts": [...]}`: retained registrations, current status/identity/expiry, plan permission/block/reset and pending/last transfer status; never tokens | 200 |
| `DELETE /v1/auth/accounts/{name}` | `LogoutResult`: local clearing and confirmed/unconfirmed/not_applicable remote revocation; ten-second host deadline | 200 |
| `POST /v1/auth/accounts/{name}/retry-plan-usage` | Explicitly clear the local plan block; no provider call, consent or turn start | 200 |

### Transcript pages and event replay

A client opening a long chat can request `GET .../history?before=0&limit=80` to show the newest entries first, then use each response's `older` cursor to request earlier entries only as needed. The server returns the nearest preceding page in canonical order; it does not impose any retention limit. The existing `after`/`next` direction is unchanged for export and older clients. A page edge may split a tool call from its result, so clients reconcile the visible row by `call_id`/turn and sequence instead of assuming each page is a complete turn. When the saved tool-call record exists, a `function_call_output` also carries that call's original `name`, `namespace`, and `arguments`; a client can explain the result without fetching arbitrary older pages. This uses the existing canonical tool record and exposes no private provider replay data.

Turn records have their own cursor space (`rowid`) and page size. `summary=true` is a projection for UI footers, not a different record: it suppresses frozen instructions but retains outcomes and usage. Full inspectors/exporters can continue using the default full response. `before` and `after` are mutually exclusive for both history and turns.

After `GET /v1/sessions/{id}`, use `replay_after_seq` as the SSE `after` cursor if the snapshot has an active turn or compaction. It starts at the current model attempt, current tool batch, or current compaction, avoiding replay of completed work from earlier turns or earlier steps in the current turn. During preparation or before a tool starts, it uses the snapshot's latest event sequence and follows only new events. For an idle snapshot, use `last_event_seq`. The snapshot and cursor are read consistently. Event delivery is still at-least-once, so deduplicate by event `seq`; saved history remains authoritative for completed entries. `GET /v1/sessions/recent?q=...` remains independent server-side chat-name/workspace-path search across loaded and unloaded chats.

### Idempotency-Key

Library mutations need an `Idempotency-Key` header except browser login completion, whose LoginID is already its one-shot identity. The host-owned catalog PUT instead uses its latest discovery snapshot identity: identical repeats in the same process are safe, but restart/reload/supersession requires rediscovery; see [installation](MODEL-INSTALLATION.md). The server passes it to the library as the request ID. If a connection drops, retry with the same key: you get the original answer, and no duplicate turn or steering message is created. Reusing a key with a different body returns `409 conflict`. A missing key returns `400 invalid_argument`. Auth starts, cancellations, logouts and plan retry acknowledgements keep safe receipts in auth SQLite, in the manager—not the handlers. These survive restart and later logins under the same name: a delayed logout retry cannot delete replacement credentials. Mutation replay describes that operation, not current account state. Browser start replay after termination/restart returns terminal status without a reusable URL; inspect current account state with GET. Receipts are retained without pruning; an interrupted start requires a new key to begin again.

### Errors

```json
{"error": {"code": "conflict", "message": "Session is running. Interrupt it or wait before compacting.", "session_id": "..."}}
```

| Code | Status |
| --- | --- |
| `invalid_argument` | 400 |
| `forbidden_origin` | 403 |
| `not_found` | 404 (session or file missing) |
| `file_too_large` | 413 (no partial file content) |
| `file_read_failed` | 422 (OS read error, including permissions) |
| `conflict`, `children_busy`, `unsupported`, `auth_required`, `history_incompatible` | 409 |
| `context_limit` | 422 |
| `provider_error` | 502 |
| `storage_error`, `storage_busy`, `closed` | 503 |
| `process_observation_failed` | 500; the operation may already have committed, but process cleanup was not confirmed |

Provider failures optionally include `error.provider_failure` with upstream status/code/param/request ID, safe message/shape, retryability, nullable retry seconds and verified reset facts. Sam's outer provider error can remain 502; upstream status stays nested. Terminal/attempt/compaction events preserve the same payload, including the final request's metadata after an exhausted Copilot renewal. A provider error inside SSE can have transport HTTP 200; it is not a successful turn and is never relabelled 429 merely from a quota code. No raw credential/response body is exposed. The local example replays saved terminal failures in their chat session beside the composer; command errors appear beside their originating controls, not in a global footer.

`storage_busy` means another process held the database lock for 5 s while your command was being accepted. Nothing was accepted, and the server stays ready: retry with the same `Idempotency-Key`. `storage_error` means persistence failed; the server stops accepting work (`/readyz` returns 503) until it is restarted.

Failures that occur after work was accepted, such as a provider error, a failed compaction, a logged-out or rejected login account (`auth_required`), or history the selected account refuses (`history_incompatible`), arrive as events (`turn.failed`, `compaction.failed`, with a `code` field), not as HTTP errors. A foreground bash observation failure appears as a failed tool result and in `GET /v1/diagnostics`; a background failure appears there after the unchanged launch acknowledgement. Both use `process_observation_failed`, and deleting the affected session or closing the runtime returns an HTTP 500/library error rather than claiming process cleanup succeeded.

### Read-only files

Tap a file link in a trusted frontend, or explicitly enter a path. The client
calls `GET /v1/sessions/{id}/file?path=<URL-encoded path>&limit_bytes=<bytes>` directly on the
**connected sam-g backend**. For a browser viewing a remote chat, `/tmp/draft/SPEC.md`
is on the backend machine; `docs/README.md` is relative to that chat's workspace. This
route does not fetch HTTP URLs or read the frontend host's disk. Use URL encoding
for spaces, non-ASCII, `#`, `?` and literal percent characters in names.

The [library file-viewing contract](LIBRARY.md#human-owned-file-viewing) owns path
resolution, text validation, error categories and bounded capture. It allows
absolute outside-workspace paths and symlinks: any regular text file readable
by the backend OS user, **including sensitive files if deliberately requested**.
Unlike safe account/status metadata, this response is **not redacted**. It is
JSON (`Cache-Control: no-store`, `X-Content-Type-Options: nosniff`), never an HTML
response or automatic download. Clients must render inert text/Markdown, avoid
embedded resource loads, avoid persisting contents, and read only on explicit
user action. A filename/link in model output is not permission to load it.

No idle check, inference, tool permission check, history/event write or file
mutation occurs. Existing Host/Origin/CORS protections apply unchanged; there is
no new network exposure or authentication scheme. This remains a trusted-owner
API, not a public multi-user file service or filesystem sandbox. Concurrent edits
are not transactional snapshots. Contents must fit the **per-open** `limit_bytes`
file-byte budget (JSON overhead excluded): omitted, empty or zero uses the
backend's **8 MiB (8,388,608 bytes)** default; a positive integer chooses another
budget for just this read. Negative, noninteger and overflowing values return
`400 invalid_argument` before unnecessary session/file I/O. Values must fit the
backend's Go `int`; no hidden absolute product maximum is added. Very large
files require available backend/browser memory. This does **not** read or change
model `tool_output_limit`, session settings, or host configuration.

`limit_bytes` is a read option, not a persistent preference. The complete open
fails with `413 file_too_large` and no text if the file exceeds it. Clients can
let the user change the size control and explicitly Open again without an idle
chat, reload/restart, pagination or automatic higher-limit retry. The returned
`limit_bytes` is the authoritative effective limit, including when defaulted.

An older backend answers `404 not_found` with `no route for ...` (or a plain
HTTP 404). Frontends should explain that sam-g needs updating, not silently run
a model tool. A current backend's missing-file 404 is a different failure.

### Example session

```sh
B=http://127.0.0.1:8080
curl -s -X POST $B/v1/sessions -H 'Idempotency-Key: create-1' \
  -d '{"name":"tests","workspace":"/home/me/project"}'
# {"id":"ses_...","phase":"idle",...}

curl -s -X POST $B/v1/sessions/ses_.../turns -H 'Idempotency-Key: turn-1' \
  -d '{"prompt":"Run the tests and fix failures"}'
# {"session_id":"ses_...","turn_id":"turn_..."}

curl -sN $B/v1/sessions/ses_.../events
curl -s -X POST $B/v1/sessions/ses_.../steering -H 'Idempotency-Key: steer-1' -d '{"text":"skip the slow tests"}'
curl -s -X POST $B/v1/sessions/ses_.../interrupt -H 'Idempotency-Key: int-1'
curl -s -X POST $B/v1/sessions/ses_.../continue  -H 'Idempotency-Key: cont-1'
```

### Usage and context status

Session responses include cumulative `usage` (main model) and
`compaction_usage` (summarizer), plus nullable `latest_usage` and
`latest_compaction_usage`, projected from the newest ended provider attempt in
each scope. Usage objects add nullable `cache_read_tokens` and
`cache_write_tokens` alongside the existing counters; older clients can ignore
the additive fields. Missing/null is unknown, not zero. `input_tokens` remains
total input including cached input. Aggregates sum reported counts only; latest
response hit rates must use the latest object's read/total pair, not aggregate
usage. An ended attempt missing data does not reuse an older known rate. Turn
records, `usage` events, and `model.attempt.ended.usage` use the same fields.
See [token usage semantics](LIBRARY.md#token-usage-and-cache-measurement) for
provider mapping, validation and durable failure accounting.

`GET …/context` is the status authority: `estimated_tokens`, `input_budget`,
`trigger_tokens`, `auto_compaction`, and optional `model_error` /
`compaction_error`. The new `compaction_error` describes current local
model/settings selection failure, not a provider/account availability check.
Render estimates approximately, for example
`ctx ~130k/264k · compact near ~237k`; the denominator reserves output, not the
model's total capacity. Auto-disabled/unavailable states should not display an
active threshold. A threshold is checked before requests with summarizable
older history, not an immediate trigger. Inspection excludes provisional
streamed output and unapplied guidance. Clients can refresh with ordinary
selection, saved-history/lifecycle/config events and reconnect; no per-delta
inspection or independent polling is required. See
[inspection contract](LIBRARY.md#instructions) and
[compaction](LIBRARY.md#compaction).

### Compaction retention

After `POST …/compact` succeeds, the model keeps at least the configured recent-history estimate in whole messages/tool groups, plus the preceding genuine user request when needed. New prompts or steering do not remove that protected request; only the next successful compaction can replace the selection. For a long tool loop, the older middle is summarized rather than retaining the entire turn. If the retained context cannot fit, compaction fails without changing the installed selection. History endpoints still return the original entries. This changes no HTTP route or event envelope.

## Host extensions and approvals

A custom Go host installs `Config.Hooks` (BeforeTurn, BeforeRequest, BeforeTool, BeforeCompaction) and then passes its runtime to `server.New`. The stock executable has no callbacks or built-in approval UI. There is no HTTP function upload, approval endpoint, grant cache or hook-specific reload. A host waiting on approval uses the callback's IDs and ctx with its own authenticated UI; normal HTTP read/control and other sessions remain available.

The existing history route forwards `kind: "extension"`, saved `role`/labelled `text`, and optional `source: {stage, handler_id, operation_id, step, attempt?}`. BeforeTurn is developer instructions; BeforeRequest is user-role **application data, not a human message**; BeforeCompaction is developer instructions labelled for that summarization operation. Source survives reload/restart, repeats append, and old notes are never regenerated. Exact labels are in [the extension-history reference](LIBRARY.md#extension-history-and-authority). Context inspection counts retained entries but keeps `instructions` base-only, and neither inspection nor SSE replay runs hooks.

SSE forwards `hook.result` / `HookResultPayload` as a diagnostic: reached decisions, accepted `history_seq` references, or failure/contributor IDs, without copying contribution text. `hook_failed` fails/pauses the turn or compaction; `tool_denied` is a saved failed tool result the model can adapt to; `compaction_denied` stops/pauses compaction without a request/install/follow-up. These are asynchronous operation outcomes, not new HTTP commands. Accepted notes can remain even if sending later fails; a history reference is not delivery proof. The library owns cancellation, durability and fits; the server has no parallel hook engine.

## Event stream

```text
id: 7
event: tool.completed
data: {"version":1,"session_id":"ses_…","seq":7,"time":"…","kind":"tool.completed","category":"output","turn_id":"turn_…","call_id":"call_…","payload":{…}}

: heartbeat
```

- `id` is the per-session sequence number, and `event` is the event kind. See [LIBRARY.md](LIBRARY.md#events) for the kinds and their payloads.
- Events optionally carry `step` and `attempt`. Group model deltas by original operation/Step/Attempt: a recovered attempt is a **new partial display**, not more bytes appended to the interrupted one. `model.attempt.started/ended` mark durable reservations/classification; `operation.suspended/resumed` explain activation, and `tool.attempt.interrupted` warns of unknown effects. Old saved events without those fields remain readable.
- Each event has a `category`: `output` (answers, tool output, normal lifecycle), `diagnostic` (retries, compaction, interruptions, recovery notices), or `error` (failed turns/tools/compactions and model refusals). Use it to style the UI; it is not the turn's outcome. A model refusal streams as `refusal.delta` events (category `error`, payload `{"text":…}` only), then a `response.refusal` event whose payload carries `code: model_refusal` and the complete text.
- **Reconnect:** send `Last-Event-ID: N` or `?after=N`. The stream replays events after `N`, then follows new ones. If both are sent, they must match. A cursor beyond the last event returns `400`. Delivery is at least once, so deduplicate by `id`.
- A `: heartbeat` comment is sent after 15 seconds without events.
- **Slow clients:** each write has a deadline (`server.sse_write_timeout_seconds`, 30 s by default). A client that stops reading is disconnected and can reconnect with its cursor. The agent never waits for clients. Events are read from SQLite, so each client holds only its cursor and one bounded page of events.
- **Stream end:** the server sends a final `event: error` frame with no `id`, then closes. The frame carries `not_found` when the session was deleted, or `closed` when the server is shutting down. Events committed during shutdown are replayed after the restart, when you reconnect.
- Closing the stream only stops watching. It never cancels work. Use `interrupt` for that.

## Check, stop, restart and change ports

For a manually launched server, check the configured listener (8080 is the default):

```sh
lsof -nP -iTCP:8080 -sTCP:LISTEN
curl -fsS http://127.0.0.1:8080/readyz   # {"status":"ready"}
```

`lsof` shows the owning process and PID; a listening port alone is not a readiness
check. Confirm the executable before signalling it. Use `kill -TERM PID`, replacing
`PID` with the actual sam-g PID, or Ctrl+C in its foreground terminal. Wait for it
to exit before starting another owner of the same databases. Don't use `kill -9`
for normal shutdown. If systemd/launchd owns the process, use that service manager's
stop/restart command instead, or it may immediately restart the process you killed.

To start manually in the background after the previous owner has exited:

```sh
mkdir -p "$HOME/.sam-g/logs"
nohup "$HOME/.sam-g/bin/sam-g" >> "$HOME/.sam-g/logs/server.log" 2>&1 < /dev/null &
# Check /readyz again; inspect failures with:
tail -n 50 "$HOME/.sam-g/logs/server.log"
```

This survives closing the terminal, not a reboot. Logs are ordinary uncapped files;
arrange rotation in your service manager if needed. Saved PID notes can become
stale—check the actual process/listener rather than blindly signalling a recorded PID.

Stopping the backend suspends unfinished work. Restart resumes it only when
`server.resume_on_start` is true; the project default is false. This startup choice
is distinct from automatic process startup after reboot and from a fresh Continue.
See [shutdown and recovery](#shutdown-and-recovery) for repeated-effect warnings.

To change the local port, edit `server.listen` in `~/.sam-g/config.json` and restart;
SIGHUP cannot change an open listener. Update direct clients and proxy targets.
For remote access, see the deployment options below.

## Deployment on a remote machine

The service is for one user on trusted machines. Anyone who can reach the port effectively has a shell as the service user. The access boundary is SSH or Tailscale, not the API.

1. Copy the binary and a config file to the remote machine. Keep `database_path` on local disk, not a network filesystem.
2. Choose how to reach it:
   - **Loopback plus an SSH tunnel (default):** keep `"listen": "127.0.0.1:8080"`. From the laptop, run `ssh -N -L 8080:127.0.0.1:8080 me@remote-host` (over Tailscale if you like), then use `http://127.0.0.1:8080`.
   - **Direct Tailscale node address (not a Tailscale Service):** set `"listen": "100.x.y.z:8080"` to the machine's Tailscale IP (`tailscale ip -4`). This is device-specific and includes a port; it is not a port-free named Service URL. Use Tailscale access controls so that only intended devices can reach it.
3. Supervise it with your OS service manager. The server does not install anything itself. Here is an example systemd user unit that you would create by hand (`~/.config/systemd/user/sam-g.service`):

   ```ini
   [Unit]
   Description=sam-g agent server

   [Service]
   ExecStart=%h/.sam-g/bin/sam-g -config %h/.sam-g/config.json
   EnvironmentFile=%h/.sam-g/env
   StandardOutput=append:%h/.sam-g/logs/server.log
   StandardError=append:%h/.sam-g/logs/server.log
   Restart=on-failure
   TimeoutStopSec=20
   KillMode=mixed

   [Install]
   WantedBy=default.target
   ```

   Create `~/.sam-g/logs/` first. `~/.sam-g/env` holds `OPENAI_API_KEY=...` with mode 0600. Enable it with `systemctl --user enable --now sam-g`, and add `loginctl enable-linger $USER` so it keeps running after you log out. On macOS, a launchd agent with `KeepAlive` does the same job.

### Named Tailscale Services

For a private HTTPS URL without a port, use a named Tailscale Service forwarding
to sam-g's loopback listener. **Never expose this API through Funnel or a public
proxy.** See the official [Tailscale Services documentation](https://tailscale.com/docs/features/tailscale-services)
for administrator setup and access policy.

1. Install Tailscale 1.86+ and `jq`. A tailnet administrator must enable HTTPS,
   define a Service such as `sam-g-server` with endpoint `tcp:443`, assign a
   suitable host tag, grant access to intended clients, and approve the host
   advertisement if required. **Tagging changes the machine's identity** and can
   invalidate existing SSH/access rules; retain a recovery connection. Grants
   are additive: a narrow grant does not cancel a broader existing allow rule.
2. Use a distinct Service name for each independent backend/database. Inspect
   `tailscale serve get-config --all` before changing routes; preserve unrelated
   mappings. Do not use broad reset/logout commands to troubleshoot deployment.
3. Add `http://sam-g-server.your-tailnet.ts.net` to the existing
   `server.allowed_origins` list. It is HTTP because Tailscale terminates HTTPS
   and forwards to the backend over HTTP. A separate browser frontend also needs
   its own exact page origin. Preserve other trusted entries and restart sam-g
   after changing listener/origin settings; SIGHUP does not apply them. See
   [Host/CORS rules](#browser-access-and-cors).
4. If sam-g is stopped, the helper configures the named local endpoint and starts
   the installed binary using the configured `127.0.0.1:PORT` listener:

   ```sh
   ./scripts/run.sh --tailscale-service sam-g-server
   ```

   A new mapping requires confirmation; noninteractive setup needs the explicit
   name and `--yes`. A same-target mapping is reused; a conflicting target fails
   rather than being overwritten. The helper never changes tags, grants,
   administrator definitions, origins, other routes or other machines.

   If sam-g is **already running**, do not start a duplicate. After reviewing any
   existing mapping, configure only forwarding, substituting the actual port:

   ```sh
   curl -fsS http://127.0.0.1:8080/readyz
   tailscale serve --service=svc:sam-g-server --https=443 http://127.0.0.1:8080
   tailscale serve get-config --all
   ```

5. From an allowed client, check
   `curl -fsS https://sam-g-server.your-tailnet.ts.net/readyz`. Expect
   `{"status":"ready"}`. For browser clients, also verify a real models/session
   read through the page. Keep existing access until DNS, HTTPS and that client
   flow work. A saved route does not prove that the app is running, and the API
   root has no homepage.

For a timeout, check local readiness, host approval and access grants. For a 403,
check both the forwarded Host origin and browser origin, then restart after
correcting config. If `tailscale dns query` resolves the name but ordinary lookup
does not, fix the client's DNS integration rather than resetting routes.

A Service route remains configured independently of the application. Neither it
nor the helper provides reboot supervision; use your service manager. The bundled
[local browser example](../example/README.md) runs a separate loopback-only host,
not a frontend for the stock API executable.

## Browser access and CORS

**Same-origin browser requests are supported by default**, including fetch mutations and EventSource/SSE. Serve a trusted page alongside the API, as the [local example](../example/README.md) does. The core library has no incoming HTTP/origin policy; this protection belongs to the `server` adapter.

The server validates the request Host against the connection's actual local IP/port (or `localhost` at that port for loopback). It never trusts `X-Forwarded-*` headers. This prevents a hostile website from pointing its own hostname at the local service through DNS rebinding. Native clients still work, but arbitrary Host aliases are not automatically trusted: use the listener's IP/localhost, or configure the intended DNS/proxy origin explicitly.

For a separate frontend, such as a dev page on port 5173 calling the API on port 8080:

```json
{
  "server": {
    "listen": "127.0.0.1:8080",
    "allowed_origins": ["http://localhost:5173"]
  }
}
```

This is a fragment of the host configuration; keep the existing database/provider/model fields. Restart after changing `allowed_origins`. Origins are exact scheme/host/port values, without paths or trailing slashes; wildcards and opaque `null`/file origins are rejected. Different ports are different origins. A reverse-proxy frontend origin must also be explicitly trusted; forwarding headers alone do not grant access. A TLS-terminating proxy should forward the backend's actual IP/port as Host rather than assume an HTTPS public origin makes an HTTP backend Host equivalent. This does not make public unauthenticated proxying safe.

Go hosts configure the same policy **before serving**:

```go
api := server.New(rt)
if err := api.SetAllowedOrigins([]string{"http://localhost:5173"}); err != nil {
    return err
}
// Mount api at /v1/ alongside your page, or serve it directly.
```

Only trusted origins receive an echoed `Access-Control-Allow-Origin`, never `*`. Preflight supports the API's GET/POST/PUT/PATCH/DELETE methods and Content-Type, Idempotency-Key, Last-Event-ID and Accept headers. Cookie-authenticated CORS is not enabled. Unknown origins, cross-site requests without a trusted Origin, and untrusted Host values get `403 forbidden_origin`. A host invoking ServeHTTP without net/http's listener context must explicitly configure its browser origin.

**An allowed origin can operate the entire coding API and explicitly read any OS-readable regular text file, including sensitive files.** This is protection against unintended web-page access, not user authentication or multi-user isolation. Trust only frontends you control; do not expose the unauthenticated service publicly.

## The agent's history tool

The agent has a built-in `history` tool for its own session: it searches the original conversation with a Go regular expression (10 matching entries per page) and reads entries by sequence number, even after compaction or a restart. Calls and results appear in the event stream like any tool. It cannot reach other sessions. Disable it per session with `"disabled_tools": ["history"]` in `PUT /v1/sessions/{id}/config`. Details are in [LIBRARY.md](LIBRARY.md#history-tool). There is no HTTP search endpoint; clients read history with `GET /v1/sessions/{id}/history`.

## Shutdown and recovery

On SIGINT or SIGTERM, the server:

1. Stops listening, so new connections are refused. Requests already in flight get `503 closed` for new work.
2. Closes the library runtime. Active turns and commands are cancelled: bash process groups get SIGTERM, then SIGKILL after 2 seconds. Eligible work becomes `suspended`, keeping original IDs and guidance pause choices. Known late tool successes/failures and complete responses remain saved. Close never launches another effect/follow-up, though completion and a queued follow-up acceptance may commit together, leaving the latter suspended. Background processes are stopped too: SIGTERM to each whole process group, up to 2 seconds for all of its members, then SIGKILL. If exit observation fails (`ECHILD`/`ESRCH` under host auto-reaping), sam sends no further signals and reports `process_observation_failed`; the command may remain running, cleanup is not confirmed, and the runtime Close result is an error.
3. Ends event streams, waits for handlers, closes SQLite, and releases the database lock after the process owner has safely relinquished uncertain ownership.

All of this must finish within a 10-second grace period. If it does not, the server logs `incomplete shutdown`, exits with status 1, and leaves cleanup to the next start. The database lock is released by the process exit, never earlier while shutdown was still running. A second signal during shutdown exits immediately. Set the service manager's stop timeout above 10 seconds.

A stalled configuration or prompt-file read during SIGHUP does not block termination handling. Shutdown does not wait for that host reload task; the runtime prevents late publication once closing begins. An OS file read cannot reliably be cancelled, so the blocked task may remain until the executable exits.

After any restart, including a crash or `kill -9`:

- New-format unfinished operations are `suspended` unless `resume_on_start` opted in. Read history/events/context and original active IDs without running effects. To opt in, set `server.resume_on_start: true` and restart. Otherwise POST `…/interrupt` to terminally stop the original work, then POST `…/continue` to deliberately start a new turn. Suspended work conflicts with ordinary new turns, manual compaction/deletion and all-idle reload; independent idle sessions and readiness remain available.
- Recovery uses latest configured prompts/tools/hooks/account choices, preserving explicit overrides and main-model binding. Replay-allowed unfinished tool calls **may repeat**, with original arguments and current policy; saved outcomes do not. Unfinished replay-unsafe calls are skipped (including never-started calls), with normal paired results distinguishing non-execution from uncertainty. The ordinary agent then continues. `GET /v1/tools` exposes host-owned `replay_unsafe`; built-in bash/write/edit set it. See [recovery policy](LIBRARY.md#recovery). Hooks may repeat with different values; all previously accepted text stays unchanged. Provider reservations/deadlines retain the total four-attempt budget across crashes. Committed main partials get one durable interrupted note; summary partials remain private.
- Explicit Interrupt is durable and never revived by startup opt-in. Legacy unfinished records without new-format checkpoints still become interrupted/paused once and require a deliberate new Continue.
- Background processes started with bash's background mode are stopped on a clean shutdown (SIGINT/SIGTERM). If shutdown runs out of its grace period, or after a crash or `kill -9`, they may keep running (a service manager can still kill them: systemd with `KillMode=mixed` sends SIGKILL to processes left in the service's cgroup when the service stops), and the restarted server neither adopts nor signals them: their old PIDs are in history, but a PID may have been reused. Stop them yourself if needed. Processes that escape their group (`setsid`, daemonizing) are never tracked; `nohup` alone does not escape it.
- Only one server can own a database. A second one fails at startup with `conflict`.

## Previewing a dev server

When the agent starts a dev server in the background, it listens on the **remote machine**. A URL such as `http://localhost:5173` in its answer means localhost of that machine, not of your laptop. To open it from your laptop, forward the port over SSH (through Tailscale if you like):

```sh
ssh -N -L 5173:127.0.0.1:5173 me@remote-host   # then browse http://127.0.0.1:5173
```

Or, if the dev server listens on the machine's Tailscale address, open that address directly. sam does not set up forwarding, firewalls, systemd units, or any network exposure for these processes. A background command ends when it exits on its own; ask the agent to run the dev server command itself (`npm run dev`), not a launcher that backgrounds it with `&` and exits. The server stops them when their session is deleted or the server shuts down cleanly; their log files stay in the workspace.

## Limits

| Setting | Value | Configurable |
| --- | --- | --- |
| JSON request body | 8 MiB | `server.max_body_bytes` |
| SSE write deadline | 30 s per write | `server.sse_write_timeout_seconds` |
| Model catalog envelope | 32 MiB, fail on overflow, no truncation | fixed |
| Provider SSE event (assembled) | 32 MiB | `max_provider_event_bytes` (library setting) |
| Tool result per call | 256 KiB | session `tool_output_limit` |
| Human full-file view (file bytes, excludes JSON) | 8 MiB default; oversize fails without text | per-open `limit_bytes` query, independent of agent config |
| SSE heartbeat | 15 s idle | fixed |
| Request header read timeout | 10 s | fixed |
| Shutdown grace | 10 s | fixed |
| SQLite busy wait | 5 s, then `storage_busy` (before acceptance) | fixed |
| Page sizes | sessions (including `recent`)/workspaces/turns 200, history/events 500 (larger requests are clamped) | per request, up to the maximum |
| Provider error text | 4 KiB body bound (cap-plus-one); about 1 KiB shown, redacted first and marked when shortened. Oversized/unreadable errors are unclassified and not retried. | fixed |

Runtime numeric zero means its built-in default. In a session override object, absent/null inherits the shared value, while explicit zero selects the built-in numeric default.

There is no global response timeout, so healthy SSE streams stay open indefinitely.
