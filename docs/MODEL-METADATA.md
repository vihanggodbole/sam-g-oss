# Discovery metadata and its sources

Discovery makes one explicit catalog GET. It never installs models, changes
budgets, enables provider policy, or substitutes another provider/account. Only
models actually returned by the selected account appear in the result. A separate
[explicit installation action](MODEL-INSTALLATION.md) can replace that account's
managed JSON model entries; it never turns advisory Pi facts into execution settings.

## Provider facts versus a reference

The top-level fields of `DiscoveredModel` are **provider-reported** facts (or the
ChatGPT catalog's documented Responses compatibility). Missing/null means unknown,
not unsupported. `reasoning_efforts: []` means an explicitly empty list. A thinking
boolean does not imply particular effort names or reasoning-summary support.

`reference`, when present, is **separate advisory metadata** from the versioned Pi
snapshot below. It has `source`, `context_tokens`, and `supports_reasoning` fields.
It never fills or overrides top-level facts, including explicit false/empty values.
Both browser clients label a displayed fallback as a reference, with its source.
A reference value is not a guarantee about an account, grant, transport or route.
In particular, Pi's `openai-codex` backend is not sam-g's public ChatGPT token-sharing
Responses endpoint. Even their context limits may differ.

Installed model `context_tokens` is the local budget used by sam-g, separate
from the discovery result. Discovery alone does not install it. During stock
snapshot installation, an explicit budget is a shared upper bound, still capped
by each provider-reported capacity. When omitted, each model uses its reported
capacity, falling back to 128,000 tokens only when that fact is unknown. Pi
reference values are not used for this choice. The example continues to start manually registered model
definitions with its existing conservative local budget and no assumed reasoning
declarations.

## HTTP field mappings

### ChatGPT token-sharing

The ChatGPT catalog uses `models[]` visibility, slug and display-name fields.
Optional metadata fields map as follows:

- `context_window` → `context_tokens`.
- `supported_reasoning_levels[].effort` → `reasoning_efforts`, preserving order.
- `supports_reasoning_summary_parameter` → `reasoning_summaries`; the older
  `supports_reasoning_summaries` spelling is recognized when the newer field is absent.

The shape is sourced from OpenAI's published model protocol:
[openai_models.rs at e57fc9ea](https://github.com/openai/codex/blob/e57fc9ea5a7ccdf1d45b5c9d50047ebd8e344b05/codex-rs/protocol/src/openai_models.rs).
The older summary field is also present in [rust-v0.99.0](https://github.com/openai/codex/blob/rust-v0.99.0/codex-rs/protocol/src/openai_models.rs).
These schemas define field meanings, **not** a promise that every token-sharing
catalog supplies them. Missing fields remain unknown. `max_context_window` is an
override ceiling, not silently substituted for the ordinary context window.

### GitHub Copilot

Recognize the direct model API fields consumed by VS Code Copilot Chat:

- `capabilities.limits.max_context_window_tokens` → `context_tokens`.
- `capabilities.limits.max_output_tokens` → `max_output_tokens` (maximum output, not a prompt limit).
- `capabilities.supports.reasoning_effort` → `reasoning_efforts`.
- `capabilities.supports.thinking` → `supports_reasoning`.
- `capabilities.supports.vision` → `supports_images`; false marks the installed model `no_images`.
- `supported_endpoints` containing `/responses` → `supports_responses: true`;
  an explicitly supplied list without it → false. Missing/null remains unknown.
- `supported_endpoints` containing `/v1/messages` → `supports_messages: true`;
  an explicitly supplied list without it → false. Missing/null remains unknown.

`api` is sam's selected supported codec, not another provider fact: prefer
`responses` when explicitly reported; otherwise select `anthropic_messages` only
for Copilot's exact `claude-opus-5.5` profile with reported Messages transport
and no contradictory provider-reported false thinking support.
Other Messages models remain transport-visible but have no supported selected
codec. Missing output capacity keeps a Messages entry needing configuration.
Do not infer an endpoint from a model prefix or an SDK request path.

Sources at Microsoft commit `5863f5a7088958050792b5dccbe8b46c6e13eccc`:
[endpointProvider.ts](https://github.com/microsoft/vscode-copilot-chat/blob/5863f5a7088958050792b5dccbe8b46c6e13eccc/src/platform/endpoint/common/endpointProvider.ts)
and [modelMetadataFetcher.ts](https://github.com/microsoft/vscode-copilot-chat/blob/5863f5a7088958050792b5dccbe8b46c6e13eccc/src/platform/endpoint/node/modelMetadataFetcher.ts).
`max_prompt_tokens` is deliberately not treated as total context capacity.
Thinking support does not prove Responses compatibility or summary support.
The cited response schema and fetcher do not declare a reasoning-summary support
field, so Copilot discovery leaves `reasoning_summaries` unknown. Installation
can retain a matching model's existing local declaration as described in
[the installation contract](MODEL-INSTALLATION.md#browser-flow-and-http-contract);
that does not turn it into a provider-reported fact.

Malformed typed fields, nonpositive context sizes, and blank/duplicate/control-
containing efforts fail the lookup with a generic error, without echoing the
upstream body. Unsupported optional fields are ignored. Generic API-key endpoints
receive neither these subscription-specific interpretations nor Pi references.

## Native Messages support

The supported native profile is Copilot **claude-opus-5.5**: adaptive thinking,
native mid-conversation system messages, and efforts `low`, `medium`, `high`,
`xhigh`, `max`. Budget-thinking profiles are not supported. Installation copies
reported effort lists without guessing missing values; native summary display is
a local profile declaration, not a provider-reported catalog fact.

Requests use Copilot bearer authentication and identification headers,
`/v1/messages?beta=true`, and `anthropic-version: 2023-06-01`. sam-g requests no
extra beta flags, eager streaming or cache hints. Ordered signed-thinking replay
is retained unchanged. This profile follows the public
[@earendil-works/pi-ai 0.87.1](https://www.npmjs.com/package/@earendil-works/pi-ai/v/0.87.1)
Anthropic Messages adapter and [Anthropic Messages API](https://docs.anthropic.com/en/api/messages).
Provider entitlement and acceptance remain external requirements.

The executable output bound must come from configuration or reported
`capabilities.limits.max_output_tokens`, never the advisory Pi reference.
See the [native request/settings contract](LIBRARY.md#native-messages-through-copilot).

## Pi reference snapshot

`model_reference.go` contains exact-ID reference data from **@earendil-works/pi-ai
0.87.1**, using only its published `contextWindow` and `reasoning` values:

| Subscription provider | Pi data file | SHA-256 |
| --- | --- | --- |
| ChatGPT | `dist/providers/data/openai-codex.json` | `4bb30a26d1b40e1f67c9f24891fca0ce25b030bc4cbbb529be78608cd4466fdf` |
| Copilot | `dist/providers/data/github-copilot.json` | `bb59f8e4ffca00a8e6031baee78f4bc39d0e95b7647452a3605decb7c4cff30a` |

These are reference facts, not copied authentication code or a runtime dependency.
There is no model-family/prefix matching, network update, catalog cache or extra
GET. A future/unmatched ID stays unknown. Every result gets a detached reference.
Pi's selected API transport is not used to infer support or lack of support for
other transports. Its thinking-level overrides are not treated as an exhaustive
provider effort list; reasoning summaries and exact efforts require provider facts
or the operator's separately verified declarations.

To update: inspect the public package data at a specified version, copy only exact
IDs and those two fields, update the version labels and hashes here, and run the
metadata and browser tests. Do not read Pi credential files or query real accounts.
A newer package is not silently consulted at runtime.
