"use strict";

const streamKinds = [
  "turn.started", "turn.completed", "turn.failed", "turn.cancelled", "turn.interrupted",
  "text.delta", "reasoning.delta", "refusal.delta", "response.refusal",
  "tool.started", "tool.output", "tool.completed", "tool.failed", "tool.cancelled", "tool.attempt.interrupted",
  "model.attempt.started", "model.attempt.ended", "model.retry", "usage",
  "compaction.started", "compaction.completed", "compaction.failed", "compaction.cancelled",
  "operation.suspended", "operation.resumed", "steering.accepted", "steering.applied", "steering.cleared",
];

function clearLive() {
  view.events?.close();
  view.events = null;
  view.live = null;
  $("live").replaceChildren();
  $("stream-status").textContent = "not connected";
}

function watchEvents(owner) {
  clearLive();
  const id = owner.id, snapshot = owner.session;
  owner.live = {
    baseline: snapshot.last_event_seq || 0, seen: 0,
    operations: new Set([snapshot.active_turn_id, snapshot.active_compaction_id].filter(Boolean)),
    cards: new Map(),
  };
  // Replay also recovers already-streamed portions of CURRENT work after a
  // page reload. Historical finished operations are filtered by the snapshot.
  const events = owner.events = new EventSource(`/v1/sessions/${encodeURIComponent(id)}/events`);
  events.onopen = () => { if (owner === view) $("stream-status").textContent = "connected"; };
  events.onerror = () => { if (owner === view) $("stream-status").textContent = "reconnecting; accepted work continues"; };
  for (const kind of streamKinds) events.addEventListener(kind, (message) => {
    if (owner !== view) return;
    const event = JSON.parse(message.data);
    if (event.session_id !== id) return;
    consumeEvent(event);
  });
}

// Terminal failures are replayed from durable events even for finished turns.
// They are neither assistant content nor transient retry/attempt failures.
function renderFailure(event) {
  const p = event.payload || {}, box = document.createElement("article");
  box.className = "message failed"; box.setAttribute("role", "alert");
  const title = document.createElement("strong"), text = document.createElement("p");
  title.textContent = `${event.kind === "turn.failed" ? "Turn failed" : "Compaction failed"} · ${event.turn_id || event.operation_id}`;
  const failure = p.provider_failure;
  // Older saved outer messages may predate status-aware formatting. Render
  // their structured facts without rewriting history or repeating HTTP 0.
  text.textContent = failure?.message
    ? `${failure.message}\n${failure.code || p.code || "provider_error"}${failure.http_status ? ` · HTTP ${failure.http_status}` : ""}`
    : p.error || p.code || "The operation failed.";
  box.append(title, text);
  const details = document.createElement("details"), summary = document.createElement("summary"), raw = document.createElement("pre");
  summary.textContent = "Safe failure details";
  raw.textContent = JSON.stringify({ code: p.code, provider_failure: p.provider_failure, turn_id: event.turn_id, operation_id: event.operation_id }, null, 2);
  details.append(summary, raw); box.append(details);
  $("turn-failures").prepend(box);
}

function liveCard(key, operation, label) {
  const liveState = view.live;
  let card = liveState.cards.get(key);
  if (card) return card;
  const box = document.createElement("article");
  box.className = "message live-card";
  const title = document.createElement("strong"), text = document.createElement("pre");
  title.textContent = label;
  box.append(title, text);
  $("live").append(box);
  card = { box, title, text, operation, settled: 0 };
  liveState.cards.set(key, card);
  return card;
}

function consumeEvent(event) {
  const liveState = view.live;
  if (!liveState || event.seq <= liveState.seen) return;
  liveState.seen = event.seq; // at-least-once reconnects cannot duplicate text
  const operation = event.operation_id || event.turn_id || "session";
  if (["turn.failed", "compaction.failed"].includes(event.kind)) renderFailure(event);
  if (event.seq <= liveState.baseline && !liveState.operations.has(operation)) return;
  liveState.operations.add(operation);
  const p = event.payload || {};
  const prefix = `${operation}/${event.step || 0}/${event.attempt || 0}`;
  const label = `Step ${event.step || "—"} / attempt ${event.attempt || "—"}`;
  if (["text.delta", "reasoning.delta", "refusal.delta"].includes(event.kind)) {
    const kind = event.kind.split(".")[0];
    liveCard(`${prefix}/model/${kind}`, operation, `${label} · ${kind} · provisional`).text.textContent += p.text || "";
  } else if (event.kind === "tool.started" || event.kind === "tool.output") {
    const card = liveCard(`${prefix}/tool/${event.call_id}`, operation, `${label} · tool ${p.name || event.call_id}`);
    card.toolResult = `${event.turn_id}/${event.call_id}`;
    if (event.kind === "tool.started") card.text.textContent = JSON.stringify(p.arguments, null, 2) + "\n";
    else card.text.textContent += `[${p.stream || "output"}] ${p.text || ""}`;
  } else if (["tool.completed", "tool.failed", "tool.cancelled"].includes(event.kind)) {
    const card = liveCard(`${prefix}/tool/${event.call_id}`, operation, `${label} · ${p.name || event.call_id}`);
    card.title.textContent += ` · ${event.kind}`;
    if (event.kind === "tool.failed") card.box.classList.add("failed");
    card.text.textContent = p.result || "";
    card.settled = event.seq;
    card.toolResult = `${event.turn_id}/${event.call_id}`;
  } else if (event.kind === "model.attempt.ended") {
    for (const [key, card] of liveState.cards) if (key.startsWith(`${prefix}/model/`)) {
      card.title.textContent += ` · ${p.outcome}`;
      card.settled = event.seq;
    }
  } else if (["model.retry", "tool.attempt.interrupted", "operation.suspended", "operation.resumed"].includes(event.kind)) {
    const card = liveCard(`${prefix}/${event.kind}/${event.seq}`, operation, `${event.kind} · ${label}`);
    card.text.textContent = p.warning || p.reason || JSON.stringify(p, null, 2);
    card.settled = event.seq;
  }
  const terminal = /^(turn|compaction)\.(completed|failed|cancelled|interrupted)$/.test(event.kind);
  if (terminal) {
    for (const card of liveState.cards.values()) if (card.operation === operation) card.settled = event.seq;
  }
  if (event.kind === "compaction.started") $("phase").textContent = "Compacting context…";
}

// Tool settlement precedes batch history assembly. Only a rendered canonical
// turn/call result replaces a tool preview, never the session event cursor.
function reconcileLive(snapshot) {
  const liveState = view.live;
  if (!liveState) return;
  for (const [key, card] of liveState.cards) {
    if (card.toolResult ? view.renderedTools.has(card.toolResult) : card.settled && card.settled <= snapshot.last_event_seq) {
      card.box.remove();
      liveState.cards.delete(key);
    }
  }
}
