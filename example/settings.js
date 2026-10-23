"use strict";

let modelDirty = false, modelRevision = 0, modelListSignature = "", modelSignature = "";

function editOverrides(update) {
  view.draft = { ...(view.draft || view.settings.overrides), ...update };
  renderSessionSettings(); renderSessionAccounts(); controls();
}
function draftConfig() {
  const c = view.settings?.effective;
  return c && { ...c, ...view.draft, compaction: { ...c.compaction, ...view.draft?.compaction } };
}

function optionList(id, options, value) {
  $(id).replaceChildren(...options.map(([text, val]) => new Option(text, val)));
  $(id).value = value || "";
}

function initializeSettings() {
  for (const name of builtins) {
    const label = document.createElement("label");
    label.className = "check";
    const input = document.createElement("input");
    input.type = "checkbox"; input.id = `tool-${name}`;
    label.append(input, document.createTextNode(name));
    $("individual-tools").append(label);
  }
  $("settings-form").oninput = () => { view.draft = editedSettings(view.draft || view.settings.overrides); $("session-tools").checked = builtins.some((name) => !view.draft.disabled_tools.includes(name)); controls(); };
  $("settings-form").onsubmit = (event) => { event.preventDefault(); if (!$("save-settings").disabled) action("save-settings"); };
  $("model-form").oninput = () => { modelDirty = true; modelRevision++; controls(); };
  $("model-form").onsubmit = (event) => { event.preventDefault(); if (!$("save-model").disabled) action("save-model"); };
  $("configured-model").onchange = () => { modelRevision++; modelDirty = false; modelSignature = ""; fillModelDefinition(); controls(); };
  $("compaction-model").onchange = () => { fillCompactionAccounts(""); view.draft = editedSettings(view.draft || view.settings.overrides); controls(); };
  $("resume-ack").onchange = controls;
}

function currentModel() { return state?.models.find((m) => m.name === view.settings?.effective.model); }

function renderConfiguration() {
  const definitions = state?.models || [];
  const listSignature = JSON.stringify(definitions);
  if (listSignature !== modelListSignature) {
    modelListSignature = listSignature;
    const wanted = $("configured-model").value || view.settings?.effective.model || definitions[0]?.name || "";
    optionList("configured-model", definitions.map((m) => [`${m.name} · ${m.provider}`, m.name]), wanted);
    const summary = draftConfig()?.compaction.model;
    optionList("compaction-model", [["Use main model", ""], ...definitions.map((m) => [m.name, m.name])], summary);
    view.renderedConfig = "";
  }
  fillModelDefinition();
  if (!view.settings) return;
  $("settings-json").textContent = JSON.stringify(view.settings, null, 2);
  const signature = JSON.stringify(view.settings);
  if (view.draft || view.renderedConfig === signature) return;
  view.renderedConfig = signature;
  renderSessionSettings();
}

function renderSessionSettings() {
  const c = draftConfig(), summary = c.compaction;
  $("session-tools").checked = builtins.some((name) => !(c.disabled_tools || []).includes(name));
  $("instructions").value = c.append_instructions || "";
  $("instructions-file").value = c.append_instructions_file || "";
  $("tool-concurrency").value = c.tool_concurrency;
  $("tool-output-limit").value = c.tool_output_limit;
  for (const name of builtins) $(`tool-${name}`).checked = !(c.disabled_tools || []).includes(name);
  optionList("reasoning-effort", [["Provider default", ""], ...(currentModel()?.reasoning_efforts || []).map((x) => [x, x])], c.reasoning_effort);
  $("reasoning-summary").checked = c.reasoning_summary;
  $("auto-compact").checked = !summary.disable_auto;
  $("compaction-model").value = summary.model || "";
  fillCompactionAccounts(summary.account);
  $("output-reserve").value = summary.output_reserve;
  $("trigger-percent").value = summary.trigger_percent;
  $("recent-percent").value = summary.recent_percent;
  $("compaction-prompt").value = summary.prompt || "";
  $("compaction-file").value = summary.prompt_file || "";
}

function fillCompactionAccounts(value) {
  const model = state?.models.find((m) => m.name === $("compaction-model").value) || currentModel();
  const kind = model?.provider === "copilot" ? "github-copilot" : "openai-chatgpt";
  optionList("compaction-account", [["Inherit session account (same provider)", ""], ...(state?.accounts || []).filter((a) => a.provider === kind).map((a) => [`${a.name} · ${a.status}`, a.name])], value);
}

function fillModelDefinition() {
  const m = state?.models.find((model) => model.name === $("configured-model").value);
  const signature = JSON.stringify(m);
  if (!m || modelDirty || signature === modelSignature) return;
  modelSignature = signature;
  $("context-budget").value = m.context_tokens;
  $("model-api").value = m.api || "responses";
  $("max-output-tokens").value = m.max_output_tokens || 0;
  $("supported-efforts").value = (m.reasoning_efforts || []).join(", ");
  $("supports-summaries").checked = Boolean(m.reasoning_summaries);
}

function numberValue(id) { return Number($(id).value || 0); }
function editedSettings(overrides) {
  return {
    ...overrides,
    append_instructions: $("instructions").value,
    append_instructions_file: $("instructions-file").value.trim(),
    disabled_tools: builtins.filter((name) => !$(`tool-${name}`).checked),
    tool_concurrency: numberValue("tool-concurrency"), tool_output_limit: numberValue("tool-output-limit"),
    reasoning_effort: $("reasoning-effort").value, reasoning_summary: $("reasoning-summary").checked,
    compaction: { ...overrides.compaction, model: $("compaction-model").value, account: $("compaction-account").value,
      disable_auto: !$("auto-compact").checked, output_reserve: numberValue("output-reserve"),
      trigger_percent: numberValue("trigger-percent"), recent_percent: numberValue("recent-percent"),
      prompt: $("compaction-prompt").value, prompt_file: $("compaction-file").value.trim() },
  };
}

async function saveSessionSettings(reset, owner) {
  const current = owner.settings;
  const update = reset ? { model: current.effective.model, account: current.effective.account } : owner.draft;
  return api(`/v1/sessions/${encodeURIComponent(owner.id)}/config`, update, "PUT");
}

async function saveModelDefinition() {
  const m = state.models.find((model) => model.name === $("configured-model").value);
  const definition = { ...m, api: $("model-api").value, max_output_tokens: numberValue("max-output-tokens"), context_tokens: numberValue("context-budget"),
    reasoning_efforts: $("supported-efforts").value.split(",").map((x) => x.trim()).filter(Boolean),
    reasoning_summaries: $("supports-summaries").checked };
  delete definition.name;
  return api("/example/model", { request_id: crypto.randomUUID(), model: m.name, model_definition: definition });
}

function advancedControls() {
  const selected = view.session;
  const busy = sessionBusy();
  const idle = selected?.phase === "idle";
  const allowed = accountUsable(conversationAccount());
  const suspended = (state?.sessions || []).filter((s) => s.phase === "suspended");
  $("recovery").hidden = suspended.length === 0;
  $("resume-warning").textContent = `${suspended.length} suspended operation(s). ${state?.host.resume_warning || "Unknown tool/hook effects may repeat. Saved outcomes are not rerun."}`;
  $("resume").disabled = pagePending.has("resume") || !suspended.length || !$("resume-ack").checked;
  $("register-model").disabled = pagePending.has("register-model") || !selectedAccount() || !$("model").value;
  $("continue").disabled = busy || !idle || !allowed;
  $("compact").disabled = busy || !idle;
  $("steer").disabled = busy || !selected || !allowed;
  $("clear-steering").disabled = busy || !selected || !view.steering.length;
  $("rename").disabled = busy || !selected;
  $("delete-session").disabled = busy || !idle;
  $("export-history").disabled = busy || !selected;
  $("settings-fields").disabled = busy || !selected;
  $("reasoning-summary").disabled = busy || !selected || !currentModel()?.reasoning_summaries;
  $("save-settings").disabled = busy || !idle || !selected || !view.draft;
  $("inherit-settings").disabled = busy || !idle || !selected;
  $("save-model").disabled = pagePending.has("save-model") || !$("configured-model").value || !modelDirty;
  $("inspect-refresh").disabled = busy || (!selected && $("inspect-kind").value !== "diagnostics");
  const info = view.context;
  const tokens = (usage) => usage ? `input ${usage.input_tokens ?? "unknown"}, output ${usage.output_tokens ?? "unknown"}, reasoning ${usage.reasoning_tokens ?? "unknown"}` : "unknown";
  $("usage").textContent = !selected ? "" : `Session model usage: ${tokens(selected.usage)}. Compaction: ${tokens(selected.compaction_usage)}.\n` +
    (info?.model_error || (info ? `Estimated context ${info.estimated_tokens} / ${info.input_budget} input tokens; reserve ${info.output_reserve}; auto compaction ${info.auto_compaction ? "on" : "off"}.` : "Context estimate unavailable."));
  $("steering-list").replaceChildren(...view.steering.map((item) => {
    const li = document.createElement("li");
    li.textContent = `${item.paused ? "Paused" : "Queued"}: ${item.text}`;
    return li;
  }));
}
