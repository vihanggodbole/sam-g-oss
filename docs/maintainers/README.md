# Maintainer guides

A short map for contributors and coding agents changing `sam-g`: where behavior
lives, which contracts a change must preserve, and how to validate it safely.
This is implementation navigation, not another API manual. Use the
[Go library contract](../LIBRARY.md), [HTTP contract](../SERVER.md),
[product specification](../SPEC.md), and focused
[auth](../AUTH-MODELS.md), [metadata](../MODEL-METADATA.md),
[model installation](../MODEL-INSTALLATION.md), and
[design](../DESIGN.md) guides for authoritative details.

## Choose a guide

- [Architecture and flows](architecture.md): package ownership, source map, and
  the path from an accepted command to durable state and visible events.
- [Invariants](invariants.md): admission, persistence, cancellation, model and
  account identity, credential, protocol, and HTTP safety rules.
- [Changing and testing](changing-and-testing.md): small change recipes, test
  infrastructure, focused and full checks, and host/script boundaries.

## Start from the task

- **Execution, tools, receipts, history, or events:** start with
  [architecture](architecture.md#durable-execution-and-state), then
  [execution invariants](invariants.md#execution-and-durability).
- **Subagents, child settings, notices, or parent deletion:** read
  [SUBAGENTS.md](../SUBAGENTS.md) and the
  [one-level lifecycle invariants](invariants.md#one-level-subagents-share-session-ownership).
- **Tools, hooks, or compaction:** use
  [runtime extension points](architecture.md#runtime-extension-points), then
  [the change recipe](changing-and-testing.md#tools-and-hooks).
- **Config, prompts, catalog install, or handoff:** read
  [model/config invariants](invariants.md#configuration-and-model-choices), then
  [the change recipe](changing-and-testing.md#configuration-and-model-management).
- **Login, refresh, provider mapping, or discovery:** read
  [auth/protocol invariants](invariants.md#authentication-and-provider-protocols)
  and [AUTH-MODELS.md](../AUTH-MODELS.md).
- **HTTP, browser action, or CORS:** read
  [host boundaries](invariants.md#http-and-host-boundaries), then
  [the route/UI recipe](changing-and-testing.md#subagents-http-and-browser-clients).
- **Build/install/run helpers or tests:** read
  [Changing and testing](changing-and-testing.md).

For exported Go fields and methods, prefer `go doc github.com/vihanggodbole/sam-g-oss <symbol>` over
copying declarations into maintainer docs. Keep durable behavior in the library;
HTTP, CLI, and browser layers should call the same owner rather than implement a
second version of it.

## Current scope

The implementation has one executor with Responses/Messages codecs, six built-in tools (including
native one-level subagents), optional host Go tools/hooks, a thin HTTP/SSE adapter,
and a local browser example. Child work reuses sessions and steering; it is not a
second executor or a file-edit coordination system. See [SUBAGENTS.md](../SUBAGENTS.md).
