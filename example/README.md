# Full-featured local sam-g example

A plain, blocky web page for trying sam-g on your own computer. Sign in to named ChatGPT and/or GitHub Copilot accounts, load models for the selected account, and create sessions with optional file/shell tools. No API key, Node.js, npm, frontend framework, or separate API server is needed.

This is a **local, single-user example** of the runtime's interactive workflow, not a public multi-user service. It uses the real sam-g library, including its shared multi-account auth SQLite store, model discovery, conversations and account-limit handling. There is no automated integration suite; **live provider/client eligibility and acceptance are not guaranteed**. Copilot is enabled by default with the same public Copilot Plugin client ID and compatibility headers used by Pi; GitHub still controls account, organization and model access.

## 1. Install Go and open the checkout

Install **Go 1.27 or later** from [go.dev/dl](https://go.dev/dl/). Use macOS or Linux.
Download or clone this repository, then open a terminal in its root directory—the
directory containing `go.mod`, not the `example/` subdirectory. Check the toolchain:

```sh
go version
```

## 2. Start the example

From the repository root:

```sh
go run ./example
```

Go downloads the module dependencies on the first run. The command builds and starts **both the web page and its backend**. Leave this terminal running. It prints:

```text
Open http://127.0.0.1:8765 in your browser.
Data: /your/home/.sam-g/example
Ctrl+C stops the example. Opening the page never starts model work.
```

Open **http://127.0.0.1:8765** in your browser. Use that exact address, not `localhost`, a `file://` HTML page, or another static-file server. Do **not** also run `go run ./cmd` for this example.

If that port is busy:

```sh
go run ./example -port 8766
```

Or choose a free port with `-port 0` and open the address printed by the program. The browser and example must run on the **same computer** for this walkthrough; a remote host's loopback address is not your browser computer's loopback address.

## 3. Add accounts and pick a model

1. In **Sign in / add account**, choose **ChatGPT** and enter a local label such as `personal` or `work-chatgpt`. Click **Sign in with ChatGPT**. This explicitly requests permission to use your ChatGPT plan.
2. Your browser navigates to OpenAI. Sign in and review the consent screen.
3. OpenAI redirects back. The account selector shows the saved label and `signed_in`. Add further accounts with different labels; signing into one does not sign out the others. Labels are not workspace or model names. Reuse the same provider/label to reauthenticate that saved identity. Account actions do not require sessions to be idle. Reauthenticating a signed-in label asks for confirmation: completing sign-in replaces its saved login, so running work using that account may fail with an authentication error. Other accounts are untouched.
4. Choose **Account for models / new sessions**, click **Load models**, then choose a model. The request uses that account's provider and credentials. Changing this selector clears the previous account's catalog; it does not redirect an existing session. Each click fetches a fresh catalog without inference, policy enablement or a catalog cache.
5. Give the session a name and choose an **existing absolute workspace folder**. The default is the directory where you started the example, not its credential directory. Leave tools unchecked for a plain chat, then click **Create session**.
6. Type something small, such as `Reply with a short hello`, and click **Send**.

**Sending uses the session's selected subscription account and its allowance.** There is no API-key or alternate-account billing fallback. Sign-in alone is not proof that your account/client can use every model or endpoint. Provider rejection is reported, not bypassed.

The **Live activity** panel streams text, provider-exposed reasoning summaries, tool output and retry/recovery notices. Previews are grouped by operation, step and attempt; they are not proof a turn succeeded. Completed tool previews stay visible while slower sibling tools finish; they disappear only when the matching turn/call result is actually rendered in saved history, not merely when the event cursor advances. Reconnects deduplicate event sequence numbers and recover partial current work without merging separate attempts. **Stop** interrupts the selected session and remains available during unrelated model discovery, login or setup requests. A failed/disconnected command is never automatically resubmitted; inspect saved state before trying again.

## GitHub Copilot device-code login

No extra setup is needed. Start normally:

```sh
go run ./example
```

The example bundles the **public GitHub Copilot Plugin client ID used by Pi** (`Iv1.b507a08c87ecfe98`) and sends the matching Copilot/VS Code compatibility headers. It is not a secret, personal access token, or sam-g-owned registration. Expect GitHub's authorization page to identify the Copilot Plugin application. Sam keeps its own credentials in its auth database; it never imports Pi's stored tokens.

To deliberately use a different registration, `-copilot-client-id YOUR_CLIENT_ID` or `SAM_G_COPILOT_CLIENT_ID` overrides the default. That app needs device flow and appropriate Copilot access; an arbitrary OAuth app is not guaranteed to work. If you previously signed in using a different client ID, sign out and sign in again to obtain a grant for the selected registration.

1. Choose **GitHub Copilot** in the sign-in form and a unique label, for example `work-copilot`.
2. Click **Get GitHub device code**. The page displays the user code, GitHub verification link, expiry and pending status.
3. Open the link, make sure GitHub is signed into the intended account, and enter/approve the code. Use a separate browser profile if you need another GitHub identity.
4. Return to this page. AuthManager polls the provider; the page polls only local status. On completion the challenge disappears and the named account becomes selectable. **Cancel this sign-in** cancels just that attempt.
5. Select that account above, click **Load models**, and create a session. A denial, expired code, disabled device flow or missing Copilot access is shown as an error—no bypass, hidden CLI, model-enablement call or static catalog fallback.

Both providers are enabled by default and share one protected AuthManager database. Existing Copilot sessions retain their provider/model bindings across restart. The [pinned compatibility profile](../docs/AUTH-MODELS.md#copilot-compatibility-profile) documents the exact headers and app ID.

The existing controls support Responses and the initial **Copilot Opus 5.5 native Messages** profile. Chat Completions and older budget-thinking Claude models remain unsupported. A catalog entry is not transport proof; native registration requires reported Messages support and maximum output. See [native settings, roles, hard caps and replay](../docs/LIBRARY.md#native-messages-through-copilot). Models marked `requires_action` are not silently enabled—enable them in a supported Copilot client/settings flow, then reload the catalog. Discovery remains one explicit GET, with no background cache or policy POSTs.

## Switching accounts

The top account selector is for discovery, new sessions, usage acknowledgement and **Sign out selected**. It does not change where messages in an existing conversation go. Sign-out asks for confirmation and affects only the selected account; running work using it may fail, but unrelated accounts keep working. **I reviewed Usage — allow another try** clears that account's local plan block without starting a message or waiting for other sessions. Cancelling a sign-in/sign-out confirmation makes no request.

For an existing session, choose **This session's account → Switch account**. Only accounts of its provider are offered, and **only this session must be idle** before applying. This is an explicit handoff: the next turn uses the new account, stored history stays unchanged, older tool/reasoning context becomes portable text, and reasoning options reset to provider defaults. The model, workspace and tools stay the same. Save or discard other settings edits before switching. Choosing another account never falls back silently.

This example page uses **new sessions** to move between providers/models. The standard model-handoff API lets other clients change future work in the same chat while preserving history. Model aliases include their provider, so the same upstream model ID from both catalogs cannot share a binding.

The selected model's discovery note shows provider-reported context capacity, reasoning efforts and summary support. When capacity/thinking support is absent, an exact-ID Pi reference may be shown with its version/source. References are advisory: Pi's Codex route differs from ChatGPT token-sharing. They do not change the example's local budget or reasoning declarations. Unknown stays distinct from unsupported. See [metadata sources](../docs/MODEL-METADATA.md).

## Everyday controls

- **Create session:** choose a name, model, workspace and whether tools are enabled. An existing session keeps its model and workspace; create another session to change them.
- **Session list:** switch between saved conversations and see which are idle, running, suspended or paused for review. Switching views does not stop work. Switching clears both unsent message/guidance composers and unsaved session-setting edits. Saved queued guidance belongs to the runtime and is not cleared. Tabs can select different sessions independently; only non-secret session/login identifiers are saved in browser session storage.
- **Stop this session:** interrupt only the selected session. Other sessions keep working. Already-completed file edits/commands are not rolled back.
- **Tools enabled → Apply:** change the selected session's tools using the normal configuration API. Only the selected session must be fully idle, including no suspended work, before changing its settings. Adding/editing a model applies only to new work and does not wait for idle sessions.
- **Sign out selected:** choose the account at the top and confirm. To avoid disrupting its work, stop/wait for turns using that account first; unrelated sessions do not need to stop. Only that account's local credentials are cleared; other accounts remain signed in. If remote revocation cannot be confirmed, read the warning and review [ChatGPT Settings](https://chatgpt.com/settings/usage). A possible extra session from a cancelled login can keep this warning present after later sign-ins.
- **Cancel sign-in:** use the pending attempt's own cancel button. ChatGPT browser attempts for different accounts can finish in either order; callbacks are matched by state rather than one global last-login ID.
- **Usage blocked:** review Usage settings, then explicitly click **I reviewed Usage — allow another try**. This only clears the local block; it does not buy credits or automatically resend a message. A reset time may be unknown.
- **Stop the server:** press **Ctrl+C** in its terminal and wait for it to exit. Start the same command again to reuse your sign-in, sessions and tool choices, then reload the browser page.
- **Server stopped mid-turn:** unfinished work stays suspended. Read the warning, check the acknowledgement and click **Resume ALL suspended work** to admit the original operations. Replay-allowed tools/hooks may repeat effects; saved outcomes are not rerun. Unfinished unsafe tools are skipped and reported to the agent, which can continue autonomously; see [recovery policy](../docs/LIBRARY.md#recovery). This is runtime-wide, not selected-session-only. Alternatively, **Stop this session** terminally ends only that saved operation. Opening the page/server never auto-resumes work. An unrelated idle session can still run using an installed model.

All saved provider accounts and sessions belong to one local OS user; they are not separate application users or security boundaries. Two tabs viewing the same session still share that conversation. Two sessions using the same folder can edit the same files—choose different workspaces or avoid conflicting tasks.

## Using tools safely

Tools are **off by default** and require an explicit checkbox when creating a session, or **Tools enabled → Apply** for an existing session. The available tools are `read`, `write`, `edit`, `bash`, `history` and `subagents`. Native [subagents](../docs/SUBAGENTS.md) are ordinary same-runtime child sessions, controlled through the tool or standard API. Child settings inherit disabled-tool choices and Stop includes owned descendants. Once enabled, the model can invoke them automatically; there is no per-command approval dialog.

**The workspace is not a sandbox.** Tools run with your OS account's permissions and can access paths outside that folder, including private credentials. Only enable them for tasks/content you trust. The ordinary API can also change tool configuration; the checkbox is a convenience, not an additional security boundary.

A safe first test is to create an empty scratch folder yourself, use its absolute path as the workspace, enable tools, and ask: `Create hello.txt containing hello, then read it back.` Expand the tool request/result cards to inspect the arguments, output and saved success/failure status. Shell commands and file writes are real; stopping later does not undo them. The UI reports active tools and streams their output; saved results show the eventual outcome. Commands that explicitly start background processes can outlive a turn; stopping a turn is not the same as stopping those processes. Runtime shutdown cleans up processes it owns.

Saved chats keep their workspace. Inspect the displayed path before enabling tools; create a new session if you need a different project directory.

## Steering, Continue and compaction

- **Send guidance** changes direction without interrupting an active turn. Guidance is saved and applied before the next model request; pending guidance can start a follow-up. On an idle unpaused session it can start a turn immediately, so it may use allowance. On a paused session it waits for Continue. The queue is visible and **Clear pending guidance** removes only pending entries.
- **Continue / follow-up** starts a new turn using queued guidance and optional text in the composer. It is not Resume and cannot bypass a suspended original operation.
- **Compact now** requests a summary for the selected idle session. It uses the configured compaction model/account and can consume allowance. Original history is retained. If there is too little eligible history, or the request cannot fit, the core returns a descriptive error rather than silently dropping context.
- **Session settings / compaction** controls automatic compaction, its model/account, output reserve, trigger and retained-tail percentages, and extra summary instructions or a prompt file. Automatic compaction is initially off; enable it explicitly for long-running work. Different-provider summaries require their own compatible account.

The usage line distinguishes provider-reported token counts from conservative context estimates. Missing counts display **unknown**, not zero; a real zero remains zero. Context inspection includes the installed summary, protected-history coverage and current instructions. It does not claim to show hidden chain-of-thought or the exact next request.

## Errors stay with their operation

If a provider rejects a turn, its saved failure appears inside that session beside the conversation/composer, labelled with the failed turn. Expand **Safe failure details** for its sanitized code, actual upstream HTTP status and request ID. Reloading or selecting the session again replays the existing durable events; this is not an invented assistant answer or a second error store. Failed attempts/retry previews remain distinct from terminal turn failure. Other sessions never receive that failure, and background failures do not steal focus or scroll you away; paused sessions are marked for review in the session list.

For example, a ChatGPT usage-limit failure can arrive inside a stream whose transport status was HTTP 200. The page shows that actual status, never HTTP 0 or an invented 429. The account's local usage block prevents new inference; the reset remains **not reported** unless supplied by the provider's documented contract. Review Usage and explicitly allow another try when appropriate. No account/API-key fallback or guessed reset timer is added.

Login/device/logout messages are beside account controls; discovery and creation errors are in the new-session area for their selected account. Tool/account override errors are beside their toolbar, advanced session-setting errors in that settings panel, and shared model-definition errors in the model panel. Stop/Continue/compaction/steering errors stay with the captured chat session. Resume-all, inspection and maintenance/export report in their own panels. A delayed response remains owned by the original session/account/model, not whichever selection happens to be visible later. Only bootstrap/disconnection failures use the top connection alert; there is no footer notification sink.

## Settings and inspection

The collapsible settings panel exposes instructions/files, individual tools, concurrency/output limits, declared reasoning options and compaction settings. Only the selected session must be fully idle to save; other sessions may keep working. Saving writes explicit overrides for the displayed fields while preserving other overrides. Toolbar tool changes and advanced settings share one override draft. Account changes use the separate explicit handoff action, not an ordinary override save, and cannot run with unsaved settings edits. **Inherit defaults** resets editable settings but keeps the session's model/account. The exact effective/override JSON is inspectable below the form.

**Model configuration (shared)** edits the API, verified maximum output, local context budget and reasoning efforts/summary support for a configured model. These are user declarations, not upstream guarantees—consult provider documentation before enabling them. A new Responses model begins with a 16,000-token local budget and no assumed reasoning capabilities. A supported Messages discovery selection additionally copies reported maximum output/efforts and the exact native profile's display declaration. No Pi advisory limit becomes executable. **Register model only** in the new-session form installs the selected model without making a conversation; it can then be used for compaction. Provider/model identity cannot be rewritten under an existing alias. API edits still require an explicit idle handoff for already-used chats: the existing account control offers **Rebind model/account** when the protocol changes. Confirm it to create the portable-history boundary, or create a new chat.

**Context / diagnostics / execution records** reads snapshots and paged records without calling a provider. Context includes installed summaries; events include retries, uncertain attempts, hooks and usage; turn records expose terminal outcomes. Runtime diagnostics include tool registration and process-observation failures. Events show up to 100 per page; **Next page** reads more rather than silently truncating a complete record.

Use **Rename**, **Delete session**, and **Export history** for session maintenance. Deletion confirms before removing the session/history and stopping owned background processes; workspace and log files remain. History export follows all pages and downloads visible canonical history, not encrypted provider items or credentials. Treat exports as private conversation data. Stop work before exporting if you want a stable endpoint for the export.

## Where data lives

By default, `~/.sam-g/example/` contains:

- `auth/auth.db` and SQLite sidecars: **private credentials** and auth receipts.
- `chats.db` and sidecars: messages, sessions, execution history and per-session overrides.
- `models.json`: the example host's selected model definitions/budgets/capability declarations, **not** a discovered catalog or credential file.

The directory is created with owner-only permissions outside the checkout. No tokens go into JavaScript, browser storage, chat history or ordinary status responses. Treat the whole directory and its backups as private. Tools running as your OS user can still read it; this example does not add keychain encryption.

The example deliberately uses its own subdirectory, separate from the regular server's `~/.sam-g/auth/` and `sessions.db`, so both hosts can run without competing for database ownership. They do not share saved logins by default.

To choose another location:

```sh
go run ./example -data "$HOME/.sam-g/another-example"
```

Use a dedicated directory, not another app's auth/conversation database. An existing data directory must have owner-only permissions (`chmod 700 /path/to/your/example-directory`). If you change `-data`, you start with that directory's separate accounts and conversations. Do not copy a live database between computers; the library has a separate [move-only credential transfer workflow](../docs/AUTH-MODELS.md#move-a-selected-session-to-another-machine).

To completely remove this example's default local data, sign out in the page first, stop the server, then remove **only** `~/.sam-g/example/`. This deletes local conversations as well as credentials. Deleting files alone does not revoke a remote session or erase backup copies.

### Configuration and restart

The host reads `models.json` **before** opening the runtime, so Resume uses the installed definitions. If the file is absent, startup initializes it from saved session bindings without executing work, redirecting providers or discarding conversations.

Model edits use `UpdateModelsAndSave`: validate definitions and save through an
atomic file replacement before publishing only the model map. Chats may keep
running with captured main/summarizer definitions; the next turn uses the latest
catalog and may need settings repair. Prompts/skills and session settings are not
refreshed. Only candidate construction, file save and model publication share the
host model lock; metadata reads, login and Resume use their existing owners. New
sessions register the host model first and then call `/v1/sessions`. Registration
may succeed even if subsequent session creation fails; the model remains installed.
Rejected definitions leave the file unchanged. Once saving starts, closing the
browser cannot cancel publication. Actual save/publication failure closes the
runtime and reports that the file may contain either version: inspect storage and
restart. Restart/recovery uses latest definitions, not old executable snapshots.
Do not edit the file concurrently with the running host or remove it to hide errors.
Full runtime Reload remains all-idle. See the
[model publication contract](../docs/MODEL-INSTALLATION.md#safe-publication-and-operator-edits).


## Scope and safety boundaries

- Named ChatGPT and Copilot accounts, with multiple saved sessions and same-provider account switching. No multi-user isolation or automatic credential fallback.
- Tools are opt-in per session, with saved permissions and workspace. No sandbox, approval framework or automatic effect rollback.
- Initial model budgets are editable local declarations, not discovered maximums. Each browser message is limited to 8,000 characters, and JSON commands to 64 KiB.
- Local saved state is polled once a second; live activity uses the ordinary SSE API. Only **Load models** fetches a catalog. Send, Continue, guidance on an idle unpaused session, Compact, opted-in automatic compaction and acknowledged Resume can cause model requests. Merely opening the page does not.
- This example exercises the built-in runtime workflow. Custom Go tools/hooks and approval/resource integrations belong in host code, not uploaded JavaScript callbacks. See the [library guide](../docs/LIBRARY.md) for hooks, reload and embedding. API-key/custom-provider setup and local VM credential transfer remain covered by the library/CLI guides rather than secret-entry/export widgets.
- Listens only on `127.0.0.1`. Do not expose it through a public tunnel, reverse proxy or LAN binding. No external scripts, fonts or assets.
- Browser requests use exact Host/Origin checks; the page's host-setup routes additionally require a per-process CSRF token. The OAuth callback is the only cross-site callback and is validated by AuthManager. Refresh the page after restarting the server to get its new setup token. Chat, account status, model discovery, SSE, interruption and sign-out use the same `/v1` HTTP API as other clients. The server now supports same-origin browsers by default; other frontend origins require an explicit allowlist.

## Troubleshooting

| What you see | What to do |
| --- | --- |
| `go: command not found` or wrong version | Install Go 1.27+, reopen the terminal, and check `go version`. |
| `address already in use` | Stop the old example or use `-port 8766`. |
| Database already owned/locked | Stop the other process using this example's data; do not delete lock files from a running instance. |
| Host/origin rejected or request says reload | Open the exact printed `http://127.0.0.1:PORT` address and reload. Do not open the HTML file directly. |
| ChatGPT signed in, but plan permission missing | Sign in again with that label and review plan consent. Permission/eligibility is controlled by OpenAI. |
| Copilot fails after previously using a custom client ID | Sign out that saved account and authorize again using the bundled default, or keep your intentional override. Never enter an access token as the client ID. |
| Copilot device flow/access denied | Check account subscription, organization policy and provider error details. With a client-ID override, check its device-flow setting too. Header parity and device approval are not proof of model access. |
| Browser login failed | Read the status on the page. Cancel a still-pending attempt before retrying. Do not share the authorization or callback URL. |
| No models or a provider error | Copy only the safe error code/message/request ID when asking for help. Do not share `auth.db`, tokens or callback URLs. |
| Wrong identity for saved registration | Retained labels preserve their verified registration. Use a new label for a different account, not the old registration's label. |

## Source files

- `main.go`, `configuration.go`: loopback listener, private data, persisted model configuration loaded before Open, one-time upgrade, and shutdown.
- `http.go`: serves the standard `server` API at `/v1/`, plus host-only model registration, acknowledged Resume, event inspection, browser-login setup and the OAuth callback. Session creation/mutations use `/v1` directly. There is no global selected-chat state. It does not strip browser headers or duplicate chat/auth execution.
- `accounts.go`: provider-qualified model restoration and state-correlated ChatGPT callbacks; Copilot device login uses the ordinary server API.
- `index.html`, `style.css`, `app.js`, `accounts.js`, `live.js`, `settings.js`, `inspector.js`: embedded UI. No frontend build step.

This example has no automated browser, executable or integration suite. The
repository maintains only small unit tests; compiling this host does not verify
its sign-in, chat or restart flows. See the
[test-selection policy](../docs/maintainers/changing-and-testing.md#test-selection).

For API and deployment options, see the [library guide](../docs/LIBRARY.md),
[server guide](../docs/SERVER.md) and [authentication guide](../docs/AUTH-MODELS.md).
