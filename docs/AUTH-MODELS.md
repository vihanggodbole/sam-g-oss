# Authentication and provider model discovery

sam-g supports direct Sign in with ChatGPT (SIWC), Copilot device login, protected SQLite credential storage, stateless discovery, account-wide plan blocking, and move-only transfers. Provider access is subject to the [limitations below](#provider-limitations).

The [library guide](LIBRARY.md) covers the runtime; the [server guide](SERVER.md) covers hosting and HTTP. This page owns the authentication/discovery details. Use `go doc github.com/vihanggodbole/sam-g-oss AuthManager` for exact declarations.

## One auth store, separate from conversations

A normal application has one `AuthManager`, one `Runtime`, and many sessions. Several runtimes may share one manager. The manager owns a **separate SQLite database**, not the conversation database and not a JSON credential file plus another status database.

```go
auth, err := sam.OpenAuth(sam.AuthConfig{
    DatabasePath: "/home/me/.sam-g/auth/auth.db",
    OpenAI: &sam.OpenAIChatGPTAuth{AgentName: "sam-g"},
    GitHubCopilot: &sam.GitHubCopilotAuth{}, // bundled Copilot Plugin client; optional ClientID override
    HTTPClient: client, // optional; host retains ownership
})
// Check err. Close every runtime using auth before auth.Close().
cfg.Auth = auth
```

`AgentName` must be the embedding app's actual nonblank name, consistently used across installations. OpenAI uses dynamic public-client registration, not a configured client secret or a borrowed Codex client ID. Copilot uses the public GitHub Copilot Plugin client ID and Pi-compatible request profile described below; an explicit ClientID can override the bundled registration. API-key providers remain independent.

Provider modes:

- `AuthOpenAIChatGPT`, JSON `"openai-chatgpt"`: exactly `https://api.openai.com/v1`. Empty `base_url` selects it. Backend-api and arbitrary proxy targets are rejected.
- `AuthGitHubCopilot`, JSON `"github-copilot"`: an explicit HTTPS `githubcopilot.com`/subdomain origin, or the same trusted origin obtained from the account's token exchange. Ports, userinfo, paths, queries and unrelated domains are rejected.
- Empty `Auth`: explicitly configured API key or environment variable and an OpenAI-compatible HTTP(S) base URL. Subscription failures never switch to this billing path.

Credentials use the configured SQLite store. Credential JSON stores and other
bundle formats are not supported; if no supported store or bundle is available,
use `auth login` to authorize a fresh account. Do not rewrite credential files to
bypass format checks.

The auth directory must be owner-only (`0700`), including an existing directory. Database, WAL, SHM, lock and bundle files must be owner-only (`0600`). The existing exclusive ownership lock prevents a second manager/process from opening the store. SQLite URI escaping and hard-link checks retain the same file-identity safeguards as runtime storage. Auth and conversation databases reject being opened as each other's store.

The small schema is `auth_meta` (version and installation host ID), `accounts` (label/provider/generation/status, registration, private credentials and plan facts), and `auth_requests` (operation identity, target generation, pending/final state and safe result). Credential changes and receipts commit together; in-memory account handles are published only after commit. Uncertain persistence stops credential use until Close/reopen; there is no old-token fallback.

The host ID is a persisted UUIDv4 URI (`urn:uuid:…`), created before first authorization. It survives restart, account changes and sign-out. Import never replaces the destination's host ID.

**Trust boundary:** tokens never enter conversation storage, ordinary account/status output, hooks, events or logs. The authorization URL contains transient state/nonce, but no retained ID-token hint. ID tokens join access/refresh/GitHub tokens in credential redaction. Auth storage and bundles are readable by tools running as the same OS user. Logical token clearing does **not** erase old SQLite pages, filesystem backups or exported copies. Protect and clean up those files as secrets; there is no encryption/keychain broker.

Explicit [human-owned file viewing](LIBRARY.md#human-owned-file-viewing) is a
different surface from redacted account/status output: the trusted owner may
deliberately read any OS-readable regular UTF-8 file, including a protected
transfer bundle or other sensitive text. No secret-filename blocklist or file
content redaction is promised; do not grant this API to untrusted frontends.

## Copilot compatibility profile

Copilot device login defaults to `DefaultGitHubCopilotClientID`, **`Iv1.b507a08c87ecfe98`**, the public [GitHub-owned Copilot Plugin registration](https://api.github.com/apps/github-copilot-plugin) used by Pi. This is application identification, not a secret or a sam-g-owned app. Users still approve their own account in GitHub. A nil `AuthConfig.GitHubCopilot` disables the flow; a non-nil empty configuration enables the bundled client. The example enables it by default. ClientID/`-copilot-client-id`/`SAM_G_COPILOT_CLIENT_ID` remain optional overrides.

The compatibility profile is pinned to **`@earendil-works/pi-ai 0.87.1`** (`dist/auth/oauth/github-copilot.js`, `dist/providers/data/github-copilot.json`, `dist/api/github-copilot-headers.js`, and the Responses client):

| Request | Copilot-specific wire behavior |
| --- | --- |
| GitHub device challenge and polling | Bundled/overridden client ID, `read:user`, `User-Agent: GitHubCopilotChat/0.35.0`; normal JSON accept/form encoding. |
| Copilot token exchange | GitHub credential as **Bearer**, plus the four profile headers below. |
| Catalog | Copilot Bearer token, the profile headers and `X-GitHub-Api-Version: 2026-06-01`. |
| Messages | Same Copilot bearer/profile and canonical `X-Initiator`, `Openai-Intent`, `X-Client-Request-Id`; `/v1/messages?beta=true`, `anthropic-version: 2023-06-01`. Text, user images and local tools only; no Claude Code identity, extra beta flags, binding-drop or API-key fallback. |
| Responses | Copilot Bearer token and profile headers; `Openai-Intent: conversation-edits`; `X-Initiator` user/agent attribution; stable session `X-Client-Request-Id` and `prompt_cache_key`; `Copilot-Vision-Request: true` only when image input is present. Function tools explicitly use `strict:false`. |

Profile headers are `User-Agent: GitHubCopilotChat/0.35.0`, `Editor-Version: vscode/1.107.0`, `Editor-Plugin-Version: copilot-chat/0.35.0`, and `Copilot-Integration-Id: vscode-chat`. These deliberately match Pi's Copilot compatibility identity; they are not a claim that sam-g is VS Code or that GitHub endorses this integration. They never apply to SIWC or API-key providers.

Token `proxy-ep` selects the Copilot origin first. If absent, a validated returned API endpoint is used; if neither is present, the Individual API is used, as in Pi. All candidate endpoint metadata remains subject to sam-g's strict HTTPS vendor-origin check, with redirects refused. Copilot renewal is on demand **five minutes before expiry**, matching Pi's margin; reported expiry remains the actual provider timestamp. A just-renewed short-lived token is checked against actual expiry, not incorrectly rejected just for being inside that margin.

**Intentional differences from Pi:** discovery stays explicit/stateless and never performs model-policy enablement POSTs or hidden catalog retries; unconfigured policy remains `requires_action`. Enable such a model through a supported Copilot client/settings flow before refreshing the catalog. Sam retains one executor with Responses and the initial Copilot Opus 5.5 native Messages codec. It does not add Chat Completions, a static availability catalog, enterprise-domain flow or account/billing fallback. A catalog entry alone does not prove transport support. See [native protocol/settings](LIBRARY.md#native-messages-through-copilot) and [supported native profile](MODEL-METADATA.md#native-messages-support). Header parity does not prove account/organization entitlement or future endpoint compatibility. Existing credentials minted under another client are not relabelled—sign out and authorize again to use the bundled registration.

## Local browser sign-in

Scenario: sign in on your laptop, then move that token session to a remote host. A loopback callback reaches the computer running the browser, not the remote server. Complete the browser flow locally before transferring the protected session.

The reusable core opens no listener or browser. The host:

1. Binds `127.0.0.1` on an available port.
2. Calls `StartLogin` with the exact `http://127.0.0.1:<port>/auth/callback` URI.
3. Presents `LoginStatus.AuthorizationURL` for the system browser.
4. Constructs the callback's full URL from its bound listener and the received path/query, **not the request Host header**, and calls `CompleteLogin`.
5. Waits for terminal status and closes its listener.

```go
st, err := auth.StartLogin(ctx, sam.LoginRequest{
    RequestID: "login-1", Provider: sam.AuthOpenAIChatGPT,
    Account: "personal", RedirectURI: callbackURI,
    EnablePlanUsage: false,
})
// Check err; show st.AuthorizationURL locally.
st, err = auth.CompleteLogin(ctx, sam.CompleteLoginRequest{
    LoginID: st.ID, CallbackURL: fullCallbackURL,
})
```

`EnablePlanUsage: true` is the user's explicit choice to request plan-use consent. It emits `prompt=consent`; routine returning sign-in omits it. It does not guarantee that permission will be granted. Copilot rejects these OpenAI-only fields and retains `device_code` challenges (`VerificationURL`, `UserCode`, provider expiry and polling interval). OpenAI status identifies `authorization_code`.

For initial registration, authorization uses `https://auth.openai.com/api/accounts/authorize`, `client_id=dynamic_agent_client`, real `agent_name_hint`, the stable host ID, resource `https://api.openai.com/v1`, and scopes `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`. Returning authorization uses the saved issued client ID and omits the name hint. Optional login/ID-token hints are intentionally omitted; returning users may see the account selector.

Each attempt has fresh random state, nonce and a 32-byte PKCE verifier with S256. Only the loopback port may vary between attempts; the exact redirect URI is unchanged within an attempt. Callback state is checked on success **and denial**. Duplicate relevant parameters, mismatched URI/client/state and ambiguous success/error callbacks are rejected without consuming another attempt. A matching denial never exchanges a code. Completion is one-shot.

The issued client ID, never `dynamic_agent_client`, is used at `/api/accounts/oauth/token` with code/verifier/exact redirect/resource and no secret. Incomplete issued registration is retained separately from a verified identity, including after `invalid_grant`; use a fresh request ID and fresh browser attempt. A returning verified issuer/subject/client ID cannot be replaced by a different identity. Use another label for another registration. Duplicate active registrations are rejected.

ID tokens use a bounded stdlib RS256 verifier and a fresh lookup at the fixed trusted `https://auth.openai.com/.well-known/jwks.json`. It checks signature, exact issuer, audience/authorized party, subject, numeric times and login nonce. It rejects algorithm confusion, malformed/ambiguous tokens, unknown/invalid keys and duplicate top-level JWT fields; token-supplied URLs never select a key source. Refresh-returned ID tokens are identity-checked without requiring a new login nonce. Access-token metadata remains opaque; there is no `chatgpt_account_id` dependency.

Required token fields, Bearer type, positive numeric expiry and granted scopes are validated. `offline_access` without a refresh token is incomplete. A valid identity-only grant remains signed in but has `PlanUsageEnabled=false`: no plan inference or discovery is sent. To request permission, start a new explicit consent authorization. There is no automatic OAuth loop or `force_reconsent` assumption.

### Login ownership and receipts

`StartLogin` deduplicates request acceptance. Changing account/provider/redirect/consent under the same request ID conflicts. `LoginStatus`, `Logins`, `WaitLogin` and `CancelLogin` expose safe state. Browser attempts have no invented device-code timeout: they wait until completion, cancellation or manager Close. Copilot honors its provider's expiry and `slow_down`.

PKCE verifier, code, nonce, state and authorization URL stay in the live attempt, not durable receipts. Active replay can return the transient URL. Terminal/reopened status has an empty URL; after process loss, unfinished attempts are cancelled and an old request ID cannot start another flow.

An accepted callback exchange belongs to the manager. Before sending the authorization code, the manager durably records that the provider may issue a session. Disconnecting its waiter does not discard an owned token write. `CancelLogin`, account retirement and Close can cancel and join it. Initial Copilot device-request I/O remains caller-bounded; accepted polling is manager-owned. If an exchange is cancelled, its reply is lost, or a successful issuance is discarded, sign-out remains unconfirmed even after that worker settles or the manager reopens. A known definite provider rejection clears only this attempt's temporary uncertainty. A crash after the durable marker but before the HTTP request can conservatively leave a warning even if no session was issued; this closes the otherwise unknowable crash gap without a remote-job queue.

[`go run ./example`](../example/README.md) starts a beginner-friendly local page with sign-in and chat, using the standard same-origin HTTP API. The local command below is another listener host.

## Automatic on-demand refresh

Access credentials renew on demand before discovery or inference through one per-account coordinator: at expiry for SIWC, within five minutes of expiry for Copilot. Other accounts do not wait. There is no idle refresh timer or separate discovery token cache.

The initiating caller controls refresh I/O and joins its settlement; other waiters can stop waiting. Manager Close/logout/export can cancel and join that registered work. Known errors remain distinct from identifiable cancellation. Rotations are saved atomically before use, retaining login generation, verified identity and plan block. A retiring or replaced generation cannot become usable because a refresh finishes late.

OpenAI refresh posts `grant_type=refresh_token`, issued `client_id`, current refresh token and resource to `/api/accounts/oauth/token`, without scope. Replacement credentials/scopes/expiry are validated together. `earliest_refresh_at` is retained **opaquely**: the runtime does not interpret its scheduling semantics. Renewal remains at documented expiry; returned Retry-After advice is separate from quota reset.

Before an actual rotating SIWC refresh sends provider I/O, the manager commits rotation uncertainty to auth SQLite. Validated replacement credentials must commit before that marker is resolved, including a late valid response during retirement. A definite token-endpoint rejection clears only this attempt's marker, preserving older uncertainty; a later signing-key/identity validation failure is not a token-endpoint refusal. Process death after the marker but before sending can conservatively leave a warning even if nothing rotated. This small write closes the crash gap without a second credential ledger or recovery queue. Copilot's nonrotating exchange of its saved GitHub token does not use this marker.

Terminal refresh codes clear unusable tokens while retaining the registration. `invalid_client` requires fixing client setup. Transport loss or malformed success may leave rotation uncertain; that uncertainty persists and prevents a later sign-out from falsely claiming confirmed remote revocation. A successful renewal can resolve uncertainty from rotation of that known session, but never clears separate uncertainty from an authorization that may have added another session.

Copilot refresh exchanges the saved GitHub token for short-lived Copilot access, on demand within the five-minute renewal margin. A Copilot 401 may use the executor's existing one forced-refresh cycle and four total model-attempt slots. **SIWC does not blindly force refresh on every 401.** It surfaces the selected identity/grant failure, preserving credentials unless refresh establishes unusability.

## Sign-out and plan limits

```go
out, err := auth.Logout(ctx, sam.AuthAccountRequest{
    RequestID: "logout-1", Account: "personal",
})
// out.LocalSignedOut; out.RemoteRevocation; out.Warning
```

Sign-out first commits `signing_out`/unusability and a pending receipt. Replacement login conflicts until settlement. It cancels/joins pending login and refresh work, preserving the latest known rotation where possible, then attempts OpenAI refresh-token revocation at `/api/accounts/oauth/revoke` with issued client ID and `token_type_hint=refresh_token`.

At most three attempts use provider Retry-After or 1/2-second fallback waits. Caller context owns remote waiting. Stock HTTP and CLI hosts impose ten seconds, capped by an earlier deadline; a direct Go `Background()` caller has **no wall-clock bound** merely from the attempt count. Custom I/O must cooperate. Already-admitted inference may finish.

Access, refresh and ID tokens are cleared locally even when revocation cannot be confirmed. Registration, host ID, plan block and any outstanding authorization uncertainty remain. Revoking the currently saved refresh token cannot prove that an additional session from a cancelled or lost authorization was also removed. That uncertainty survives ordinary refresh, reauthorization of the same registration, settlement and Close/reopen; a known definite rejection does not create it. There is no API to attest that manual cleanup removed an unknown session, so the same registration continues to report unconfirmed even after the user disconnects it manually. `LogoutResult.RemoteRevocation` is `confirmed`, `unconfirmed`, or Copilot's `not_applicable`; uncertain OpenAI cleanup includes guidance to disconnect in [ChatGPT Settings](https://chatgpt.com/settings/usage). There is no remote retry queue. Close/reopen finishes accepted incomplete retirement locally as unconfirmed. Storage failures remain real errors.

Receipts describe **that mutation**, not current account state. Replaying an old logout cannot revoke a newer login. Inspect `Accounts()` for current state: retained labels, status, safe identity, expiry, plan permission/block/reset/error facts, pending operation, last transfer ID and `exported`/`export_unconfirmed`/`imported` transfer status.

### When a plan request hits a limit

Scenario: two runtimes share `work`; one receives `subscription_sharing_usage_limit_exceeded`. The manager atomically saves that registration's block in auth SQLite. Every new SIWC inference HTTP attempt checks it, including retries and compaction. Work already admitted may finish. The failed operation stops normally; there is no account switch or API-key fallback.

The block survives reopen, refresh and routine reauthorization. Catalog/status reads remain usable when normal authentication/scopes allow them. There is no timer or guessed quota window. `AccountInfo.PlanResetAt` is nullable: **the current official SIWC documentation establishes no concrete reset field**, so the adapter currently leaves it unknown. Generic Retry-After/rate-limit headers and fabricated fixture fields are not mapped to plan reset.

After reviewing Usage settings, the host can explicitly acknowledge another try:

```go
info, err := auth.AllowPlanUsageRetry(ctx, sam.AuthAccountRequest{
    RequestID: "usage-reviewed-1", Account: "work",
})
```

This only clears the local block and current hints. It grants no scope, adds no credits, calls no provider and starts no turn. The host separately calls Continue/Resume where appropriate. Replaying that old request ID cannot clear a later block. This is shared **local** state, not cross-machine quota synchronization.

`Error.ProviderFailure` and terminal/attempt/compaction error events preserve upstream status, exact safe code/param/request ID, safe message, `error`/`detail`/`unknown` shape, retryability, nullable retry seconds and nullable verified reset time. There is no raw-body bag. Stream errors retain the actual transport status (often HTTP 200 followed by a provider failure); an unknown status is omitted from readable text, never displayed as HTTP 0 or guessed as 429 from a quota code. Final exhausted Copilot 401 errors retain the final request's structured diagnostics. Error bodies exceeding the existing 4 KiB diagnostic bound retain known status/request ID but return an explicit unclassified, nonretryable failure instead of guessing a quota code. Exact SIWC limit/eligibility/capability/route codes precede generic 429/503 retry handling. Usage/user unavailability may use the existing bounded model-attempt ledger; discovery never adds its own retry loop. Failed/incomplete/EOF responses stay distinct, even after streamed text; only complete validated Responses completion or Messages terminal `message_stop` can authorize tools.

## Move a selected session to another machine

OpenAI documents local OAuth, protected transfer and VM-owned renewal. **Move-only export and stock-server downtime are sam-g policies**, not literal vendor requirements.

```text
sam-g auth login    -config laptop.json -account personal
sam-g auth accounts -config laptop.json
sam-g auth export   -config laptop.json -account personal -out /private/transfer/account.json
# Transfer over a host-owned secure channel, for example SSH.
sam-g auth import   -config vm.json -account work -in /private/transfer/account.json
sam-g auth accounts -config vm.json
```

Use `-enable-plan-usage` for explicit OpenAI consent, or `-provider github-copilot` for device login. `auth logout -config … -account …` uses the same truthful sign-out API. These commands open **no Runtime/conversation database**. Stop the relevant server first because the auth database has one owner. An embedding app already owning the manager uses its methods directly.

```go
result, err := auth.ExportAccount(ctx, sam.ExportAccountRequest{
    RequestID: "move-1", Account: "personal", Path: protectedPath,
})
info, err := auth.ImportAccount(ctx, sam.ImportAccountRequest{
    RequestID: "import-1", Account: "work", Path: protectedPath,
})
```

Export reserves a transfer ID/receipt and disables the source before file work, settles owned refresh/login work, atomically writes one protected versioned bundle, then clears source tokens and finalizes. **It never revokes the moved session.** The bundle contains registration/session/plan facts, not host identity or unrelated accounts. Its destination must not already exist and its directory must be owner-only. If refresh-token rotation itself is uncertain, the source is still deactivated but export refuses to make a bundle; sign in again before retrying. By contrast, uncertainty that a cancelled authorization may have added an **additional** remote session does not block moving the current known credentials. The bundle carries that fact, and the successful result warns that other remote sessions may remain. `TransferResult.Confirmed` means the source was deactivated and the bundle was written; it does not prove that every remote session was removed.

An interrupted export never automatically reactivates the source. On reopen, source deactivation is finalized and the transfer is unconfirmed. Inspect safe account/receipt status. Use a fully written bundle **only after confirming source deactivation**; a missing/invalid bundle or lost rotation requires fresh sign-in. There is no cross-file or cross-machine atomicity promise.

Import accepts only this app's supported protected bundle from the user's trusted machine. Exports and imports use only `sam-g-account-v1`; imports validate identity, replay and destination state. Signature/registration consistency checks do not prove ownership of arbitrary opaque access/refresh token pairs; an expired retained ID token is historical identity, not a fresh login. Active destinations and duplicate active sessions/registrations are rejected. Destination host ID/unrelated accounts remain. For a laptop-to-VM move, authorization uncertainty travels with the bundle; importing into an inactive matching registration also preserves uncertainty already known by the VM. Fresh credentials cannot prove that an older possible remote session disappeared, so a later VM sign-out remains unconfirmed when either machine already knew of that possibility. An existing destination plan block wins; otherwise source block/reset facts carry over.

The transfer-ID receipt commits with credentials. Reimporting an old transfer under a new request ID cannot replace rotated credentials. After a lost acknowledgement, inspect `Accounts()`/last transfer ID instead of overwriting. Remove bundle copies after confirmed import; backups and filesystem remnants remain secrets. No HTTP export/import, SSH client, tunnel, sync service or browser-automation framework is included.

## Discover upstream models without installing them

```go
models, err := sam.DiscoverModels(ctx, sam.DiscoverModelsRequest{
    Provider: cfg.Providers["chatgpt"], Account: "work",
    Auth: auth, HTTPClient: client,
})
// Or with an existing runtime:
models, err = rt.DiscoverModels(ctx, "chatgpt", "work")
```

`work` is a saved account label, not a workspace/model. Subscription selection is mandatory; API-key discovery rejects a nonempty account. `DiscoverModelsRequest` is Go-only wiring and may contain private configuration: never serialize/log it as an HTTP body. API-key discovery requires no auth database; subscription discovery uses the manager's store but opens no other database.

Each call makes **one catalog GET after normal auth resolution**. Token renewal and an observed exact account-limit error can update auth state, but catalog contents never persist. No catalog persistence, TTL/cache/singleflight, background polling, catalog retries, model-policy POSTs, fallback availability catalogs, inference, receipts, hooks, config changes or alias installation. Cancellation belongs to the lookup. A selected login generation is pinned before I/O; an admitted GET may finish after logout/reload but cannot change identity. Hosts discard stale picker results after selection changes and retain supplied resources until calls finish.

`DiscoveredModel` contains exact `ID`, `DisplayName`, `Availability`, and selected supported `API`, and nullable `SupportsResponses`, `SupportsMessages`, `MaxOutputTokens`, `ContextTokens`, `ReasoningEfforts`, `ReasoningSummaries`, `SupportsReasoning`, `SupportsImages`. A separate optional `Reference` contains versioned, advisory Pi metadata; it never replaces provider facts or populates missing models. See [metadata sources and mappings](MODEL-METADATA.md). Nil means unknown; a reported empty reasoning list would mean none. Required invalid/duplicate IDs fail the query. Order is preserved and missing display names fall back to ID. The fixed catalog envelope cap is 32 MiB, checked with cap-plus-one detection independently of the SSE setting; unsupported envelopes/pagination fail rather than silently truncate.

Adapters:

- SIWC: Bearer `GET https://api.openai.com/v1/models`; `models[]`, `visibility=list`, `slug` and `display_name`. These documented selections support Responses. Optional `context_window`, `supported_reasoning_levels`, and reasoning-summary support fields are mapped when present; missing fields remain unknown.
- Copilot: exchanged Copilot token (not GitHub token), trusted-origin `GET /models`; observed `data[]` with `id`, optional `name` and policy. Enabled → available, disabled → unavailable, unconfigured → requires_action, missing/unknown → unknown. False picker flags do not hide enabled entries. Maps the documented VS Code direct-HTTP context/output limits, thinking/effort fields, and `supported_endpoints`, preserving missing versus false/empty. Prompt-only limits do not become context windows. Uses the pinned Pi-compatible Copilot headers above. No policy enablement or fallback availability catalog.
- API key: explicit configured key/environment, `GET <base>/models`, `data[].id`, availability/capabilities unknown. Custom Responses endpoints may not implement that catalog; errors are returned, not hidden by a subscription/API-key fallback.

`Runtime.Models()` and `GET /v1/models` remain the installed configured aliases and budgets. Separately, the stock server offers [explicit snapshot installation and model handoff](MODEL-INSTALLATION.md); those actions do not change the stateless Go discovery contract. Provider discovery is separate: `GET /v1/providers/{provider}/models?account=work`. It needs no Idempotency-Key and is usable while sessions work. Choosing a result does not mutate a bound conversation. A host explicitly installs definitions through `UpdateModels`/`UpdateModelsAndSave` for new work, without idle-chat coordination or prompt/settings reload. Full configuration Reload remains all-idle; unknown budgets/options remain the host’s configuration choice.

## Inference and restart-safe tool identity

SIWC uses only public `/v1/responses`, full client-managed input, `store:false`, `stream:true`, base `instructions`, and a stable `sam` function namespace with `strict:false`. It emits none of the preview-forbidden parameters and no explicit system message items. BeforeTurn/BeforeCompaction remain developer instructions; BeforeRequest remains labelled user-role application data. Original history is not role-converted.

Namespace/name must match the tools offered by the prepared request. The validated local dispatch target **or an explicit unavailable decision** is committed with the completed response. Restart consumes that saved authorization, never today's provider config or dotted-name guessing; current tool availability/BeforeTool still apply. Legacy flat rows remain explicitly unnamespaced. Original raw namespace/name/call IDs stay in history and public inspection.

There is one executor and no public namespace router, second model engine, Codex app-server, Copilot CLI/SDK dependency or plugin framework.

## Provider limitations

Account eligibility, model entitlement, inference, cache behavior and remote
revocation depend on the provider. A successful login or catalog response does
not guarantee access to every model or protocol feature. Copilot's compatibility
profile is not a public guarantee from GitHub. Automated coverage is limited to
small unit tests; it establishes neither complete account flows nor live-provider
acceptance.

- Upstream denials remain visible. No automatic policy enablement, role
  substitution, or model/account/billing fallback is performed.
- `earliest_refresh_at` does not schedule renewal; the runtime uses documented
  expiry. Quota-reset time remains unknown unless the provider reports it.
- Namespace/developer-role support and replay across account changes must be
  accepted by the selected provider; the runtime does not conceal incompatibility.

Public references: OpenAI's [SIWC overview](https://developers.openai.com/siwc/token-sharing-open-source),
[registration](https://developers.openai.com/siwc/token-sharing-open-source/sign-in),
[models/inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference),
[preview limits](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations),
[refresh/revocation](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions),
[VM setup](https://developers.openai.com/siwc/token-sharing-open-source/self-hosted-vms),
[token reference](https://developers.openai.com/siwc/token-sharing-open-source/token-reference),
and [errors](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery).
See [model metadata sources](MODEL-METADATA.md) for catalog mappings and attribution.
