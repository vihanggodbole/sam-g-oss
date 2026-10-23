// Package sam provides an embeddable runtime for persistent, tool-using agent
// sessions. It can be used directly by a Go application; an HTTP server is not
// required. The package opens no listener and installs no process signal
// handlers.
//
// A typical host opens a Runtime with Open, creates a session for an existing
// workspace, and calls StartTurn. StartTurn saves the request and returns a
// turn ID; the work then continues in the background. Subscribe replays saved
// events and follows new ones, while Steer adds guidance and Interrupt stops
// work for one session. Continue starts a new turn after the previous one
// ended. Close suspends eligible work and releases SQLite after joining owned
// resources. Open loads latest configuration without launching saved work;
// Resume explicitly opts into recovery. Unfinished replay-unsafe tools are
// skipped with results for the agent; replay-allowed tools/hooks may repeat
// effects. Saved known outcomes never rerun. The host owns the
// runtime lifetime. See Runtime.Resume.
//
// Providers and models are declared in Config; model capabilities are not
// discovered automatically. Sessions use the configured tools plus optional
// host tools. Tools run with the host process's authority: a workspace is a
// working directory, not a security sandbox. Configure only trusted workspaces
// and tools. Reload replaces typed shared configuration and prompt contents only
// when all sessions are fully idle. SessionOverrides tracks explicit choices;
// removing an override resumes inheritance. GetConfig returns effective settings
// separately from overrides. Workers and inspection never reread prompt files.
//
// Config.Hooks registers host-owned BeforeTurn, BeforeRequest, BeforeTool and
// BeforeCompaction callbacks. Multiple handlers run serially with detached inputs
// and no order guarantee. Policies explicitly allow or deny; hosts own approval
// UI and cancellation cooperation. Accepted text appends to attributed extension
// history, never the leading system prefix, and survives later failures/restarts.
// Reload replaces hooks through the same all-idle boundary. Inspection, replay
// and transport retries never invoke them. See Hooks.
//
// Providers can use direct Sign in with ChatGPT or GitHub Copilot instead
// of API keys. AuthManager owns a separate private SQLite database containing
// registrations, credentials, plan blocks and mutation receipts. ChatGPT uses
// browser authorization with a host-owned loopback listener; Copilot retains
// device login. Expired tokens refresh on demand. A live turn pins its selected
// login generation; there is no account or billing fallback. Logout reports
// whether remote revocation was confirmed. Protected local ExportAccount and
// ImportAccount move a session between trusted machines without revoking it.
//
// DiscoverModels queries a provider explicitly, without a runtime, inference,
// catalog cache or config mutation. Runtime.Models remains configured aliases.
// Live SIWC acceptance and authorized-client Copilot entitlement require a
// separately approved integration check; local tests do not establish them.
//
// Accepted session requests, conversation history, tool outcomes, and numbered events
// are persisted locally in SQLite. Use a unique RequestID for each session mutation
// and reuse that same ID if its acknowledgement is uncertain. Request contexts
// do not cancel work after it has been accepted. Events can be replayed after a
// disconnect; their Category is a presentation hint, not a guarantee that a
// turn succeeded. Turn and compaction failures discovered after acceptance
// are reported through events and their operation records.
//
// Configuration and lifecycle details are in docs/LIBRARY.md. The package's
// role alongside the optional HTTP server is described in docs/README.md.
package sam
