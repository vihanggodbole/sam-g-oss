# sam-g

A small Go runtime for persistent agent work. Embed it in an application or
control it through the HTTP/SSE server. Sessions, model output, tool results and
events live in SQLite. Disconnecting a client stops observation, not accepted work.

[Go API](LIBRARY.md) · [HTTP API](SERVER.md) · [Browser example](../example/README.md)
· [Illustrated tour](explore.html)

The illustrated tour is an offline explanation, not a live console. Open
`explore.html` alongside its CSS/JavaScript files, or serve this docs directory
statically; GitHub displays HTML source rather than running it.

## Requirements

- **Go 1.27+**, on Linux or macOS. Bash is required for shell tools.
- An OpenAI-compatible **Responses** endpoint, or a supported ChatGPT/Copilot
  subscription account. Chat Completions is not supported. Copilot also supports
  the exact native Messages profile described in the [library guide](LIBRARY.md#native-messages-through-copilot).
- Writable **local-disk** SQLite storage, with one runtime process owning each
  conversation database. Do not share or sync a live database over a network filesystem.
- No Node.js or C toolchain is needed for the application. A workspace's tools
  may have their own dependencies.

The Go package is `sam`, with module path `github.com/vihanggodbole/sam-g-oss`.
Until a release is published, external applications use the local `replace`
setup below.

## Try the browser example first

From the repository root:

```sh
go run ./example
```

Open **http://127.0.0.1:8765**. Add a named ChatGPT or Copilot account, load its
models and create a session. The command serves both the page and its backend;
do not also start the stock API server for this example. Data stays under
`~/.sam-g/example/`. Tools are off by default. Sending messages uses the selected
account's allowance; login alone does not guarantee model access. Follow the
[example walkthrough](../example/README.md) before enabling file or shell tools.

## Quick start

For the standalone API server, run from this checkout:

```sh
./scripts/install.sh
```

The installer asks for confirmation, builds `~/.sam-g/bin/sam-g`, creates private
host directories, and copies [the example config](sam-g.example.json) to
`~/.sam-g/config.json` **only if absent**. It does not start the server or configure
network access. `--yes` explicitly permits noninteractive installation.

Edit the config to select provider/model IDs your account supports. The template
uses `OPENAI_API_KEY`; supply that variable through the server's environment,
never put the key in JSON. Its context budgets are local declarations, not verified
provider limits. Subscription setup uses the [authentication guide](AUTH-MODELS.md)
and [subscription template](sam-g.subscription.example.json).

```sh
# After setting the required credentials and reviewing configuration:
./scripts/run.sh
# In another terminal, using the configured listener:
curl -fsS http://127.0.0.1:8080/readyz
```

The server defaults to loopback port 8080 and serves an API, not a homepage.
The run helper uses the installed binary and existing listener configuration.
Foreground is default; `--bg` survives closing the terminal and appends
`~/.sam-g/logs/server.log`, but does not provide reboot supervision or log rotation.
Do not start a second owner of the same stores. See [stop/restart instructions](SERVER.md#check-stop-restart-and-change-ports).

The binary embeds its runtime documentation and publishes a versioned snapshot
under `~/.sam-g/docs/` at startup. It needs no source checkout to run. Publication
or verification failure stops startup; fix the reported directory problem rather
than bypassing it. Existing snapshots are retained for processes that use them.

### Connect a browser client

A separate frontend uses the standard HTTP/SSE API. Add its exact page origin to
`server.allowed_origins`, preserving other intended entries, and restart sam-g.
Trust only frontends you control. Host/Origin checks are not user authentication;
see [browser access](SERVER.md#browser-access-and-cors).

### Remote access

Use loopback with an SSH tunnel, or restricted Tailscale access. The server guide
owns [remote deployment](SERVER.md#deployment-on-a-remote-machine), including the
optional `--tailscale-service` helper and [named Services](SERVER.md#named-tailscale-services).
No helper installs systemd/launchd supervision, grants network access or makes
public unauthenticated exposure safe.

### Create, watch, and reconnect

Use an existing absolute workspace path **on the server**. New mutations need
new request IDs; retry an uncertain request with the same ID and unchanged body.

```sh
B=http://127.0.0.1:8080
curl -fsS -X POST "$B/v1/sessions" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-create-1' \
  -d '{"name":"test repair","workspace":"/home/me/project"}'
S=ses_REPLACE_WITH_RETURNED_ID
curl -fsS -X POST "$B/v1/sessions/$S/turns" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: demo-turn-1' \
  -d '{"prompt":"Run the tests and fix failures"}'
curl -fsSN "$B/v1/sessions/$S/events"
```

Ctrl+C stops watching, not the turn. Reconnect with `Last-Event-ID` set to the
last event your client handled. Delivery is at least once: deduplicate by session
and event sequence. A turn acknowledgement means work was saved, not that it
succeeded. Use the [HTTP controls](SERVER.md#api) to steer, interrupt, continue,
inspect history, change settings or compact an idle session.

## Embed the Go core

The library opens no listener or signal handlers. Your program owns its lifetime:
`Open` → create/select session → `StartTurn` → observe events → `Close`.
Use `Resume` explicitly to admit suspended work after reopening.

For a separate local Go application:

```sh
go mod init example.com/my-agent-host
go mod edit -require=github.com/vihanggodbole/sam-g-oss@v0.0.0
go mod edit -replace=github.com/vihanggodbole/sam-g-oss=/absolute/path/to/sam-g-oss
```

Import `sam "github.com/vihanggodbole/sam-g-oss"`. See the [library contract](LIBRARY.md)
for runtime setup, tools/hooks and recovery, and the [browser example](../example/README.md)
for a host using that API.
The optional `github.com/vihanggodbole/sam-g-oss/server` package maps HTTP requests to that same runtime.

## Tools, settings and context

The runtime enables six built-ins by default: `read`, `write`, `edit`, `bash`,
`history` and `subagents`. The browser example instead requires explicit opt-in.
Hosts register custom tools and approval/context callbacks through existing Go
configuration. Tools are not uploaded over HTTP and no plugin loader is provided.

- [Tools and output limits](LIBRARY.md#tools): oversized output fails the call
  with actionable guidance; no automatic retry or overflow archive.
- [Subagents](SUBAGENTS.md): one-level child sessions on the same runtime, with
  narrowed permissions and retained history. Shared-workspace edits are not coordinated.
- [Configuration](LIBRARY.md#configuration): explicit per-session overrides,
  idle boundaries and deliberate prompt/resource reload.
- [Model installation and handoffs](MODEL-INSTALLATION.md): discover and install
  definitions, then explicitly select another model/account for a used chat.
- [Compaction](LIBRARY.md#compaction): summarize context for future requests;
  original history remains in SQLite. A different summarizer provider receives
  the selected conversation content.

## Trust boundary and recovery

**Tools run as the host OS user. A workspace is not a sandbox.** There is no
built-in approval UI; without host hooks, enabled tools require no approval.
Anyone controlling the API can effectively operate that user's shell and read
sensitive files. Keep the API, databases, logs, exports and credentials private.
Known-credential redaction is not a general scrubber for arbitrary tool output.

Open leaves unfinished work suspended. Explicit Resume, or the stock server's
opt-in `server.resume_on_start`, continues through the normal executor. Saved
outcomes never rerun; unfinished replay-unsafe calls are skipped and reported to
the agent. Replay-allowed tools and hooks may repeat effects. Interrupt cannot
undo completed effects, and stopping a turn is not the same as stopping its
acknowledged background processes. See [recovery](LIBRARY.md#recovery).

## Current limitations

- Trusted-user deployment only: no public-user authentication, multi-user
  isolation, filesystem sandbox, or cross-machine delegation.
- No stock HTTP Resume endpoint, browser tool, MCP integration or production
  chat frontend. The local example has its own explicitly acknowledged Resume control.
- Provider eligibility, entitlement, model availability, revocation and cache
  behavior depend on the upstream service. No silent model/account/billing fallback.
  See [provider limitations](AUTH-MODELS.md#provider-limitations).
- Only the documented Responses/Messages profiles and inputs are supported.
  Missing provider metadata stays unknown; local budgets are not exact token counts.
- Runtime recovery is separate from process supervision. Configure and validate
  your own service manager and network access; installation and backgrounding do
  not start sam-g after reboot.

## Development

Use the [maintainer guides](maintainers/README.md) for source ownership and tests.
Only small unit tests are maintained; there is no integration or browser test suite.
They do not verify whole-system execution, restart or live providers. From the root:

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
bash -n scripts/install.sh scripts/run.sh
```

The [specification](SPEC.md) defines product behavior; [design](DESIGN.md) explains
ownership boundaries. [Model metadata](MODEL-METADATA.md) documents field mappings
and upstream attribution. Use `go doc github.com/vihanggodbole/sam-g-oss` for exact public declarations.
