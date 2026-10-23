"use strict";

const $ = (id) => document.getElementById(id);
const csrf = document.querySelector('meta[name="csrf-token"]').content;
const builtins = ["read", "write", "edit", "bash", "history", "subagents"];
let state = null, polling = false, workspaceInitialized = false, choices = [], listSignature = "";
let initialSession = "";
try { initialSession = sessionStorage.getItem("sam-g-session") || ""; } catch { /* optional */ }
function selectedView(id, revision = 0) {
  return { id, revision, session: null, settings: null, context: null, steering: [], draft: null,
    pending: new Set(), historyCursor: 0, toolNames: new Map(), renderedTools: new Set(),
    events: null, live: null };
}
let view = selectedView(initialSession);
const pagePending = new Set();
const sectionMessages = new Map(); // disposable command results, never a durable error history
const sessionSections = ["chat", "settings", "toolbar-settings", "maintenance", "inspection"];
function sectionOwner(section) {
  if (sessionSections.includes(section)) return view.id;
  if (["account", "catalog"].includes(section)) return $("account").value;
  if (section === "model") return $("configured-model").value;
  if (section === "login") return `${$("login-provider").value}/${$("account-name").value.trim()}`;
  return "";
}
function messageOwner(section) {
  const owner = { section, id: sectionOwner(section), text: "", request: true };
  sectionMessages.set(`${section}/${owner.id}`, owner);
  return owner;
}
function sectionMessage(owner, message = "") {
  const key = `${owner.section}/${owner.id}`;
  if (owner.request && sectionMessages.get(key) !== owner) return;
  owner.text = message;
  sectionMessages.set(key, owner);
  renderMessages();
}
function renderMessages() {
  for (const section of ["connection", "account", "login", "catalog", "recovery", "model", "sessions", ...sessionSections]) {
    const node = $(`${section}-message`);
    const text = sectionMessages.get(`${section}/${sectionOwner(section)}`)?.text || "";
    node.textContent = text; node.hidden = !text;
  }
}
async function api(path, body, method = body === undefined ? "GET" : "POST") {
  const options = { method, headers: {}, cache: "no-store" };
  if (path.startsWith("/example/")) options.headers["X-Example-CSRF"] = csrf;
  if (method !== "GET") options.headers["Idempotency-Key"] = crypto.randomUUID();
  if (body !== undefined) { options.headers["Content-Type"] = "application/json"; options.body = JSON.stringify(body); }
  const response = await fetch(path, options), text = await response.text();
  let data;
  try { data = text ? JSON.parse(text) : null; } catch { throw new Error(text || `HTTP ${response.status}`); }
  if (!response.ok) {
    const error = data?.error || {}, provider = error.provider_failure;
    let message = error.message || `HTTP ${response.status}`;
    if (error.code) message += `\nCode: ${error.code}`;
    if (provider) message += `\nProvider: ${provider.code || "unknown"}${provider.http_status ? ` / HTTP ${provider.http_status}` : ""}${provider.request_id ? ` / request ${provider.request_id}` : ""}`;
    throw new Error(message);
  }
  return data;
}
function sessionBusy() { return [...view.pending].some((name) => name !== "stop"); }
function controls() {
  const allowed = accountUsable(conversationAccount()), selected = view.session;
  const active = selected && selected.phase !== "idle", busy = sessionBusy();
  accountControls();
  $("model").disabled = pagePending.has("catalog") || choices.length === 0;
  $("new").disabled = !accountUsable(selectedAccount()) || pagePending.has("new") || !$("model").value;
  $("prompt").disabled = !allowed || busy || active || !selected;
  $("send").disabled = !allowed || busy || active || !selected;
  $("stop").disabled = !active || view.pending.has("stop");
  $("session-tools").disabled = !selected || busy;
  $("apply-tools").disabled = !selected || busy || selected.phase !== "idle" || !view.draft;
  $("session-info").textContent = selected
    ? `${selected.name}\n${view.settings.effective.model} · account: ${view.settings.effective.account}\n${selected.workspace}` : "Create or select a session.";
  advancedControls();
  const names = selected?.active_tools.map((tool) => tool.name).join(", ");
  $("phase").textContent = !selected ? "No session selected." : selected.phase === "suspended"
    ? "Suspended after server restart. Resume ALL with acknowledgement, or Stop this session to end its saved operation."
    : active ? `${selected.phase}${names ? ` · ${names}` : ""}${selected.queued_tools ? ` · ${selected.queued_tools} queued` : ""} — saved results will appear below.`
    : selected.paused ? "Paused after a stop or failure. Review the conversation, then Continue or send a new prompt when ready." : "Ready. Other sessions can keep working while you are here.";
  $("tools-note").textContent = active ? "Changing tools requires this session to be idle, including no suspended work. Stop or wait for it first."
    : "Enabling tools allows automatic file edits and shell commands as your OS user, outside this folder too. No per-command approval dialog or sandbox.";
  renderMessages();
}
function modelOptions() {
  const selected = $("model").value;
  $("model").replaceChildren(new Option(choices.length ? "Choose a model" : "Load models after signing in", ""));
  for (const model of choices) {
    const messages = model.api === "anthropic_messages";
    const unsupported = model.supports_responses === false && !messages;
    const missingOutput = messages && model.max_output_tokens == null;
    const status = unsupported ? " · unsupported API" : missingOutput ? " · output limit needs configuration" : ["unavailable", "requires_action"].includes(model.availability) ? ` · ${model.availability}` : ` · ${model.api || "Responses (manual verification)"}`;
    const option = new Option(`${model.display_name || model.id} (${model.id})${status}`, model.id);
    option.disabled = model.availability === "unavailable" || model.availability === "requires_action" || unsupported || missingOutput;
    $("model").add(option);
  }
  if ([...$("model").options].some((o) => o.value === selected && !o.disabled)) $("model").value = selected;
  renderModelMetadata();
  controls();
}
function renderSessions() {
  const sessions = state?.sessions || [];
  const signature = JSON.stringify([view.id, sessions.map((s) => [s.id, s.name, s.phase, s.paused])]);
  if (signature === listSignature) return;
  listSignature = signature; $("sessions").replaceChildren();
  if (!sessions.length) { const p = document.createElement("p"); p.className = "hint"; p.textContent = "No sessions yet. Create one below."; $("sessions").append(p); }
  for (const s of [...sessions].reverse()) {
    const button = document.createElement("button"), phase = document.createElement("span");
    button.className = "session-choice"; button.type = "button";
    button.setAttribute("aria-current", String(s.id === view.id));
    button.append(document.createTextNode(s.name));
    phase.textContent = `${s.phase}${s.paused ? " · paused — review conversation" : ""}`; button.append(phase);
    button.onclick = () => selectSession(s.id); $("sessions").append(button);
  }
}
function selectSession(id) {
  clearLive();
  view = selectedView(id, view.revision + 1);
  try { sessionStorage.setItem("sam-g-session", id); } catch { /* optional */ }
  $("messages").replaceChildren(); $("turn-failures").replaceChildren();
  $("prompt").value = ""; $("steering-text").value = "";
  $("session-tools").checked = false;
  resetInspector(); renderSessions(); controls();
  refresh().catch(connectionFailure);
}
function appendHistory(item) {
  const tool = item.type === "function_call" || item.type === "function_call_output";
  if (!tool && item.type !== "message" && item.kind !== "note" && item.type !== "reasoning") return;
  if (item.type === "reasoning" && !item.text?.trim()) return; // opaque reasoning without a provider summary
  const box = document.createElement(tool ? "details" : "article");
  box.className = `message ${tool ? "tool" : item.role === "user" ? "user" : "assistant"}`;
  const label = document.createElement(tool ? "summary" : "strong"), text = document.createElement(tool ? "pre" : "p");
  const key = `${item.turn_id}/${item.call_id}`;
  if (item.type === "function_call") {
    view.toolNames.set(key, item.name); label.textContent = `Tool request · ${item.name}`; text.textContent = item.arguments;
  } else if (tool) {
    label.textContent = `Tool result · ${view.toolNames.get(key) || item.call_id} · ${item.status || "saved"}`;
    text.textContent = item.text;
    if (item.status && item.status !== "completed") box.classList.add("failed");
    view.renderedTools.add(key);
  } else {
    label.textContent = item.type === "reasoning" ? "Reasoning summary" : item.role === "user" ? "You" : item.role === "assistant" ? "Assistant" : "Notice";
    text.textContent = item.text;
  }
  box.append(label, text); $("messages").append(box); // all content stays inert text
}
async function listSessions() {
  const sessions = []; let after = "";
  try {
    do { const page = await api(`/v1/sessions?after=${encodeURIComponent(after)}`); sessions.push(...page.sessions); after = page.next || ""; } while (after);
    return sessions;
  } catch (error) {
    if (!state || error instanceof TypeError) throw error;
    sectionMessage({ section: "sessions", id: "" }, error.message);
    return state.sessions;
  }
}
async function readLocal(path, section, fallback, id = sectionOwner(section)) {
  try { return await api(path); }
  catch (error) {
    if (!state || error instanceof TypeError) throw error; // bootstrap / disconnected host
    sectionMessage({ section, id }, `Reading saved state: ${error.message}`);
    return fallback;
  }
}
async function refreshSelected(owner) {
  const base = `/v1/sessions/${encodeURIComponent(owner.id)}`, previousSettings = owner.settings;
  try {
    const [selected, settings, context, steering] = await Promise.all([api(base),
      readLocal(`${base}/config`, "settings", owner.settings, owner.id),
      readLocal(`${base}/context`, "inspection", owner.context, owner.id),
      readLocal(`${base}/steering`, "chat", { steering: owner.steering }, owner.id)]);
    if (owner !== view || !settings) return;
    Object.assign(owner, { session: selected, context, steering: steering.steering });
    // A config mutation may have committed while this older read was pending.
    if (owner.settings === previousSettings) owner.settings = settings;
    if (!owner.events) watchEvents(owner);
    renderSessionAccounts(); renderConfiguration();
    let page;
    do {
      page = await api(`${base}/history?after=${owner.historyCursor}`);
      if (owner !== view) return;
      const nearBottom = $("messages").scrollHeight - $("messages").scrollTop - $("messages").clientHeight < 80;
      for (const item of page.items) { if (item.seq > owner.historyCursor) appendHistory(item); owner.historyCursor = Math.max(owner.historyCursor, item.seq); }
      if (nearBottom) $("messages").scrollTop = $("messages").scrollHeight;
    } while (page.next);
    reconcileLive(selected);
  } catch (error) { sectionMessage({ section: "chat", id: owner.id }, `Reading session: ${error.message}`); }
}
async function refresh() {
  if (polling) return;
  polling = true;
  try {
    const [host, accounts, sessions, logins, models] = await Promise.all([api("/example/state"),
      readLocal("/v1/auth/accounts", "account", { accounts: state?.accounts }), listSessions(),
      readLocal("/v1/auth/logins", "login", { logins: state?.logins }),
      readLocal("/v1/models", "model", { models: state?.models })]);
    state = { host, accounts: accounts.accounts, logins: logins.logins, models: models.models, sessions };
    sectionMessage({ section: "connection", id: "" }); renderAccounts();
    if (!workspaceInitialized) { if (!$("workspace").value) $("workspace").value = host.default_workspace; workspaceInitialized = true; }
    if (view.id && !sessions.some((s) => s.id === view.id)) selectSession(sessions.at(-1)?.id || "");
    else if (!view.id && sessions.length) selectSession(sessions.at(-1).id);
    renderSessions();
    if (view.id) await refreshSelected(view);
    renderConfiguration(); controls();
  } finally { polling = false; }
}
function actionSection(name) {
  if (["login"].includes(name)) return "login";
  if (["logout", "retry-usage"].includes(name)) return "account";
  if (["new", "register-model"].includes(name)) return "catalog";
  if (name === "resume") return "recovery";
  if (name === "save-model") return "model";
  if (["apply-tools", "apply-account"].includes(name)) return "toolbar-settings";
  if (["save-settings", "inherit-settings"].includes(name)) return "settings";
  if (["rename", "delete-session", "export-history"].includes(name)) return "maintenance";
  return "chat";
}
async function perform(name, owner) {
  const id = owner.id, base = `/v1/sessions/${encodeURIComponent(id)}`;
  if (name === "login") return startLogin();
  if (name === "resume") return api("/example/resume", { request_id: crypto.randomUUID(), acknowledge_repeated_effects: $("resume-ack").checked });
  if (name === "save-model") return saveModelDefinition();
  if (name === "new" || name === "register-model") {
    const account = selectedAccount();
    const config = { account: account.name, disabled_tools: $("new-tools").checked ? [] : builtins };
    const workspace = $("workspace").value.trim(), title = $("name").value.trim() || "New session";
    const provider = authModes[account.provider], selected = choices.find((m) => m.id === $("model").value);
    const native = selected?.api === "anthropic_messages";
    const definition = native ? { provider, model_id: selected.id, api: selected.api,
      max_output_tokens: selected.max_output_tokens, context_tokens: selected.context_tokens || 0,
      reasoning_efforts: selected.reasoning_efforts || [], reasoning_summaries: true } : undefined;
    const result = await api("/example/model", { request_id: crypto.randomUUID(), provider, account: account.name,
      model: native ? `${provider}/${selected.id}` : $("model").value, model_definition: definition });
    if (name === "register-model") return result;
    return api("/v1/sessions", { name: title, workspace, config: { ...config, model: result.model } });
  }
  if (name === "apply-account") {
    if (owner.draft) throw new Error("Save or discard settings edits before an account handoff.");
    if (!confirm("Use the latest model definition and this account for future work? History stays intact; older tool/reasoning context becomes portable text and reasoning options reset.")) return null;
    return api(`${base}/model`, { model: owner.settings.effective.model, account: owner.accountChoice ?? owner.settings.effective.account });
  }
  if (["save-settings", "inherit-settings", "apply-tools"].includes(name)) return saveSessionSettings(name === "inherit-settings", owner);
  if (name === "export-history") return exportHistory(id);
  if (name === "rename") { const title = window.prompt("Session name", owner.session.name); return title === null ? null : api(base, { name: title }, "PATCH"); }
  if (name === "delete-session") {
    if (!window.confirm("Delete this session and its history? Owned background processes will stop; workspace/log files are not deleted.")) return null;
    await api(base, undefined, "DELETE"); if (owner === view) selectSession(""); return null;
  }
  if (name === "steer") return api(`${base}/steering`, { text: $("steering-text").value });
  if (name === "clear-steering") return api(`${base}/steering`, undefined, "DELETE");
  if (name === "continue") return api(`${base}/continue`, { prompt: $("prompt").value });
  if (name === "compact") return api(`${base}/compact`, {});
  if (name === "logout") {
    const account = $("account").value;
    if (!confirm(`Sign out "${account}"? Running work using this account may fail with an authentication error; other accounts are untouched.`)) return null;
    return api(`/v1/auth/accounts/${encodeURIComponent(account)}`, undefined, "DELETE");
  }
  if (name === "retry-usage") return api(`/v1/auth/accounts/${encodeURIComponent($("account").value)}/retry-plan-usage`, {});
  if (name === "stop") return api(`${base}/interrupt`, {});
  return api(`${base}/turns`, { prompt: $("prompt").value });
}
async function action(name) {
  const owner = view, message = messageOwner(actionSection(name)), modelEdit = modelRevision;
  const pending = sessionSections.includes(message.section) ? owner.pending : pagePending;
  if (pending.has(name)) return;
  pending.add(name); controls(); sectionMessage(message);
  try {
    const result = await perform(name, owner);
    if (name === "login" && result?.authorization_url) { window.location.assign(result.authorization_url); return; }
    if (name === "new" && owner === view && message.id === sectionOwner("catalog")) { selectSession(result.id); $("name").value = ""; $("new-tools").checked = false; }
    if (owner === view) {
      if (name === "send" || name === "continue") $("prompt").value = "";
      if (name === "steer") $("steering-text").value = "";
      if (result && ["settings", "toolbar-settings"].includes(message.section)) { owner.draft = null; owner.settings = result; owner.renderedConfig = ""; }
      if (name === "apply-account" && result) owner.accountChoice = undefined;
    }
    if (name === "save-model" && modelEdit === modelRevision && message.id === sectionOwner("model")) { modelDirty = false; modelSignature = ""; }
    if (name === "logout" && result && message.id === sectionOwner("account")) clearCatalog();
    if (result?.warning) sectionMessage(message, result.warning);
    await refresh();
  } catch (error) { sectionMessage(message, error.message); } // never automatically resend accepted work
  finally { if (name === "resume") $("resume-ack").checked = false; pending.delete(name); controls(); }
}
$("messages").replaceChildren();
initializeSettings(); initializeInspector();
for (const name of ["logout", "stop", "retry-usage", "apply-tools", "apply-account", "resume", "register-model", "continue", "compact", "clear-steering", "inherit-settings", "rename", "delete-session", "export-history"]) $(name).onclick = () => action(name);
$("login-form").onsubmit = (event) => { event.preventDefault(); if (!$("login").disabled) action("login"); };
$("account-name").oninput = controls;
$("login-provider").onchange = () => {
  if ($("account-name").value === "personal" && $("login-provider").value === "github-copilot") $("account-name").value = "copilot";
  else if ($("account-name").value === "copilot" && $("login-provider").value === "openai-chatgpt") $("account-name").value = "personal";
  controls();
};
$("account").onchange = () => { const account = selectedAccount(); if (account) { $("login-provider").value = account.provider; $("account-name").value = account.name; } clearCatalog(); };
$("session-account").onchange = () => { view.accountChoice = $("session-account").value; controls(); };
$("model").onchange = () => { renderModelMetadata(); controls(); };
$("session-tools").onchange = () => editOverrides({ disabled_tools: $("session-tools").checked ? [] : builtins });
$("steering-form").onsubmit = (event) => { event.preventDefault(); if (!$("steer").disabled && $("steering-text").value.trim()) action("steer"); };
$("new-session").onsubmit = (event) => { event.preventDefault(); if (!$("new").disabled) action("new"); };
$("composer").onsubmit = (event) => { event.preventDefault(); if (!$("send").disabled && $("prompt").value.trim()) action("send"); };
$("models").onclick = async () => {
  const account = selectedAccount(), selectedCatalog = catalogRevision, owner = messageOwner("catalog");
  pagePending.add("catalog"); controls(); sectionMessage(owner);
  try {
    const result = await api(`/v1/providers/${authModes[account.provider]}/models?account=${encodeURIComponent(account.name)}`);
    if (selectedCatalog !== catalogRevision || account.name !== selectedAccount()?.name) return;
    choices = result.models || []; modelOptions();
    if (!choices.length) sectionMessage(owner, "No selectable models were returned for this account.");
  } catch (error) { sectionMessage(owner, error.message); }
  finally { pagePending.delete("catalog"); controls(); }
};
function connectionFailure(error) { sectionMessage({ section: "connection", id: "" }, `${error.message}\nIf you restarted the server, reload this page.`); }
async function poll() { try { await refresh(); } catch (error) { connectionFailure(error); } setTimeout(poll, 1000); }
poll();
