# sam-g library

Package `sam` (module path `github.com/vihanggodbole/sam-g-oss`, import as `sam "github.com/vihanggodbole/sam-g-oss"`) is the reusable public Go API. Its public types and methods are declared directly in the root package, so `go doc github.com/vihanggodbole/sam-g-oss Runtime.StartTurn` and `go doc github.com/vihanggodbole/sam-g-oss Config` show full contracts and fields. It runs persistent agent sessions that call a configured Responses endpoint or native Claude Messages through Copilot and use tools (`read`, `write`, `edit`, `bash`, `history`, `subagents`, plus host tools).

Where things live: sessions, history, turns, and events are in one local SQLite database. Subscription credentials, registrations, plan status and auth receipts are in a separate private SQLite database owned by an `AuthManager`. Workspace files and background-process logs are ordinary files that sam does not copy into the database.

The library opens no network listener and installs no signal handlers. A host (your Go program, or the [`server` package](SERVER.md)) opens a `Runtime`, calls its methods, and closes it.

## A short walkthrough

1. The host calls `Open` with a config listing providers (endpoints), models, and defaults. The runtime locks the database, applies migrations, loads the latest configuration, and reconciles unfinished work as suspended without launching it. Call `Resume` to opt into recovery.
2. `CreateSession` saves a session tied to a workspace directory.
3. `StartTurn` saves the prompt and returns a turn ID right away. A background worker then loops: ask the model, run the tools it requests (in parallel, up to the concurrency limit), feed back the results, and repeat until the model answers without tools.
4. Every step is written to SQLite as it happens: history items, tool-call records, and numbered events. A client reads events with `Subscribe`. If it disconnects, it later subscribes again from its last sequence number and receives what it missed.
5. While the turn runs, `Steer` adds guidance that is applied before the next model request. `Interrupt` stops the session's current work and pauses any guidance that has not been applied. `Continue` resumes.
6. When the context grows large, the runtime summarizes older history (compaction), optionally with a cheaper model.

## Opening and closing

```go
func Open(ctx context.Context, cfg Config) (*Runtime, error)
func (r *Runtime) Close(ctx context.Context) error
func (r *Runtime) Resume(ctx context.Context) error
func (r *Runtime) Ready() error
```

- `Open` validates `cfg`, creates the database directory (mode 0700) if needed, takes an exclusive lock on `<database>.lock` next to the database's real (symlink-resolved) path, migrates, and recovers. `ctx` bounds opening only. A second runtime on the same database, including one reached through a symlinked directory, fails with `conflict`.
  - The path is passed to SQLite as an escaped `file:` URI, so characters such as `?`, `#`, and `%` are part of the filename. The file SQLite opens is always the file the lock protects.
  - A database file with more than one hard link is refused with `invalid_argument`: another name for it would take a different lock. Remove the extra link (or copy the database while no runtime uses it). This is a simple check, not a system-wide inode registry.
  - An invalid host tool registration does not fail `Open`; see [Tools](#tools). Failing to open, lock, or migrate the database does.
- `Close` stops accepting work, cancels and joins active workers, saving eligible work as `suspended` without changing its guidance pause choice, stops background processes started by `bash` in this runtime (see [Tools](#tools)), then closes SQLite and releases the lock. This is **one shutdown operation**, started by the first call; every `Close` call, including concurrent ones, waits for it and returns its result. If exit observation fails and a process group can no longer be safely signalled, `Close` still releases storage after the owner relinquishes it, but returns `process_observation_failed` instead of claiming cleanup succeeded; the command may remain running.
- If `ctx` expires first, `Close` returns a `closed` error and shutdown **continues in the background**: the runtime keeps ownership of the database until every worker has settled and every background process has either stopped/reaped or returned an explicit `process_observation_failed` uncertainty error. It then closes the database and releases the lock. Call `Close` again to wait for the final result. Never reuse a closed runtime.
- Scenario: an embedding app gives `Close` 40 ms, but a dev server ignores SIGTERM. `Close` returns the timeout after 40 ms; about 2 s later the dev server is killed and reaped, then the database is released. Opening the same database in between fails with `conflict`; a second `Close(context.Background())` returns once it is released.
- `Ready` returns nil while new work is accepted, temporary `conflict` during configuration preparation, `closed` once `Close` has started, or the `storage_error` that stopped admission. Contention on a command that was not accepted (`storage_busy`) does not change it.
- `Diagnostics()` returns structured startup problems and asynchronous lifecycle failures: `[]Diagnostic{Category, Code, Tool, SessionID, Message}`. Any bash whose exit can no longer be observed safely adds an error diagnostic with code `process_observation_failed` and its session ID; foreground bash also returns the failure as a tool result.

## Configuration

```go
type Config struct {
    DatabasePath string              `json:"database_path"`
    Providers    map[string]Provider `json:"providers"`
    Models       map[string]Model    `json:"models"`
    Defaults     SessionConfig       `json:"defaults"`
    MaxProviderEventBytes int        `json:"max_provider_event_bytes"` // 0 means 32 MiB
    HTTPClient   *http.Client        `json:"-"` // nil: default client
    Tools        []Tool              `json:"-"` // add tools or replace built-ins by name
    Hooks        Hooks               `json:"-"` // host lifecycle callbacks; see below
    Auth         *AuthManager        `json:"-"` // subscription accounts; see below
    DocumentationDir string          `json:"-"` // optional host-installed docs path
}
type Provider struct {
    BaseURL   string `json:"base_url"`    // versioned base, e.g. https://api.openai.com/v1
    Auth      string `json:"auth"`        // "", "openai-chatgpt", or "github-copilot"
    APIKeyEnv string `json:"api_key_env"` // environment variable holding the key
    APIKey    string `json:"-"`           // direct key for Go hosts; never persisted
}
type Model struct {
    API                ModelAPI `json:"api,omitempty"` // responses (also empty) or anthropic_messages
    MaxOutputTokens    int      `json:"max_output_tokens,omitempty"` // verified bound; 0 unknown
    Provider           string   `json:"provider"`
    ModelID            string   `json:"model_id"`
    ContextTokens      int      `json:"context_tokens"`
    ReasoningEfforts   []string `json:"reasoning_efforts"`
    ReasoningSummaries bool     `json:"reasoning_summaries"`
}
func LoadConfig(path string) (Config, error) // JSON; unknown fields ignored
```

Responses requests use `BaseURL + "/responses"`; the supported Copilot Messages profile uses `BaseURL + "/v1/messages?beta=true"`. A non-empty `APIKey` is used as given; otherwise the variable named by `APIKeyEnv` is read from **this process's** environment when each model request starts. Scenario: you rotate a key. Editing it in another shell or in a service's environment file does not change a process that is already running; restart the server (or, in a Go host, call `os.Setenv` in that process) and the next request uses the new value. `APIKey` is copied at `Open`/`Reload`; change it through an explicit reload. Keys are never stored, returned, or included in events, and they are redacted from provider error messages. A provider with `Auth` set uses login accounts instead (no `api_key_env`), and `Open` requires `Config.Auth` with that login configured; see [Subscription login and accounts](#subscription-login-and-accounts).

Execution budgets/options are declared in config. Explicit provider discovery may report some capabilities, but never installs aliases or guesses missing budgets. For Responses, when a model declares any reasoning efforts or reasoning summaries, requests include `reasoning.encrypted_content` so reasoning items can be replayed statelessly (every request uses `store: false`).

`MaxProviderEventBytes` bounds one assembled provider SSE event (all of its `data:` lines together), protecting memory from a runaway stream. A larger event fails that model request with a descriptive error and is not retried. Raise it if a model legitimately sends larger completed responses (for example big encrypted reasoning items).

**Zero means default.** Numeric settings in `Config` and `SessionConfig` treats 0 as "use the built-in default" (listed next to each field). Negative numbers and percentages above 100 are rejected. `Model.MaxOutputTokens` is metadata: zero means unknown, not a default.

`Open` and `Reload` snapshot maps, slices, tool schemas and hook registrations. Mutating the supplied Go value afterwards has no effect. HTTP clients, AuthManager, tool/hook functions and their captured state remain host-owned implementations. Auth and subscription requests use shallow HTTP-client copies that refuse redirects; sam does not mutate or close the caller's client/transport. Environment credentials remain request-time values, not prompt reload triggers.

Documentation is also host-owned. A host that wants installed, version-matched reference files explicitly calls `sam.InstallDocumentation(root)`, then sets the returned absolute path in `Config.DocumentationDir` before `Open` or `Reload`. The helper publishes an immutable content-addressed snapshot and never replaces a conflicting directory. `Open` itself does not discover a home directory or write docs. The stock executable installs its embedded snapshot under `~/.sam-g/docs/` before opening the runtime; a copied binary therefore works without a checkout or a particular working directory. A custom Go host may omit docs, or choose and own a different install location.

### Unified configuration reload

```go
func (r *Runtime) Reload(ctx context.Context, cfg Config) error
```

Scenario: edit project instructions and replace a Go tool implementation. Supply the complete typed `Config` to `Reload`. It refreshes prompt contents, provider/model/tool definitions, implementations and hook registrations together for existing and new sessions. There is no separate prompt/tool registry or HTTP code-upload API. The executable exposes this host operation with SIGHUP; a custom Go host calls it directly, even without HTTP.

Every session must be **fully idle**, including suspended executions, stopping workers, automatic compaction and immediate queued follow-ups. Resume unfinished work or Interrupt it before reloading; a genuine process restart instead loads the latest configuration even with unfinished work. A busy reload returns `conflict`, without queueing or interrupting work. Preparation reserves admission, but file reads run outside runtime/SQLite locks; read APIs can still inspect the old installed view. A preparation error leaves installed prompts/settings/tools unchanged. Publication persists all effective session settings atomically while new work is excluded. Concurrent configuration preparation also returns `conflict`; retry after it finishes. `Close` can begin during preparation and prevents publication.

`DatabasePath` cannot relocate an already-open database. Other config fields, including host-supplied HTTPClient/Auth references, may be replaced when idle; the host must keep any old resources still used by its background work alive. Reload never closes host handles or kills acknowledged background processes. Used chats pin their model/account choices. Removing a model leaves history readable, not redirected. Newly introduced binding redirection rejects reload. A binding already incompatible after reopen remains read-only for work but does not block unrelated settings repairs when its definition is unchanged. Credential refresh remains independent.

Reload has no durable request ID: Go function replacements are not JSON-serializable requests. Session mutations still have their ordinary deduplication. No-op reload preserves model-visible instructions and ordered tool definitions; real changes may alter cache reuse. History is never rewritten. Each installed session receives a `config.changed` event, even on an unchanged reload.

### Session configuration

```go
type SessionConfig struct {
    Model                  string   `json:"model"`
    ReasoningEffort        string   `json:"reasoning_effort"`   // must be in the model's list
    ReasoningSummary       bool     `json:"reasoning_summary"`  // model must declare support
    AppendInstructions     string   `json:"append_instructions"`
    AppendInstructionsFile string   `json:"append_instructions_file"` // relative paths resolve against the workspace
    DisabledTools          []string `json:"disabled_tools"`
    ToolConcurrency        int      `json:"tool_concurrency"`   // 0 means 4, per session batch, across all tools
    ToolOutputLimit        int      `json:"tool_output_limit"`  // 0 means 262144 bytes per call
    Account                string   `json:"account"`            // login account, for providers with auth set
    Compaction             CompactionConfig `json:"compaction"`
}
type CompactionConfig struct {
    Model          string `json:"model"`           // "" means the session's bound main model
    Account        string `json:"account"`         // "" means the session's account when both use the same login provider
    Prompt         string `json:"prompt"`          // inline prompt, or
    PromptFile     string `json:"prompt_file"`     // contents prepared at setup/reload (not both)
    DisableAuto    bool   `json:"disable_auto"`
    OutputReserve  int    `json:"output_reserve"`  // 0: a Messages model's max output up to 128000 and context/2, else min(8192, context/4); must be below each model's context
    TriggerPercent int    `json:"trigger_percent"` // 0 means 90 (% of input budget)
    RecentPercent  int    `json:"recent_percent"`  // 0 means 20 (minimum % of input budget kept verbatim)
}
```

- `SessionConfig` is concrete effective settings and the runtime defaults type. `CreateSessionRequest.Config` is `*SessionOverrides`; `UpdateConfigRequest.Config` is `SessionOverrides`. Each ordinary field is a pointer to the corresponding `SessionConfig` field type; `Compaction` is `CompactionOverrides`, with individually optional pointer fields. Nil (JSON absent or null) inherits. Explicit `""`, `false`, `0`, and `[]` override. Numeric zero uses the existing built-in numeric default, not the shared value. An explicit empty main model is invalid.
- `GetConfig` returns `SessionSettings{Effective SessionConfig, Overrides SessionOverrides}`. Effective includes built-in numeric defaults; Overrides preserves intent. Edit **Overrides**, not Effective, then pass that override set to UpdateConfig. PUT replaces the entire override set: omitting a field removes its override and resumes inheritance.
- `UpdateConfig` shares the target-only preparation path with `ReloadSession`: **only that session** must be fully idle. Its override set, effective settings and prompt resources refresh atomically; other chats and the shared setup cache are untouched. File reads do not block ordinary work; target idleness and any newly bound model/account selection are rechecked at publication. An identical accepted request retry returns its original acknowledgement without refreshing files again.
- Prompt fields inherit independently: to replace an inherited compaction prompt file with inline text, set both `Prompt: new("...")` and `PromptFile: new("")`; configuring both alternatives remains invalid.
- Shared defaults apply to existing sessions wherever they inherit. Legacy full saved configurations are migrated conservatively to explicit overrides for every field, including old zero/empty values; intent cannot be reconstructed by comparing defaults. History is untouched. Remove individual fields from Overrides (or PUT `{}` to remove all) to opt into current shared defaults.
- **One model/account handoff.** Once a chat is used, `UpdateConfig` cannot change either its main model or account. `SwitchModel` is the explicit target-session-idle path for both, including account-only changes. The first binding pins the existing choices into overrides; reopen pins legacy inherited choices from their last saved configuration before applying current defaults. Other settings still inherit normally.
- **Explicit model binding.** The first accepted message pins alias, provider, base URL, API model ID and effective API. Ordinary `UpdateConfig` cannot redirect a used chat. `Runtime.SwitchModel(ctx, SwitchModelRequest{RequestID, SessionID, Model, Account})` explicitly changes future work when the target session is fully idle (including no stopping workers or immediate follow-ups). Unrelated sessions may keep working. It preserves history and records the handoff. Old tool protocol items and exposed reasoning summaries become portable read-only context; encrypted reasoning, native signatures and redacted blocks are not forwarded. Reasoning requests reset; other settings are retained. Missing models leave chats readable with `Session.ModelError`, not deleted. See [handoff and installation details](MODEL-INSTALLATION.md).

A Go GET/edit/replace flow (only the selected session must be fully idle):

```go
settings, err := rt.GetConfig(ctx, sessionID)
if err != nil { return err }
settings.Overrides.Account = new("work") // explicit choice
settings.Overrides.AppendInstructions = nil // remove override, inherit
_, err = rt.UpdateConfig(ctx, sam.UpdateConfigRequest{
    RequestID: "config-work-1", SessionID: sessionID, Config: settings.Overrides,
})
```

### Reload one chat's prompt files

`Runtime.ReloadSession(ctx, SessionRequest{RequestID, SessionID})` returns
`SessionSettings` and rereads only that chat's configured append-instruction and
compaction prompt files, workspace instructions and skill catalog. Only the target must be fully idle (including no
suspended execution or stopping/follow-up worker); other chats may keep running.
Settings, overrides, model/account selection, pause state, and historical turns
are unchanged. The next turn/compaction uses the refreshed contents. A
`config.changed` event (`reason: "reload"`) and request receipt commit with the prompts and resource snapshot. Retrying an
accepted ID returns its receipt without rereading files; use a new ID for a new
reload.

Preparation reads outside runtime/SQLite locks and rechecks the target's idleness
before publication. New work may start during preparation; if the target is then
busy, reload returns `conflict` without changing its prompts. Missing/invalid
files also preserve the installed text. Concurrent configuration preparation can
return `conflict`; retry after it finishes. No inference is performed.

This does not read the host JSON configuration, refresh tools/hooks, or update the
shared path cache: other and newly created chats continue using previously
installed shared contents until a global reload/restart (or their own explicit
chat reload). File paths come from the chat's current effective settings; relative
paths resolve against its workspace. The built-in `prompts/system.md` is embedded
in the binary, not a runtime file: changing that source requires rebuilding and
restarting. For editable server-side rules, configure `append_instructions_file`
and reload the chat after editing that file.

### Native Messages through Copilot

Set `Model.API` to `APIAnthropicMessages` (`"anthropic_messages"`). Empty API or
`"responses"` remains Responses. Unknown APIs and unsupported auth/API combinations
fail definition validation. Initially Messages supports only Copilot's exact
`claude-opus-5.5` adaptive profile with a verified positive `max_output_tokens`;
no Anthropic API key, Claude Code OAuth, Pi process or older budget-thinking model.
[Supported profile and catalog mappings](MODEL-METADATA.md#native-messages-support)
is versioned separately from live acceptance.

- **Output:** the operation's existing `compaction.output_reserve` becomes
  Messages `max_tokens`, including thinking. Zero uses the model's verified
  maximum output, at most 128000 and half its context, separately for main and
  summary models, so replies and thinking are not cut short by a small default;
  for Responses models zero stays `min(8192, context_tokens/4)`, which only keeps
  input room. A configured result must be positive, below context capacity and
  at most that model's verified maximum output. Invalid settings fail; there is
  no clamp. Responses requests still have no hard output cap.
- **Thinking:** always `thinking.type=adaptive`. Empty effort omits
  `output_config.effort` for the provider default; nonempty effort must be declared
  by the installed model and one of low/medium/high/xhigh/max. Off/minimal fail.
  `reasoning_summary: true` requests `display: summarized`; false requests
  `display: omitted`, not thinking disabled. Signatures/redacted blocks remain
  private replay data regardless of display. Compaction requests use adaptive
  thinking with omitted display and provider-default effort.
- **Roles and inputs:** ordered leading application instructions and skill catalog
  become system text blocks. Historical developer instructions become native
  system messages in their original position, not user data; BeforeRequest data
  retains its user-shaped application provenance. Text, user images (as base64
  image blocks) and local tools only: other images,
  citations, server tools, unknown blocks and unsupported roles fail before sending
  or accepting the response. Instructions/messages interrupting the required
  tool-use/result adjacency fail before sending rather than being moved or dropped.
  A saved tool result carries its exact ID, actual text and error status.
- **Completion:** only complete validated `message_stop` after a terminal delta
  can authorize tools. EOF, malformed known events, incomplete JSON, output-limit
  and unsupported stops never execute calls. Refusal remains a saved visible
  refusal, not a blank success. Known terminal usage is retained on failure;
  input includes ordinary + cache-read + cache-write tokens only when all are
  reported. Unreported counters, including reasoning, remain null.
- **History and identity:** ordinary text, exposed thinking summaries and literal
  tool arguments have one authoritative copy in the existing canonical items.
  Extra signatures/redacted payloads, caller metadata and complete-response grouping
  remain private; public history/search/events, hooks and exports exclude those
  fields. Context estimates size the projected native blocks, not redundant storage.
  Same-model continuation replays complete signed/redacted blocks unchanged and
  in order; compaction never splits their response/tool-result group. An explicit
  handoff converts older context to readable text and never revives opaque data,
  even when switching back. The binding includes the effective API; an omitted
  API means Responses.
- **Recovery:** one executor, HTTP/auth owner and four-attempt budget serve both
  codecs and mixed main/summarizer APIs. Active work keeps captured definitions;
  restart uses latest configuration. Changed API bindings require idle SwitchModel.
  A provider's generic Messages 400 stays a visible provider failure with repair/
  handoff guidance; signatures are not silently dropped or recognized by prose
  matching. Instruction/credential changes may invalidate signed context; use an
  explicit idle handoff or a new chat. Unknown external effects may repeat on
  recovery; saved outcomes do not. No live proxy compatibility is promised yet.

### Instructions

The built-in system prompt (`prompts/system.md` in the source checkout) includes concise file-tool preference guidance; it is advice, not routing or authorization. Custom prompts/tools remain host-owned.

Prepared instructions are built in this order: the embedded default prompt (`prompts/system.md`, which lists the workspace, enabled tools and—when the host supplies it—the installed documentation location), then `AppendInstructions`, then the contents of `AppendInstructionsFile`, then workspace-root `AGENTS.md` (or `CLAUDE.md` only when AGENTS is absent).

The stock binary installs a content-addressed snapshot of the top-level user/runtime Markdown guides (including deployment, architecture and known limits) and selected Go examples. Changed bundled resources get a new directory; unchanged resources reuse the verified snapshot. Older snapshots stay available, so an already-running older binary keeps the path its prompt advertises. Startup fails before opening the runtime if the stock host cannot publish or verify its docs directory. The library does not install docs unless the host explicitly calls the helper. The bundle is read on demand with `read` or `bash`; when both tools are disabled the agent cannot inspect it. The bundle excludes implementation source and maintainer guides, and links to those files require a source checkout. Hosts without a bundle get only a short unavailable notice, not the topic guide for files they cannot provide. It does not fetch online docs or update itself.

Configured append/compaction file contents are prepared at Open, initial workspace/session setup, explicit unified reload, and per-chat `ReloadSession`—not during turns, compaction or inspection. Absolute shared paths are prepared even before any sessions exist. Relative shared paths are prepared when first used by an inheriting session in a workspace; an explicit path override does not require an unused relative default file to exist there. The installed configuration owns text by resolved absolute path; another session using an already prepared path reuses that content until reload. New workspace/path setup may read new resources, but cannot refresh existing ones. Shared paths and explicit overridden paths use the same helper. Open/reopen refreshes files before exposing the runtime; legacy choices remain explicit.

Scenario: delete the append file after setup. Turns, compaction and inspection still use the installed text. Reload fails synchronously with `invalid_argument` and leaves that text installed. Restore the file and explicitly reload to install its new contents. Initial setup fails synchronously if required files cannot be prepared.

Each turn saves its initial base instructions/settings; each compaction saves its initial prepared summarization prompt as historical facts. `InspectContext.InstructionsState` is `prepared` when a worker has a live prepared request, otherwise `base_only` for installed config. Inspection uses the active work’s captured main definition and current main settings/base, never a summarizer prompt or the historical original turn snapshot. Suspended work uses the latest installed base plus committed context. It performs no file reads. Estimates include retained history, fixed protected user, summary, instructions, the prepared skill catalog and tools, but not uncommitted pending guidance or provisional streamed output; they are conservative, not the exact next payload.

For a status such as `ctx ~130k/264k · compact near ~237k`, use
`ContextInfo.EstimatedTokens / InputBudget`, not total `ContextTokens`.
`InputBudget` subtracts `OutputReserve`; `TriggerTokens` uses the configured
trigger percentage with integer division. For context 272000, default reserve
8192 and trigger 90%, these are 263808 and 237427. `AutoCompaction` reports the
enabled setting; `ModelError` or `CompactionError` reports a current local
model/settings selection failure (no account/provider probe). Active work uses
its captured definitions for this check; idle/suspended work uses latest
configuration. Show disabled/unavailable states rather than a working threshold
when appropriate. The threshold is checked **before requests** and only when
older history can be summarized; crossing it alone does not cause immediate
compaction. See [compaction](#compaction) for retention and fit behavior.

### Workspace instructions and skills

Only the workspace root is searched for `AGENTS.md`, with `CLAUDE.md` as fallback
when AGENTS is absent. No ancestors or nested instruction files are searched.
An empty AGENTS file still wins; unreadable, invalid UTF-8 or broken-symlink
selected files reject setup/reload rather than silently using CLAUDE.

Skill roots are on the **server filesystem**:

1. The server user's `~/.agents/skills/` (global).
2. `Config.SkillDirectories` / JSON `skill_directories`, in configured order
   (extra). Absolute paths stay absolute, `~/` uses the server user's home,
   and other relative paths resolve against each session's workspace.
3. `<workspace>/.agents/skills/` (workspace). Only this root's subtree is
   searched, never ancestor `.agents` directories.

For example, `"skill_directories": ["~/.pi/agent/skills", ".claude/skills", "/srv/team/skills"]`
uses those existing collections without copying or automatically loading other
cross-tool locations. Grouping directories are recursively traversed in lexical directory order.
Traversal stops at a directory containing SKILL.md: its scripts/references/assets
(including nested example SKILL.md files) belong to that skill, not the catalog;
symlinks to roots, directories and SKILL.md files are followed, with resolved
paths preventing directory cycles. This is not a filesystem sandbox. Reported
skill paths are resolved physical paths. Missing roots are normal; unreadable
paths, malformed entries, duplicates and shadowing appear in diagnostics.

A skill directory contains `SKILL.md` with YAML frontmatter delimited by `---`.
The maintained `go.yaml.in/yaml/v3` parser supports quoted/multiline descriptions
and nested metadata. A valid `name` is 1–64 lowercase ASCII letters, digits or
single separating hyphens; `description` must be nonblank and at most 1024 Unicode
characters. Unknown metadata is ignored; `allowed-tools` never grants authority.
`disable-model-invocation: true` entries are skipped with a diagnostic because
explicit skill commands are not provided. Workspace skills override global/extra
skills of the same name, even if both reference the same physical file. Otherwise
the first root/lexical path wins. The final catalog is sorted by name.

Workspace instructions and skill catalogs are read together afresh at new session
setup, reopen and explicit reload, then frozen per session. Unlike the existing
configured append/compaction file cache, new setup never reuses stale discovered
instruction contents while presenting freshly scanned skill metadata. Per-chat
reload does not modify any other chat or the shared configured-file cache.
Discovery diagnostics are nonfatal for skills; failure reading selected workspace
instructions or required configured files rejects the whole candidate atomically.
No file watchers, automatic scripts, packages, installers or remote prompts exist.

Only skill names, descriptions, paths and scopes are offered to the model. Full
bodies are read on demand using enabled `read` or `bash`, and remain ordinary
chronological tool results. With neither tool enabled the skill catalog is not
offered; inspection reports the discovered skills and an unavailability diagnostic.

The Responses request keeps stable system/host/workspace text in `instructions`
and native tool definitions in `tools`. The skill catalog is a stable leading
**developer input** item before growing conversation history, not appended to
leading instructions, copied into saved conversation, or promoted from tool
results. No timestamps, reload counters or UI notices enter this prefix. An
unchanged reload preserves model-visible bytes/order. Provider-internal tool
serialization and token-prefix/cache ordering are not controllable: JSON field
order is not a token-order guarantee. Unit tests do not establish end-to-end auth
profile compatibility, live-provider acceptance or cache-hit behavior.

`SessionSettings.Resources` and `ContextInfo.Resources` expose the same durable
`PreparedResources` snapshot used for prompt construction: enabled tool names,
instruction source kinds/paths (built-in, inline, file, workspace), selected skills,
diagnostics and `skill_catalog`. Reads never scan the filesystem. This is the
current installed setup, not a historical record of every prior setup. Setup/reload
cards belong to clients, never synthetic conversation messages.

## Lifecycle hooks and host extensions

Scenario: one extension supplies project rules, another adds issue details, and a deployment policy asks your application's user before a command runs. Each independently appends typed registrations to `Config.Hooks`; sam waits outside its locks. Your application owns the approval UI and replies, not the HTTP server.

```go
type Hook[I, O any] struct {
    ID string
    Handle func(context.Context, I) (O, error)
}
type Hooks struct {
    BeforeTurn       []Hook[BeforeTurnInput, string]
    BeforeRequest    []Hook[BeforeRequestInput, string]
    BeforeTool       []Hook[BeforeToolInput, HookDecision]
    BeforeCompaction []Hook[BeforeCompactionInput, CompactionDecision]
}
// HookDecision{Action: HookAllow} or {Action: HookDeny, Reason: "public-safe reason"}
// CompactionDecision also has AppendInstructions, allowed only with HookAllow.
```

Use `go doc github.com/vihanggodbole/sam-g-oss Hooks` and `go doc github.com/vihanggodbole/sam-g-oss BeforeTurnInput` (or the other stage input types) for the complete field declarations. `HookScope` carries session, turn, operation, step, attempt, workspace, model alias and account name, never credentials. Attempt is the prospective provider/tool attempt ordinal (zero for BeforeTurn), not an exactly-once callback token. Turn/tool/request operations use the turn ID; compaction has its own ID. Steps are one-based for requests/tools, zero before a turn and for manual compaction. BeforeTurn gives this turn's genuine accepted user/steering messages; BeforeRequest gives committed summary/history high-water and detached tool schemas, not a transcript copy. Read paginated History if needed.

- Registrations require nonblank valid-UTF-8 IDs unique **within each stage**, and nonnil callbacks. Invalid hooks reject Open **before database ownership/recovery**, or reject Reload without publication. This is deliberately stricter than invalid custom tools, which retain their skip/disable diagnostic policy.
- Handlers run serially within one stage with **no promised order**; each gets a detached copy of the same input. Other sessions and tool calls can overlap. The first deny/error/invalid result stops dispatch. Every decision must explicitly allow; zero/unknown actions and contradictory reason/text are failures. Empty handler sets allow. No priorities, approval cache, timeout framework, panic recovery or abandoned callback goroutines.
- BeforeTurn runs once per accepted new turn source, including follow-ups; its text stays private until the first conversational request accepts it. Its known size counts in the ordinary automatic-compaction trigger and resulting-context fit, but it is not sent to the summarizer as committed history. BeforeRequest runs once after normal guidance/automatic compaction preparation, then guidance arriving during its wait is drained once before final assembly. Live retries keep their accepted input frozen and invoke no hooks. Process-loss recovery re-prepares with current BeforeRequest/BeforeCompaction callbacks; replay-allowed unfinished tool calls pass current BeforeTool again; recovery-skipped calls do not. BeforeTurn repeats only until its initial acceptance (including an empty result) commits. Inspection and event/command replay invoke no hooks.
- Nonempty new text enables a **whole-input** conservative fit check (base, tools, summary, protected user, history, latest applied guidance and new contributions). Oversize is actionable `context_limit`, with contributor IDs but no rejected text or fabricated failing handler. Invalid UTF-8 is `hook_failed`. Empty callbacks and older saved notes do not add an unconditional new budget gate. No clipping, deduplication or hook/compaction retry loop rescues candidate text.
- Accepted text and source/audit references are appended atomically **before sending**. They survive subsequent auth/provider/cancellation failure, even if no HTTP request was delivered. Failed preparation discards only unaccepted candidates, without falsely blaming earlier successful callbacks. History is never rebuilt from today's registrations.

### Extension history and authority

`HistoryItem.Kind == "extension"` and `Source *ExtensionSource{Stage, HandlerID, OperationID, Step, Attempt}` identify application content. Source fields use JSON `stage`, `handler_id`, `operation_id`, `step`, and optional `attempt`. The exact accepted canonical message JSON is stored once and reused unchanged: `message` with `input_text` content, a fixed label, two newlines, and the verbatim body. IDs in labels are JSON-quoted.

| Stage | Saved role and label |
| --- | --- |
| BeforeTurn | developer: `[Application extension instructions; handler=ID; stage=before_turn]` |
| BeforeRequest | user: `[Application extension data (not a human message); handler=ID; stage=before_request]` |
| BeforeCompaction | developer: `[Application extension summarization instructions; handler=ID; operation=OPERATION; scope=this summarization operation only]` |

No role fallback is attempted if a provider rejects developer messages; live provider acceptance is unverified. User-shaped extension data is **not genuine human provenance**: it cannot become a protected-user anchor or a Copilot user initiator. Initiator uses genuine canonical sources, not the last wire role (including old notes/summary/protected replay). `InspectContext.Instructions` remains the real leading base; saved notes count in its retained-context estimate and are readable/searchable with their source. Pending candidates and unapplied guidance are not claimed sent. Returning `sup` again appends another `sup`; returning empty leaves earlier copies alone.

### Tool and compaction decisions

BeforeTool waits while the original call is **planned**, occupying its ordinary tool-concurrency slot. Availability is checked first; valid non-object custom arguments remain supported, with exact inspected bytes. A denial saves `tool_denied` as that call's normal failed result, so the model can adapt. A real hook failure latches its originating `hook_failed` before cancelling a **batch child context**, stops scheduling and joins started calls. Completed writes, success after cancellation and acknowledged background launches survive. Interrupt/shutdown still takes settlement precedence. Intent and Interrupt are serialized; a final cancellation check can record known `not_executed` after intent. Arbitrary Go Run code must cooperate after that check; no zero-effect guarantee exists.

BeforeCompaction runs after normal preparation/no-work checks, against the frozen cut, model/account and prompt. It may append operation-scoped instructions or deny, never replace the summary/algorithm/selection. Denial pauses/stops manual or automatic work, with no request/install/follow-up. Accepted instructions stay **after** the frozen cut in main history, and the summarizer consumes those same messages. Summarizer input-fit and resulting main-context fit include them; the latter also includes pending BeforeTurn text. Failed requests/result-fit/install retain accepted entries and preserve the previous installed selection. Manual compaction never simulates BeforeTurn. Compaction denial explanations remain private operation failures, not temporary future model notes.

`hook.result` uses `HookResultPayload`: `stage`, `step`, `outcome`; reached decision `handlers: [{id,action}]`; accepted `contributions: [{id,history_seq}]`; mixed-budget `budget_contributors: [{stage,id}]`; direct `stop_handler` and `code`. Envelope IDs supply operation/call context. It is a diagnostic, not proof of delivery/execution. No copied text, timing trace, text attendance roster or empty-success text record. Cancelled decision stages retain reached valid decisions, ignoring late returns. Callback reasons/errors must be public-safe; configured-key redaction cannot discover arbitrary host secrets.

Callbacks may use read APIs, but must not synchronously Close/Reload or wait for their own operation to become idle. They must honor ctx; Close retains its ordinary deadline/join semantics. Required persistence failures use the existing runtime fail-stop. Host callback side effects cannot be rolled back or promised exactly once. A custom host may pass the runtime to `server.New` without changing these semantics.

## Subscription login and accounts

Use direct SIWC (`AuthOpenAIChatGPT`, `"openai-chatgpt"`) or Copilot (`AuthGitHubCopilot`, `"github-copilot"`). `AuthManager` owns a **separate private SQLite database** for registrations, credentials, plan blocks and receipts. The [authentication/discovery guide](AUTH-MODELS.md) owns the complete protocol, API, storage and transfer contract.

Scenario: run `sam-g auth login` on the laptop whose browser receives the loopback callback, move the selected protected registration to the stopped VM host, then let the VM refresh it on demand. OpenAI uses a browser SIWC callback. Copilot retains device login; `GitHubCopilotAuth{}` defaults to the public Copilot Plugin client ID used by Pi, with matching Copilot headers and an optional explicit ClientID override. No live provider acceptance is claimed.

```go
auth, err := sam.OpenAuth(sam.AuthConfig{
    DatabasePath: "/home/me/.sam-g/auth/auth.db",
    OpenAI: &sam.OpenAIChatGPTAuth{AgentName: "sam-g"},
})
if err != nil { return err }
defer auth.Close() // close runtimes using it first
cfg.Auth = auth
```

| Operation | Contract |
| --- | --- |
| `StartLogin(ctx, LoginRequest)` | Durable RequestID/Provider/Account plus OpenAI RedirectURI and explicit EnablePlanUsage. Host binds literal 127.0.0.1 first. Returns authorization_code URL or Copilot device_code challenge. |
| `CompleteLogin(ctx, CompleteLoginRequest{LoginID, CallbackURL})` | Strict one-shot callback validation. After acceptance the manager owns exchange/persistence; caller disconnect stops only waiting. |
| `LoginStatus`, `Logins`, `WaitLogin`, `CancelLogin` | Safe state; terminal/reopened browser status contains no reusable URL. Browser attempts wait until cancellation/Close; interrupted attempts never resume through an old request ID. |
| `Accounts()` | Retained registrations, current status/identity/expiry, plan permission/block/reset facts and safe pending/last transfer information; never tokens. |
| `Logout(ctx, AuthAccountRequest)` | Commits unusability before remote revocation and joins owned work. Returns LocalSignedOut, confirmed/unconfirmed/not_applicable remote disposition and warning. Cancellation cannot undo accepted local clearing. Direct callers choose a deadline. |
| `AllowPlanUsageRetry(ctx, AuthAccountRequest)` | Explicitly clears the local plan block, not scope, quota or a turn. Old receipt replay cannot clear a new block. |
| `ExportAccount`, `ImportAccount` | Local protected move-only bundle; source disabled, no remote revoke, transfer-ID stale replay protection. No HTTP secret transfer. |
| `Close()` | Cancels/joins owned work, closes auth SQLite and releases ownership. Concurrent callers join one result. |

Tokens refresh through one per-account coordinator and commit atomically before use (SIWC at expiry; Copilot within its five-minute renewal margin). Actual SIWC rotation is marked uncertain durably before provider I/O. Only validated saved credentials or a definite token-endpoint rejection resolve that attempt's marker; older uncertainty and separate authorization uncertainty are preserved as described in the [auth guide](AUTH-MODELS.md#automatic-on-demand-refresh). The initiating request controls refresh I/O; manager retirement can cancel/join it. Refresh never reactivates a retiring generation. Copilot retains one forced 401 refresh cycle within the existing four-attempt ledger; SIWC does not treat every 401 as forced refresh. Exact provider limit codes precede generic backoff. Plan blocking is shared through the manager and survives restart/refresh/reauthorization; unknown reset time remains nil.

Each live turn pins its account generation across steps, tools and compaction. A genuine runtime reopen repins current login without resetting attempts. Re-login/logout cannot silently switch a live turn. A SwitchModel on a fully idle target session selects another account for future work and makes earlier context portable, just like a model change. A saved account never silently becomes API-key billing. Reauthorization under the same label is a separate auth action, not a chat handoff; if that provider rejects old opaque history, an explicit same-model/account handoff can retire that opaque replay without changing saved history.

New SIWC conversations use public Responses and the `sam` tool namespace. Validated dispatch authorization is saved atomically with the completed output, so restart cannot reinterpret `other.read` as local `read`. Existing flat calls remain unnamespaced. Old endpoint-bound conversations are never silently redirected. An explicit SwitchModel can select a supported model/account while preserving history; incompatible message roles or provider restrictions still fail honestly.

## Provider model discovery

```go
models, err := sam.DiscoverModels(ctx, sam.DiscoverModelsRequest{
    Provider: cfg.Providers["chatgpt"], Account: "work", Auth: auth,
    HTTPClient: cfg.HTTPClient,
})
models, err = rt.DiscoverModels(ctx, "chatgpt", "work") // thin alias adapter
```

One explicit GET after normal credential resolution, no Runtime required standalone, no catalog cache/persistence/retries/inference/enablement/config mutation. Subscription account is required; API-key discovery rejects Account. Results preserve order and exact IDs. Provider capabilities remain nullable; a separate optional `ModelReference` supplies versioned, advisory Pi metadata without changing provider facts, model availability or execution settings. See [field mappings and reference sources](MODEL-METADATA.md). Copilot direct-route entitlement remains unverified. Fixed 32 MiB catalog-envelope bound is separate from SSE limits. See [normalization and evidence limits](AUTH-MODELS.md#discover-upstream-models-without-installing-them).

`Models()` remains installed configured aliases. Discovery does not install or switch them.

To install a discovered model, a host converts it with `ConfiguredModel(provider, account, discovered, contextTokens)`, the same rules the stock server's catalog installer uses: `contextTokens` 0 takes the reported window (or `FallbackContextTokens`), a positive budget is capped by it, and a model this runtime cannot use keeps its reason in `Unavailable` rather than being dropped.

`UpdateModels(ctx, map[string]Model)` replaces only definitions for new work; no
session must be idle and no prompt/skill files are reread. Running turns and manual
compactions retain their admitted main and summary definitions, budgets and
capabilities in memory. Definitions are validated independently of existing chats;
new work checks the latest selection, reasoning capabilities and budget, reporting
required repairs without changing model/account/billing or chat settings. Recovery
uses latest configuration, not a historical executable snapshot.

The stock host's snapshot installation uses `UpdateModelsAndSave(ctx, models, save)`.
It validates, saves host JSON outside the state lock, then publishes. Work/read/Stop
remain available during save, using the old catalog until publication. Once save
starts, caller cancellation no longer cancels publication. A real save/publication
failure seals admission before releasing preparation ownership, then closes the
runtime because the file may contain the new models. Explicit Close still wins.
Full `ReloadAndSave(ctx, cfg, save)` retains its all-idle boundary for whole-runtime
changes; ordinary Go hosts may use Reload without saving. See
[host persistence and HTTP installation](MODEL-INSTALLATION.md).


## Images

`StartTurnRequest.Images` sends pictures with a prompt (the prompt may then be empty): at most 4, JPEG, PNG, GIF or WebP, 3.75 MiB together, each checked against its declared type. They are saved in history as `input_image` parts with data URLs and sent with every later request, so the model keeps seeing them until compaction summarizes the message as `User: … [image]`. Context estimates count each image as about 1,500 tokens, not its encoded size. `HistoryItem.Images` and `TurnStartedPayload.Images` give the count; image data is not returned by history. A model whose provider reports no vision support (`Model.NoImages`, set by `ConfiguredModel` from `DiscoveredModel.SupportsImages`) refuses such prompts with `invalid_argument` before anything is saved. Images cross `SwitchModel` with their messages; a model that cannot read images (after a switch, or once its catalog entry says so) gets `[image]` notes in their place instead. Steering, Continue and subagent prompts carry text only: send images when the session is idle.

Images are sent again with every later request until compaction, while the estimate counts each as about 1,500 tokens, so a conversation with many large pictures can grow beyond what a provider accepts in one request before compaction triggers. We do not guard against that: shrink pictures before sending (screens and photos read well at about 1,600 pixels), and if a provider rejects the request size, start a new session.

## Errors

Synchronous failures from `Runtime` and `AuthManager` methods are normally a `*sam.Error` with a stable `Code`. Exceptions: when the caller's `ctx` is cancelled or its deadline passes, a method may return `context.Canceled`/`context.DeadlineExceeded` (possibly wrapped) instead; `Output` writes past the limit return `ErrOutputLimit`. Use `sam.ErrorCodeOf(err)` (empty for non-sam errors) together with `errors.Is(err, context.Canceled)` rather than assuming one type. Codes:

| Code | Meaning |
| --- | --- |
| `invalid_argument` | Bad request, config, missing request ID, unreadable configured file, bad cursor |
| `not_found` | Session or requested file does not exist (also ends subscriptions when a session is deleted) |
| `conflict` | Busy session, bound model, request ID reused with different arguments, database owned by another runtime, nothing to compact |
| `unsupported` | Requested reasoning option not declared for the model, or a file is nonregular / not supported UTF-8 text |
| `file_too_large` | Human file read exceeds its per-open byte limit; no partial content returned |
| `file_read_failed` | OS file read failed, for example permission denied; not a persistence failure |
| `provider_error` | Model request failed (in turn/compaction events), or a login provider answered unexpectedly |
| `auth_required` | A login account is missing, logged out, logged in again mid-operation, of the wrong provider, or its credentials were rejected or could not be refreshed. Log in again or select another account. |
| `history_incompatible` | The selected account or route rejected required saved history (opaque reasoning). History is unchanged; the session pauses. |
| `context_limit` | New hook contributions do not fit the whole input, compaction input does not fit, or the summary does not reduce/fit the context |
| `hook_failed` | Host callback failed or returned invalid UTF-8/decision; stage and handler identify the source |
| `tool_denied` | BeforeTool refused the call; saved as a failed tool result so the model can adapt |
| `compaction_denied` | BeforeCompaction refused summarization; the operation stops and guidance pauses |
| `storage_error` | Persistence failed. The runtime stops admission and cancels active work. |
| `storage_busy` | The database stayed locked by another connection for the 5 s busy wait while accepting a new command. Nothing was accepted and the runtime stays healthy: retry the same request ID. |
| `process_observation_failed` | The OS could not confirm child exit/ownership (for example `ECHILD`/`ESRCH` under host auto-reaping); no further signals are sent, and cleanup is unconfirmed. |
| `closed` | Runtime closed or closing |

Errors may include `ProviderFailure`: upstream HTTPStatus, exact safe Code/Param/RequestID, safe Message, Shape, Retryable, nullable RetryAfterSeconds and verified ResetAt. This payload survives refresh/compaction wrappers, exhausted Copilot 401 handling and terminal/attempt events; no raw response body is exposed. SSE errors carry the actual transport HTTP status, which can be 200 despite a failed provider operation; unknown status is not formatted as HTTP 0. Generic Retry-After is not quota-reset evidence. `sam.ErrorCodeOf(err)` extracts the outer code. Failures after work was accepted appear in events (`turn.failed`, `compaction.failed`, with a `code` field) and on the turn record, not as method errors. Read-only methods use the caller's `ctx` for their queries; if it ends, they return the context's error.

### Storage contention versus storage failure

Two situations look alike but are handled differently:

- **Before a command is accepted.** Another process (for example an `sqlite3` shell) holds the database's write lock. The command's transaction takes the write lock as it begins (`BEGIN IMMEDIATE`), so it waits up to 5 s for the other process; if the lock is released in time the command proceeds, otherwise it returns `storage_busy`. Nothing was saved, so retrying the same request ID is safe. The runtime stays ready.
- **After work is accepted.** A write that records running work (tool start/result, output, settlement, snapshots) fails for any reason, including a lock. The runtime stops admission (`Ready` returns `storage_error`) and cancels active work, because continuing would create effects without durable records. Reopen the runtime to recover.

## Chat lists and activity

`ListSessions` remains the full-snapshot export in creation order. Interactive
lists use `RecentSessions` and `Workspaces`, which read bounded pages from
indexes instead of building a full `Session` for every stored session.

**Activity** is conversation activity only: creation, an accepted turn (prompt,
continue or steering start), accepted steering (including a child completion
notice) and every saved history entry (including a model-handoff note). Renames, settings
changes, pauses and reads never change it. A parent's
tree activity also includes its direct children's activity, wherever they run,
so a parent rises with delegated work. Databases from before this field are
backfilled from their saved history, turn and steering times.

`RecentQuery` modes, by priority: `Search` (case-insensitive ASCII substring of
session names or workspace paths, across all workspaces; message contents are
not searched), `Active` (sessions with unfinished work), `Parent` (direct
children, optionally only in `Workspace`), `Workspace` (top-level rows: sessions
whose parent is absent or in another workspace, ordered by tree activity, each
with up to `RecentChildren` most recent nested children and a `ChildCount`), or
none (all sessions). A child in another workspace than its parent is a
top-level row in its own workspace, with `ParentName`/`ParentWorkspace`.

Cursors are keysets of (activity time, ID), so equal times page by ID without
loss or repetition. Activity only moves forward: a session that becomes active
after you read a page moves above your cursor. Later pages never repeat it;
reread the first page to see it. There is no maximum number of pages. These are
live lists, not snapshot exports. Workspace cursors use the workspace path as
the tie-breaker; deleting its newest chat can lower a workspace's aggregate
activity, so clients deduplicate workspace paths across pages and refresh the head.

## Mutations and request IDs

Every session/auth mutating request has a `RequestID` that the caller chooses. Host-local `Reload` and recovery admission `Resume` are not durable JSON commands and have no RequestID. It is required even for direct Go calls. Retrying with the same ID and the same arguments returns the original acknowledgement and does no new work, even after the session finished or was deleted, or its workspace or configured files changed. The request ID is checked before session/file validation for new requests; configuration preparation happens outside runtime/SQL locks. Concurrent configuration preparation may reject with `conflict` before deduplication; retry after it finishes. Reusing an ID with different arguments returns `conflict`. The request's `ctx` never cancels accepted work. If a call's outcome is unknown, retry it with the same ID.

## Operations

| Method | Behavior |
| --- | --- |
| `CreateSession(ctx, CreateSessionRequest{RequestID, Name, Workspace, Config})` → `Session` | Workspace must be an existing absolute directory. |
| `ListSessions(ctx, ListQuery{After, Limit})` → `SessionPage` | Creation order. `Next` is the opaque cursor for the next page. Limit 0 or over 200 means 200. |
| `RecentSessions(ctx, RecentQuery{Workspace, Parent, Search, Active, After, Limit})` → `RecentPage` | Bounded chat-list pages of compact `SessionSummary` rows (no usage or tool arguments), most recent conversation activity first. See [chat lists](#chat-lists-and-activity). |
| `Workspaces(ctx, ListQuery{After, Limit})` → `WorkspacePage` | Workspace paths with session/active counts and newest activity/metadata change, most recent activity first; returns no session rows. |
| `ReadFile(ctx, ReadFileRequest{SessionID, Path, LimitBytes})` → `FileView` | Trusted-owner, bounded text read on this runtime machine; [details below](#human-owned-file-viewing). No idle requirement or model activity. |
| `GetSession(ctx, id)` → `Session` | Phase, pause flag, binding, active turn/compaction, running tools, queued tools, pending guidance count, last event sequence, active-operation replay cursor, main and compaction usage. |
| `RenameSession(ctx, RenameSessionRequest)` → `Session` | Allowed any time. |
| `DeleteSession(ctx, DeleteSessionRequest)` | Idle only, otherwise `conflict`; unfinished owned children return `children_busy`. Allowed parent deletion preserves child history and their non-delegating role. Removes all session records (request IDs are kept), then stops background processes started for the session by this runtime and waits until they are reaped (up to the 2 s kill grace) or `ctx` ends; cleanup continues either way and `Close` waits for it. If observation fails, deletion has committed but the method returns `process_observation_failed` and does not claim the command stopped. Workspace and log files are left alone. |
| `StartTurn(ctx, StartTurnRequest{RequestID, SessionID, Prompt, Images})` → `TurnAck` | Idle only (a busy session returns `conflict`; steer instead). Applies pending and paused guidance first, then the prompt, and clears the pause. Returns immediately. See [Images](#images). |
| `StartSubagent(ctx, StartSubagentRequest)` → `TurnAck` | Atomically create a one-level child and accept its first turn. Optional `Config *SessionOverrides` selects model/provider alias, account, thinking and other settings without expanding inherited tool authority. [Selections, ownership and recovery](SUBAGENTS.md). |
| `Subagents(ctx, parentID)` → `[]Subagent` | Direct children with ordinary session snapshots, latest saved turn outcome, account and history/event references. |
| `Steer(ctx, SteerRequest{RequestID, SessionID, Text})` → `SteerAck` | Always saved first. Running: applied before the next model request. Idle and unpaused: starts a turn (`TurnID` set). Paused: waits, with `Paused` set in the ack. During manual compaction: waits for the compaction result. |
| `Interrupt(ctx, SessionRequest)` | Stops whatever the session is doing (turn, follow-up, compaction) and pauses. On an idle session it only pauses. Owned descendants are stopped/paused too. It does not target a specific turn or affect unrelated sessions or the process. Settlement arrives as events. |
| `Continue(ctx, ContinueRequest{RequestID, SessionID, Prompt})` → `TurnAck` | Starts a **new turn** while idle, applying paused guidance and optional prompt. Cannot bypass a suspended original operation. |
| `Resume(ctx)` → `error` | Explicit duplicate-effect opt-in: admit all suspended executions, preserving their original IDs, without waiting. No RequestID; repeated admission never duplicates a worker. |
| `ListSteering(ctx, id)` → `[]Steering` | Pending guidance in arrival order, with the session's pause flag. |
| `ClearSteering(ctx, SessionRequest)` → `ClearAck` | Clears all guidance pending at acceptance. It does not change applied history, the pause flag, or running work. |
| `Compact(ctx, SessionRequest)` → `CompactAck` | Idle only. A busy session returns `conflict` "Session is running. Interrupt it or wait before compacting." Nothing is queued. |
| `GetConfig`, `UpdateConfig` | See configuration above. |
| `InspectContext(ctx, id)` → `ContextInfo` | Saved active or installed base instructions with `instructions_state`, resolved model, budgets, retained-context estimate and installed summary. No file reads. |
| `ListTurns(ctx, ListQuery{SessionID, After, Before, Summary, Limit})` → `[]Turn, cursor` | Status, source, reason/error, frozen instructions unless `Summary`, and per-turn usage. `After` pages forward; `Before` pages backward, with `"0"` selecting the newest page. The returned cursor continues in the same direction. Do not combine them. Limit 0 or over 200 means 200. |
| `History(ctx, HistoryQuery{SessionID, After, Before, Limit})` → `HistoryPage` | Canonical history for inspection or export. `After` continues the existing forward API; a non-nil `Before` selects backward pages (`0` means newest) and returns `Older`. Encrypted reasoning and native private replay blocks are never included. A `reasoning` item's `Text` is its provider summary parts joined by a blank line, or empty when the provider sent no summary (common: summaries are optional per step); UIs should omit empty ones rather than invent content. Items carry `Category`; refused answers have `Code: "model_refusal"`; tool results have `Status` and, when their saved call record exists, its original `Name`, `Namespace`, and `Arguments`, even if the call item is on another page. Limit 0 or over 500 means 500. |
| `Events(ctx, EventQuery{SessionID, After, Limit})` → `[]Event` | One page. Limit 0 or over 500 means 500. |
| `Subscribe(ctx, EventQuery)` → `*Subscription` | Replay after `After`, then follow new events. |
| `Providers()`, `Models()`, `Tools()` | Configured catalog and tool schemas only. No secrets, no discovery. For a login provider, `ProviderInfo.Auth` names it and `HasCredential` reports whether any account of it is logged in. |
| `UpdateModels(ctx, models)`, `UpdateModelsAndSave(ctx, models, save)` | Publish only definitions for new work, with optional host save before publication. No idle-chat requirement or prompt/settings reload; active work retains captured definitions. [Persistence and failure rules](MODEL-INSTALLATION.md#safe-publication-and-operator-edits). |
| `Auth()` → `*AuthManager` | The manager passed as `Config.Auth` (or nil), for hosts and the HTTP server. The runtime does not own or close it. |
| `Diagnostics()` → `[]Diagnostic` | Startup diagnostics (skipped tool registrations) and asynchronous lifecycle failures such as `process_observation_failed`. |

**History and turn cursors.** A non-nil `HistoryQuery.Before` is an exclusive sequence boundary: `Before: ptr(int64(0))` selects the newest page. Returned items remain in canonical ascending sequence order, so render them normally; pass `HistoryPage.Older` to the next backward query. `Older` is the first sequence in the current page and is omitted when no still-older entries exist. The query reads only the nearest page plus one sentinel row; it does not scan or rewrite older history. `After` remains the existing forward/export API and continues to return `Next`. Each page is a transport/render batch, never a history-retention limit. Tool call/result pairs and turn boundaries are not artificially kept together, so a UI should reconcile entries by sequence and tool-call identity across adjacent pages.

`ListQuery.Before` works the same way for turns but uses an opaque decimal `rowid` cursor, not a history sequence. `Summary: true` omits frozen `Turn.Instructions` while retaining turn status, source, errors/reasons, times and usage; callers that inspect/export full turn records can leave it false. If a saved turn still says `running` but this runtime has no worker for its session, `ListTurns` reports `suspended` rather than suggesting that work is live.

`Session.ReplayAfterSeq` is a cursor for reconnecting to current activity, not a replacement for `LastEventSeq`. For a model request it starts at that attempt; for a tool batch it starts at the first tool call in that batch; for compaction it starts at that compaction. During preparation, or before any tool in a batch has started, it equals the snapshot's latest event sequence so the subscriber receives subsequent events without replaying completed work from the same turn. Pass it as `EventQuery.After` when reconnecting to active work; for an idle snapshot, use `LastEventSeq`. `GetSession` reads these values and the active execution from one transaction. Event delivery remains at-least-once; clients deduplicate by `Seq`.

Session phases are `idle`, `running`, `compacting` (manual or automatic), `suspended`, and `stopping`. They are derived from the saved execution plus live worker ownership. A worker still joining is not idle. Turn statuses are `running`, `suspended` (no end time), `completed`, `cancelled` (Interrupt), `failed`, and legacy `interrupted`. Active IDs stay visible while suspended. Turn sources are `prompt`, `continue`, `steering`, and `follow_up`.

### Human-owned file viewing

Scenario: your browser opens `/tmp/draft/SPEC.md` from a chat on a remote server.
`Runtime.ReadFile(ctx, ReadFileRequest{SessionID, Path, LimitBytes})` reads
**the runtime server's** file, not a browser-local or frontend-host file. Relative names use the owning session's workspace. Absolute
paths outside it, `..` and legitimate file/directory symlinks are allowed. There
is no home expansion: supply an absolute home path, not `~/...`.

This is the trusted owner's OS-level access, **not a sandbox** or a redacted
metadata API. Any readable regular text file may be deliberately opened,
including credentials, logs and other sensitive files. Tool disabling applies to
models, not this human operation. Hosts should expose it only within their
existing trusted-owner boundary; browser clients should read only after an
explicit action and must not execute the contents or fetch embedded resources.

`FileView` contains `SessionID`, `Path` (cleaned absolute opened name, retaining
symlinks), `Text`, `Bytes` and the effective `LimitBytes`. `ReadFileRequest` carries
`SessionID`, `Path` and an optional `LimitBytes` integer. Zero selects
`DefaultFileViewLimit`: **8 MiB (8,388,608 bytes)** per open. Positive values are
explicit file-byte budgets; negative values return `invalid_argument` before
session/filesystem I/O. Values are bounded only by the Go `int` representation
and practical memory/OS limits, not a second hidden product cap. JSON overhead
is outside the file-byte budget. Model `ToolOutputLimit`, tool disabling and
session configuration do not select or change this budget.

The operation always returns the **whole file**, never a page or prefix. Capture
stops after one overflow probe byte; `file_too_large` returns no content, with no
truncation, archive or automatic retry. Increase `ReadFileRequest.LimitBytes` (or
the viewer's per-open size control) and explicitly reopen, or open a smaller
file. No idle boundary, config mutation/reload or restart is needed. A running
read keeps the selected limit it captured; each subsequent explicit open can
choose another. Very large requested files still require real backend and
browser memory; selecting a large limit is not a promise that resources exist.

Only regular valid UTF-8 files without binary control bytes are supported (tab,
CR and LF are allowed; other ASCII controls and DEL are rejected). Missing files
return `not_found`; nonregular/invalid text returns `unsupported`; OS errors such
as permissions return `file_read_failed`. Directories, FIFOs and devices are not
browsed or downloaded; FIFO opening cannot block waiting for a writer.

Reads neither mutate session/activity/history/events nor call providers/hooks or
model tools. They work during active or suspended chats without holding runtime,
SQL or built-in file-tool locks across disk I/O, so Stop is independent. They
are **not transactional snapshots**: concurrent external/Bash/tool edits can
change what is observed. Cancellation is checked between chunks, but cannot
reliably interrupt an OS read blocked on a stalled filesystem. No file data is
persisted by this operation.

### Pause rules

A session has one pause flag. Interrupt, terminal turn/compaction failure, and legacy recovery set it. Clean Close/new-format crash recovery preserve it; Resume does not clear a pause. Only `StartTurn` and `Continue` clear it. While paused, steering is saved but does not start work. After normal completion, pending guidance on an unpaused session starts a `follow_up` turn in the same transaction that completes the old turn, so a racing steer is either applied or scheduled, never lost or applied twice. If the latest catalog makes that follow-up inadmissible, preserve the completed result and pending guidance, pause the session, and emit the repair reason in `session.paused`; repair settings/model selection before Continue. After successful manual compaction, pending guidance starts a turn only if the session is unpaused. Note that after an interrupt with nothing pending, the next bare `Steer` is also held until `Continue`.

## Events

```go
type Event struct {
    Version     int             // EventSchemaVersion (1)
    SessionID   string
    Seq         int64           // increasing per session, starting at 1
    Time        time.Time
    Kind        string
    Category    string // "output", "diagnostic", or "error" (from Kind)
    TurnID, OperationID, CallID string // optional
    Step, Attempt int                 // optional; distinguish replacement streams
    Payload     json.RawMessage
}
```

| Kind | Payload type |
| --- | --- |
| `turn.started` | `TurnStartedPayload{Source, Prompt, Images, Model, Account, Reasoning}` (`Reasoning` is `"summaries"` or `"unavailable"`; `Account` only for login providers) |
| `turn.completed` / `turn.cancelled` / `turn.failed` / `turn.interrupted` | `TurnEndedPayload{Reason, Code, Error, ProviderFailure}` (`Code` is the error code of a failed turn) |
| `text.delta`, `reasoning.delta`, `refusal.delta` | `DeltaPayload{Text}`, batched to about 16 KiB or 50 ms. Reasoning deltas insert the same blank line between summary parts as saved history text |
| `response.refusal` | `RefusalPayload{Code: "model_refusal", Text}`: the complete refusal of a completed response |
| `tool.started` | `ToolStartedPayload{Name, Arguments}` with `CallID` |
| `tool.output` | `ToolOutputPayload{Stream, Text}` (`stdout`, `stderr`, or `output`) |
| `tool.completed` / `tool.failed` / `tool.cancelled` | `ToolEndedPayload{Name, Result}`. `Result` is exactly what the model sees. |
| `steering.accepted` / `steering.applied` | `SteeringPayload{SteeringID, Text, Paused}` |
| `steering.cleared` | `SteeringClearedPayload{Cleared}` |
| `session.paused` | `{"reason": ...}` |
| `session.renamed` | `{"name": ...}` |
| `subagent.started` | Parent event containing ordinary `TurnAck` identifying the accepted child's session and first turn. Terminal notices use `steering.accepted/applied`, not a separate event queue. |
| `compaction.started` / `completed` / `failed` / `cancelled` | `CompactionPayload{Model, Account, Automatic, CoveredSeq, Code, Error, ProviderFailure}` with `OperationID` = compaction ID, including events written by crash recovery |
| `hook.result` | `HookResultPayload`: decisions, direct failures, accepted history references or combined-budget contributor IDs; no duplicated text |
| `config.changed` | installed `SessionSettings` (`effective` plus `overrides`) |
| `model.attempt.started` | `ModelAttemptStartedPayload{Model, Account, Compaction, Reason}` (`initial`, `retry`, or `recovery`) |
| `model.attempt.ended` | `ModelAttemptEndedPayload{Outcome, Code, Message, Usage, ProviderFailure}`; usage null when unknown |
| `operation.suspended` / `operation.resumed` | reason or duplicate-effect warning, original operation IDs |
| `tool.attempt.interrupted` | warning that the identified attempt may have produced effects |
| `model.retry` | `RetryPayload{Attempt, Reason, DelaySecs, Compaction}` |
| `usage` | `UsagePayload{Usage, Compaction}`. Counters are nullable; `null` means unknown. Every completed main-model step records one. |

### Token usage and cache measurement

`Usage` has nullable `*int64` counters: `InputTokens`, `OutputTokens`,
`ReasoningTokens`, `CacheReadTokens`, and `CacheWriteTokens` (JSON:
`input_tokens`, `output_tokens`, `reasoning_tokens`, `cache_read_tokens`,
`cache_write_tokens`). Nil/null or an absent legacy field means **unknown**;
reported zero means known zero. These are measurements, not a billing model or
cache-hit guarantee. Counts must be nonnegative and representable as int64;
reported cached input cannot exceed a reported total input. Invalid counts or
accumulation overflow fail rather than clamp or fabricate data.

`InputTokens` is **total input including cached input**. Responses maps
`input_tokens_details.cached_tokens` to `CacheReadTokens`; no reported detail
(including Copilot omissions) leaves it unknown. Responses has no mapped cache
write counter. Messages preserves `cache_read_input_tokens` and
`cache_creation_input_tokens`; total input remains ordinary `input_tokens` plus
those two components, only when all three are reported. Do not add the cache
components to sam's total again.

`Turn.Usage`, `Session.Usage` (main requests), and `Session.CompactionUsage`
(summary requests) sum only reported counters, including known terminal failure
usage. An aggregate can therefore be a partial known sum, not proof that every
response reported that counter. NULL + NULL stays NULL. Legacy stored turns and
compactions acquire NULL cache columns; old events remain unchanged.

`Session.LatestUsage` and `Session.LatestCompactionUsage` project the newest
`model.attempt.ended` event in the respective main/summary scope. No extra
history or saved cache state exists. They are nullable, and an attempt with no
reported usage clears the projection rather than selecting an older known
response. A running attempt does not replace the previous ended attempt yet.
Failures, interruptions and shutdown use the same durable settlement owner;
reopen projects the same events. To display a latest-response cache hit rate,
use **that single attempt's** `CacheReadTokens / InputTokens`, only when both
are reported and total input is positive. Zero total or missing data means no
known rate. Never divide a latest cache count by conversation or turn totals.

### Presentation categories

Every event, history item, and diagnostic has a `Category` that tells a UI how to show it. It is a presentation hint, not a turn outcome: streamed text does not mean the turn succeeded, and a failed tool call is shown as an error even though the model usually recovers from it within the same turn. The specific `Kind`, codes, and IDs stay authoritative.

| Category | Events |
| --- | --- |
| `output` | answer and reasoning deltas, tool start/output/completion, turn started/completed, steering, config, rename, usage |
| `diagnostic` | `hook.result`, `model.retry`, compaction started/completed/cancelled, turn cancelled/interrupted, tool cancelled, session paused (including recovery notices) |
| `error` | `turn.failed`, `tool.failed`, `compaction.failed`, `refusal.delta`, `response.refusal` |

History items: assistant text and successful tool results are `output`; runtime notes are `diagnostic`; refused answers and failed, uncertain, or not-executed tool results are `error`; cancelled tool results are `diagnostic`.

**Model refusals.** When the model refuses, its refusal text streams as `refusal.delta` events (payload `{text}` only, category `error`) and the completed refusal is saved in history and announced with `response.refusal` (`code: model_refusal`). It is never shown as a blank successful answer and never retried as a transport problem. The turn itself completes normally (the provider did answer); the user can rephrase and continue.

Events are committed before anyone can read them. `Subscription.Next(ctx)` returns the next event. Before yielding a buffered event or loading a page it checks the subscription, the runtime, and `ctx`, so a closed subscription stops immediately even mid-replay. It reads pages of 200 from SQLite and, when caught up, polls about every 100 ms while `Next` is waiting. There is no broadcaster or unbounded queue: a slow reader holds its cursor and at most one page of events. `Close` (or the runtime closing) also cancels a page query in progress, including one waiting for the shared database connection. `Next` returns `not_found` if the session is deleted and `closed` when the subscription or runtime closes. `Subscribe` rejects a cursor beyond the last event with `invalid_argument`. Delivery is at least once across reconnects, so deduplicate by `Seq`.

## Tools

```go
type Tool struct {
    Name, Description string
    Parameters json.RawMessage // JSON schema
    Run func(ctx context.Context, call ToolCall) (string, error)
}
type ToolCall struct {
    SessionID, TurnID, CallID, Workspace string
    Attempt int
    Arguments json.RawMessage
    Output *Output
}
```

`Run` must honor `ctx`. When it stops because the context ended, return or wrap `ctx.Err()` or `context.Cause(ctx)` so `errors.Is` identifies cancellation. Nil means known success even after cancellation; other errors remain known failures. Explicit overflow/process-observation errors stay known. Only identifiable shutdown cancellation of invoked code becomes pending/uncertain for recovery. `(SessionID, TurnID, CallID)` is the stable logical identity; Attempt counts saved execution intents, not another generated call ID. It should write output through `call.Output` (an `io.Writer`; `Output.Stream(name)` labels a stream), which streams events and enforces the output limit during capture. The returned string is short result metadata appended to the captured output (after a newline when needed). The whole result, captured output plus metadata, must fit the call's output limit. A returned error becomes a `tool_error` result the model can act on: its message (bounded to 2 KiB) plus any captured output, outside the output limit so the failure can always be explained. Host tools with a built-in's name replace it. Disabled tools are not advertised and cannot run: a call to one returns `tool_unavailable`.

**Registration checks.** `Open` and unified reload check each host tool: a non-empty `Name`, a non-nil `Run`, and `Parameters` that is a JSON schema object with `"type": "object"`. An invalid registration is **skipped**, not fatal (the existing tool policy also applies on reload): valid tools still load, and `Diagnostics()` (HTTP `GET /v1/diagnostics`) lists `{category: "error", code: "invalid_tool", tool, message}`. An invalid registration that uses a built-in's name disables that name entirely; it does not silently fall back to the built-in. A skipped name may still appear in `DisabledTools` without making the configuration invalid. Panics inside a host tool's `Run` are not recovered.

Scenario: you register a replacement `bash` but forget `Run`. The runtime starts, reports the diagnostic, and offers no `bash` tool at all, so the model never runs a command through a tool you meant to replace.

Built-ins:

- `read {path, offset?, limit?}` reads text lines (offset is 1-based) and rejects binary files.
- `read` also rejects selected lines that are not valid UTF-8 (a NUL byte in the first 8 KiB marks a binary file). Only regular files are supported.
- `write {path, content}` creates parent directories and replaces the file atomically (temp file plus rename). `content` is required; a missing or `null` value fails before anything changes, while `""` deliberately writes an empty file.
- `edit {path, edits: [{old_text, new_text}]}` requires each `old_text` to match exactly once, with no overlaps; overlapping occurrences count too (`aa` in `aaa` is ambiguous). `new_text` is required (`""` deletes the match; missing or `null` fails). Every edit is checked before the file changes, and existing permissions are preserved.
- `bash {command, timeout_seconds?}` runs `bash -c` in the workspace in its own process group. stdout and stderr share the limit. Cancellation, timeout, or overflow sends SIGTERM to the whole group and gives every member up to 2 s, even if the shell exits first, then SIGKILL. When the shell exits on its own, leftover group members (for example `cmd &` jobs) are killed once the output pipes close, or after 2 s if they keep them open; in that case the call fails with a descriptive error (captured output is kept) instead of claiming success. The result ends with `[exit status N]`. If the OS exit observer fails, bash returns a failed tool result with code `process_observation_failed`; it sends no further signal because process-group ownership is uncertain, and warns that the command may still run. A call already cancelled before the command starts runs nothing. `nohup` alone does not leave the process group, so it does not protect a process; use background mode.
- `bash {command, background: true, log_file}` launches a long-lived process, for example a dev server. See below.
- `history {action, subagent_id?, view?, ...}` searches/reads the caller's or an owned child's visible history/events. See below.
- `subagents {action, ...}` starts, inspects, steers or cancels ordinary durable child sessions. See [native subagents](SUBAGENTS.md).

### Background processes

Scenario: the agent runs `npm run dev`, keeps working, and tells you where to preview the site.

- The model calls `bash` with `background: true`, the long-running command itself (`npm run dev`, not `npm run dev &`), and an explicit `log_file` (relative paths resolve against the workspace; parent directories are created; existing symlinks are followed and outside paths are allowed). `timeout_seconds` is not allowed with it, and `log_file` is not allowed without it. There is no temporary-log fallback.
- The command starts in its own process group with stdin from `/dev/null` and stdout/stderr **appended** to the log file, so earlier log contents and the file's mode are kept. The log must be an ordinary file: a directory, socket, or FIFO is refused, and opening never blocks waiting for a FIFO reader. The call returns immediately with a short launch result:

  ```text
  Launched in background (this does not mean it is ready): {"pid":4242,"log_file":"/home/me/project/logs/dev log.txt"}
  Its stdout and stderr are appended to log_file. It stops when its command exits, the session is deleted, or the runtime shuts down; to stop it sooner while it runs: kill -TERM -- -4242
  ```

  `log_file` is the real path, with existing parent symlinks resolved, written as a JSON string (no shell command is built from it). The agent checks readiness with ordinary commands (`curl`) and reads the log with `read`.
- The launch result is the saved tool output, so it appears in `tool.completed` events and history after reconnects and restarts. It counts against the ordinary output limit. **Before starting**, the runtime checks that the whole result fits, allowing for a 10-digit PID; if it does not (a tiny `ToolOutputLimit` or a very long path), nothing is created or started and the call fails: use a shorter `log_file`, or have the user raise `tool_output_limit` with `UpdateConfig` while the session is idle. Log contents stay in the file only: they are not copied into SQLite or streamed, and the log is never capped, rotated, or archived by sam.
- **Lifetime.** The launched command is what sam manages. The process survives the launching call, the end of the turn, and interruption of later or current session work. When the command exits on its own (it crashed, finished, or was a launcher that backgrounded something and exited), whatever it left in its group is killed at once and the leader is reaped promptly; nothing lingers as a zombie. When its session is deleted or the runtime closes, its whole group gets SIGTERM and up to 2 s to exit, even if the leader exits first, then SIGKILL; then the leader is reaped. Deleting a session does not delete log or workspace files.
- **Ownership.** Each launch has one owner inside the runtime that observes the leader's exit without reaping it, sends every signal, and reaps it last, so the group ID cannot be reused by another process while sam may still signal it, and is never signalled afterwards. Darwin confirms a kqueue exit event still has a waitable `WNOWAIT` status; Linux uses `waitid(WNOWAIT)`. If observation fails (`ECHILD`/`ESRCH` included), the owner sends no further signals and reports `process_observation_failed`; the command may still run and cleanup is unconfirmed. The runtime exposes the failure through `Diagnostics()`; foreground bash also returns a failed tool result. Deletion/`Close` return the cleanup error. The host must not ignore `SIGCHLD` or enable automatic child reaping; sam does not install or change host signal handlers. Deletion and `Close` join a stop already in progress. If the launch result cannot be saved (a storage failure after the process started), that new process is stopped; earlier acknowledged processes keep running, and nothing it already did is undone.
- Ownership is in memory only. If the runtime crashes between starting a process and saving its result, that process keeps running untracked and the tool call is reported as uncertain. After a crash, a restarted runtime does not adopt, restart, or signal those processes; it never signals a PID read from SQLite, since the PID may belong to an unrelated process by then. Stop leftovers yourself (`kill -TERM -- -PID` if you know it is still that process group). Processes that deliberately escape the group (`setsid`, double-forking daemons) are not tracked.
- `localhost` in a dev server's URL is the **remote machine's** localhost. To preview from your laptop, forward the port yourself, for example `ssh -N -L 5173:127.0.0.1:5173 me@remote-host`, or open the machine's Tailscale address if the dev server listens there. sam does not configure port forwarding, firewalls, or network exposure.

### History tool

Scenario: after compaction, the summary says "the user gave a callback URL" but not the URL. The agent calls `history` with `{"action":"search","query":"(?i)callback.*url"}`, sees entry `seq` 124 with an excerpt, then calls `{"action":"read","around_seq":124,"before":3,"after":3}` and reads the original message. The default system prompt tells the agent to do this instead of guessing.

- **Scope.** The calling session from runtime-supplied `ToolCall.SessionID`, or a directly owned child selected by optional `subagent_id`. Arbitrary session/parent identities and SQL are rejected. Read-only apart from the ordinary tool-call record.
- **View.** Optional `view` is `history` (default) or `events`. Events use ordinary visible Event envelopes for progress/retries/failures and a distinct seq/cursor scope. Search matches event JSON; read returns `{"entries":[Event...]}`. The same bounded search/read rules apply; event range reads snapshot their upper bound. See [examples and recovery rules](SUBAGENTS.md).
- **What is searched.** The same visible entries `Runtime.History` returns, including entries already compacted: user, steering, and assistant messages (refusals too), reasoning summaries, tool calls (`name` plus `arguments`), tool results (including background launch results), and runtime notes. Encrypted reasoning and native private replay payloads are dropped before visible reads reach Go; credentials are never in history. Search skips the history tool's own calls and results, so it does not rediscover earlier search output.
- **`search {query, cursor?}`.** `query` is a Go regular expression (RE2: `(?i)` works; lookaround and backreferences do not). An invalid pattern fails with Go's error message. `limit` is rejected: a page holds at most 10 matching entries, newest first. Each entry appears once with `seq`, `turn_id`, `kind`, `type`, `role`, `name`, `call_id`, `category`, `status`, `code`, `time`, and an `excerpt` of up to about 320 bytes around the first match. The result is `{"matches":[...],"next_cursor":"..."}`; `next_cursor` appears only if another match exists. Pass it as `cursor` with the **same** query. A cursor from another session or query, or a malformed one, fails clearly. Pages continue below the previous page, so new messages never reshuffle them; a new search without a cursor includes them.
- **`read`.** Either `{from_seq, to_seq}` (inclusive) or `{around_seq, before?, after?}` (entry counts, default 0). Mixing them, a reversed range, negative counts, or an `around_seq` that does not exist fails. Reads return every entry `Runtime.History` shows, including earlier history lookups, and `before`/`after` count those same entries. The result is `{"entries":[HistoryItem...]}`. If it exceeds the output limit, the call fails with `tool_output_limit` and asks for a narrower range. Excerpts are previews; reads return originals.
- **Cost.** SQLite only retrieves rows, 64 at a time newest first; rows are released between batches, cancellation is checked between them, and only the page's results are kept. There is no index or cache: a search reads rows until it finds an **eleventh** match (to know whether `next_cursor` is needed) or reaches the oldest entry. So a query with exactly ten recent matches still scans all older history. Memory is bounded by one batch of rows, not by bytes: a single very large entry is still loaded whole. Disable it like any tool with `DisabledTools: ["history"]`.

All built-in file tools share one runtime-wide `sync.RWMutex`: reads overlap, while writes and edits are exclusive, including their validation. `bash` and host tools do not take this lock. Paths are a working-directory convention, **not a sandbox**.

**Output limit.** Each call's result may be up to `ToolOutputLimit` bytes (default 262,144), counting captured output, the separator, and returned metadata such as bash's `[exit status N]` line. Capture stops at the first byte beyond the limit; a result that exceeds it only after the metadata is appended fails too. Either way the call fails with:

```json
{"error":{"code":"tool_output_limit","limit_bytes":262144,"message":"bash output exceeded 256 KiB across stdout and stderr. Cancellation was requested; earlier side effects may have occurred. ..."}}
```

Output already streamed stays in the event log. Nothing more is captured, no truncated success is returned, and nothing is retried. Other calls in the batch continue.

Calls from one model response run in parallel, up to `ToolConcurrency` across all tools. Results go back to the model in call order. Interrupting cancels running calls and prevents queued ones from starting. A real BeforeTool hook failure also cancels/join siblings through a batch child context and preserves its originating error; ordinary tool errors/denials do not. A call whose `Run` already returned successfully keeps its real result even if the interrupt arrives while that result is being saved; only unfinished calls are reported as `cancelled`.

## Model requests

Model-provider diagnostics (distinct from fixed OAuth diagnostics above) are bounded: the credential is redacted first, including a partial key cut off at a read limit, and then the message is shortened to about 1 KiB with an explicit `… [truncated N bytes]` marker. Error bodies use the existing 4 KiB diagnostic bound with cap-plus-one detection. Oversized/unreadable errors preserve known status/request ID but have unknown code/shape and are not retried; truncation must not turn an unclassified plan failure into a generic 429 retry.

Subscription providers resolve (and, when needed, refresh) the operation's account credential before every attempt. Only Copilot uses the one forced 401 refresh cycle; SIWC surfaces direct-admission failures and checks the shared account plan gate before each inference attempt. See [Subscription login and accounts](#subscription-login-and-accounts). Main-agent and compaction requests share the same retry rule: at most 3 retries after the first durably reserved attempt, including all restarts, for connection errors, HTTP 408/429/500/502/503/504, and `server_error`/`rate_limit_exceeded` stream failures. A valid `Retry-After` header (seconds or HTTP date) is honored in full. Without one, the waits are 1, 2, and 4 s. Waits are interruptible. Known permanent errors (such as `insufficient_quota` or `invalid_api_key`), other 4xx responses, cancellation, and malformed completed responses are not retried. Once visible text or reasoning summary from a main-agent request has been published, a stream failure ends the turn instead of retrying. Compaction output is private until the summary is installed, so a compaction stream that fails partway is retried within the same budget; a completed but inadequate summary is not. The partial text, or a reasoning summary shown without any answer text, is kept and quoted in a labelled runtime note in history, and `Continue` resumes. Incomplete provider items and their private reasoning are never replayed. Completed tools are never rerun by retries.

## Compaction

- The context budget is `context_tokens − output_reserve`. Automatic compaction runs before a model request when the estimate (UTF-8 bytes ÷ 3 + 20%) exceeds `trigger_percent` of that budget and there is history outside the recent window.
- It summarizes older history, including completed steps of the current turn. `recent_percent` is a **minimum**: whole recent items (including complete native Messages response groups) cover at least that conservative token estimate where available, expanding backwards for complete tool/reasoning groups. An assistant message larger than that target stays whole. The genuine user preceding the earliest retained exchange is kept word for word too, separately if outside the recent tail. A long task keeps its user plus recent steps, while older intervening steps can be summarized.
- The cut and exact protected-user reference stay fixed until the next successful compaction. New prompts, steering and restart append after that context rather than removing the old user. Fit checks include the expanded tail and protected user; an impossible fit fails clearly. Legacy summaries freeze whichever separate user was visible at migration time, without guessing their original installation-time selection. Original history remains available.
- It makes one summarization request (with transport retries), using the compaction model, the installed current prompt (its initial preparation is also saved on the compaction record as history, not restored for recovery or reread by workers), and plain-text history. Tools and opaque reasoning are never sent. If the input does not fit the compaction model, it fails with `context_limit` before any request is made. The summary is installed only if it reduces the estimate and fits the main model. Original history and events are kept.
- Installing the summary and interrupting are serialized: if an interrupt is accepted first, the summary is not installed and the previous context stays; if the summary was committed first, the compaction stays completed and the interrupt stops the work that follows.
- Guidance accepted while an automatic compaction runs is applied before the very next main-model request.
- A terminal automatic failure ends the turn as `failed` and pauses the session. The fix is: `GetConfig` → `UpdateConfig` (for example a larger-context compaction model) → `Compact` → `Continue`.

## Recovery

`Open` loads latest configuration/prompts/tools/hooks and reconciles saved progress without running effects. Explicit session overrides and the bound main model remain authoritative. Call `Resume(ctx)` to admit **all suspended work**. It returns after admission; its caller context does not own admitted work. Independent idle sessions can accept fresh commands before Resume. Suspended sessions allow reads, queued guidance and Interrupt, but reject StartTurn/Continue/manual Compact/Delete and prevent all-idle reload.

Cancellation before Resume admission prevents saved resumption events and worker launch. After a successful admission commit, cancellation of the caller does not stop the admitted work. The context is **not a strict return-time guarantee**: if SQLite is already waiting for a writer lock, Resume may return cancellation only when that wait finishes or reaches the existing five-second busy timeout. This uses the normal database wait policy, not a new recovery timeout or polling loop.

**Resume uses the same executor and batch/result paths as a normal run.** Known saved outcomes never rerun, even before ordered history assembly. Startup reconciliation settles unfinished replay-unsafe calls; replay-allowed calls stay pending and run through current availability/BeforeTool with their original arguments. After the batch settles, the next ordinary model request includes all results and the agent can recover without a separate human approval or a new conversation.

`Tool.ReplayUnsafe` is host-owned policy, also exposed by `ToolInfo.ReplayUnsafe`; it is not a model argument. Built-in `bash`, `write`, and `edit` set it. Custom tools default to replay allowed; a host replacement supplies its own definition and policy. A call's restriction is saved when its batch is planned; recovery honors that bit **or** a stricter current tool definition. Only this restriction is saved, not old tools/configuration. Legacy calls without the bit use current policy. New model-issued calls can execute normally, including unsafe ones.

Every unfinished carried-over unsafe call is skipped, including ones that never started. Its normal paired tool result has code `recovery_skipped`, a turn/step/call history reference, and one of two truthful outcomes: `not_executed` if it never started, or `uncertain` if an attempt may have produced effects without a saved result. Previous partial tool output stays in events. Results tell the agent to inspect state before issuing replacements, not blindly repeat a server shutdown command. Skips are saved once; repeated Open does not duplicate them. No whole-batch cutoff discards other results or prevents replay-allowed tools from settling.

**Replay-allowed tools and hooks may still repeat external effects, twice or more.** There is no exactly-once guarantee, repeat cap, old-code restoration or account fallback. The agent can also issue a fresh unsafe call after reassessment; this is not a sandbox against new commands. An uncertain replay-allowed call's eventual result warns about earlier possible effects, including tiny output-limit errors. Hook recovery behavior is unchanged. Saved stop still wins over recovery and late results/retries/install/follow-up, retaining known outcomes.

Provider requests have four total reserved attempts (initial + three retries), including across process loss. Reservation precedes credential/provider I/O; saved Retry-After/backoff deadlines survive. Partial main output is quoted once from committed deltas when an unfinished attempt ends, both on crash and clean Close. Completed responses create no partial note; private summary text never leaks. Clients should group streamed deltas by operation/Step/Attempt rather than concatenate a replacement onto the old partial.

BeforeTurn acceptance is durable even when empty. Recovery rebuilds a lost request with current request/compaction hooks and newly queued guidance; previous accepted extension messages remain exact and may be followed by new identical contributions. Ordinary live retries remain frozen. Auto compaction gets one opportunity per logical step; installed summaries are not repeated if private turn candidates were lost. A pending compaction keeps its ID/budget but may reselect under latest settings. An inadequate changed preparation fails normally rather than looping summaries.

Close suspends eligible work and stops owned background processes; it does not record user cancellation. Interrupt saves terminal stop intent before signalling and works on suspended work without executing it. Original terminal operations never Resume. After Interrupt, Continue deliberately starts a new turn. After restart, stored PIDs are never adopted or signalled. An unacknowledged built-in Bash launch is skipped with uncertainty rather than relaunched; hosts still own cleanup of untracked leftovers. Planned self-redeployment should run outside the foreground tool call: stopping its own host can interrupt that call before it reaches the start/verification commands.

Unfinished records without recovery checkpoints are conservatively interrupted, preserving known results, history and receipts. They are never guessed into resumable work. Saved calls without a replay restriction use current tool policy.

## Embedding example

See the [embedding setup](README.md#embed-the-go-core) and the [browser example](../example/README.md) for a host using the library and HTTP API.
