"use strict";

const authModes = { "openai-chatgpt": "chatgpt", "github-copilot": "copilot" };
const authLabels = { "openai-chatgpt": "ChatGPT", "github-copilot": "GitHub Copilot" };
let accountSignature = "", loginSignature = "";
let catalogRevision = 0;
let lastLogin = "";
try { lastLogin = sessionStorage.getItem("sam-g-login") || ""; } catch { /* optional */ }

function accountByName(name) { return state?.accounts.find((a) => a.name === name); }
function selectedAccount() { return accountByName($("account").value); }
function conversationAccount() { return accountByName(view.settings?.effective.account); }
function providerEnabled(account) {
  return account && (account.provider === "openai-chatgpt" || (account.provider === "github-copilot" && state?.host.copilot_enabled));
}
function accountUsable(account) {
  return providerEnabled(account) && account.status === "signed_in" && !account.plan_blocked &&
    (account.provider !== "openai-chatgpt" || account.plan_usage_enabled);
}
function conversationProvider() {
  const provider = view.session?.binding?.provider;
  if (provider) return provider === "copilot" ? "github-copilot" : "openai-chatgpt";
  return conversationAccount()?.provider || (view.settings?.effective.model.startsWith("copilot/") ? "github-copilot" : "openai-chatgpt");
}
function clearCatalog() {
  catalogRevision++;
  choices = [];
  modelOptions();
}

function renderModelMetadata() {
  const m = choices.find((model) => model.id === $("model").value);
  const node = $("model-metadata");
  node.hidden = !m;
  if (!m) { node.textContent = ""; return; }
  const ref = m.reference;
  const source = ref ? `reference: ${ref.source}` : "unknown";
  const context = m.context_tokens != null ? `${m.context_tokens.toLocaleString()} (provider)`
    : ref ? `${ref.context_tokens.toLocaleString()} (${source})` : "unknown";
  let reasoning = "unknown";
  if (m.reasoning_efforts != null) reasoning = `${m.reasoning_efforts.join(", ") || "none"} (provider)`;
  else if (m.supports_reasoning != null) reasoning = m.supports_reasoning ? "supported; efforts unreported (provider)" : "not supported (provider)";
  else if (ref) reasoning = `${ref.supports_reasoning ? "supported; efforts unreported" : "not supported"} (${source})`;
  const summaries = m.reasoning_summaries == null ? "unknown" : m.reasoning_summaries ? "yes (provider)" : "no (provider)";
  node.textContent = `API: ${m.api || "needs verification"}. Messages transport: ${m.supports_messages == null ? "unknown" : m.supports_messages ? "yes" : "no"}. Maximum output: ${m.max_output_tokens ?? "unknown"}. Context capacity: ${context}. Reasoning: ${reasoning}. Summaries: ${summaries}. These do not change your local budget or declarations. Pi references are advisory; its Codex route differs from ChatGPT token-sharing.`;
}

function renderAccounts() {
  const accounts = state?.accounts || [];
  const signature = JSON.stringify(accounts.map((a) => [a.name, a.provider, a.status]));
  if (signature !== accountSignature) {
    accountSignature = signature;
    const previous = $("account").value;
    $("account").replaceChildren(new Option("Choose a saved account", ""));
    for (const a of accounts) $("account").add(new Option(`${a.name} · ${authLabels[a.provider]} · ${a.status}`, a.name));
    $("account").value = accounts.some((a) => a.name === previous) ? previous : (accounts.find((a) => a.status === "signed_in")?.name || accounts[0]?.name || "");
    if (previous !== $("account").value) clearCatalog();
  }
  const enabled = Boolean(state?.host.copilot_enabled);
  $("login-provider").querySelector('[value="github-copilot"]').disabled = !enabled;
  $("copilot-note").textContent = enabled
    ? "Copilot uses the same public GitHub Copilot Plugin registration and compatibility headers as Pi by default. No client-ID setup is needed. Account/organization/model policies still apply; this host supports Responses and Copilot Opus 5.5 native Messages."
    : "Copilot is not configured in this host.";
  renderLogins();
}

function renderSessionAccounts() {
  const current = view.settings?.effective.account || "";
  const provider = conversationProvider();
  const candidates = (state?.accounts || []).filter((a) => a.provider === provider);
  const wanted = view.accountChoice ?? current;
  const signature = JSON.stringify([wanted, candidates.map((a) => [a.name, a.status])]);
  if (signature === view.renderedAccounts) return;
  view.renderedAccounts = signature;
  $("session-account").replaceChildren(new Option("Choose same-provider account", ""));
  for (const a of candidates) {
    const option = new Option(`${a.name} · ${a.status}`, a.name);
    option.disabled = a.status !== "signed_in" && a.name !== current;
    $("session-account").add(option);
  }
  if (current && !candidates.some((a) => a.name === current)) $("session-account").add(new Option(`${current} · not signed in`, current));
  $("session-account").value = [...$("session-account").options].some((o) => o.value === wanted) ? wanted : current;
}

function accountControls() {
  const busy = ["login", "logout", "retry-usage", "cancel-login"].some((name) => pagePending.has(name));
  const account = selectedAccount();
  const signed = account?.status === "signed_in";
  const copilot = $("login-provider").value === "github-copilot";
  $("account").disabled = busy || !state?.accounts.length;
  $("login-provider").disabled = busy;
  $("account-name").disabled = busy;
  $("login").disabled = !state || busy || (copilot && !state.host.copilot_enabled);
  $("login").textContent = copilot ? "Get GitHub device code" : "Sign in with ChatGPT";
  $("logout").disabled = !signed || busy;
  $("models").disabled = busy || pagePending.has("catalog") || !providerEnabled(account) || !signed || (account.provider === "openai-chatgpt" && !account.plan_usage_enabled);
  $("retry-usage").hidden = !account?.plan_blocked;
  $("retry-usage").disabled = busy;
  $("session-account").disabled = !view.session || sessionBusy();
  const protocolChanged = view.session?.binding && currentModel() && (view.session.binding.api || "responses") !== (currentModel().api || "responses");
  const changedAccount = view.accountChoice && view.accountChoice !== view.settings?.effective.account;
  $("apply-account").textContent = protocolChanged ? "Rebind model/account" : "Switch account";
  $("apply-account").disabled = !view.session || sessionBusy() || view.session.phase !== "idle" || Boolean(view.draft) || (!changedAccount && !protocolChanged) || !accountUsable(accountByName($("session-account").value));
  let label = account ? `${account.name} · ${authLabels[account.provider]} · ${account.status}.` : "Add or choose a saved account.";
  if (signed && account.provider === "openai-chatgpt" && !account.plan_usage_enabled) label += " Sign in again with this label to request plan consent.";
  if (account?.plan_blocked) label += ` Usage is blocked. Reset: ${account.plan_reset_at || "not reported"}.`;
  if (account && !providerEnabled(account)) label += " This provider is not enabled in this host.";
  $("account-status").textContent = label;
  for (const button of $("logins").querySelectorAll("button")) button.disabled = busy;
}

function renderLogins() {
  const visible = (state?.logins || []).filter((login) => ["starting", "pending"].includes(login.state) || login.id === lastLogin);
  const signature = JSON.stringify(visible);
  if (signature === loginSignature) return;
  loginSignature = signature;
  $("logins").replaceChildren();
  for (const login of visible) {
    const box = document.createElement("div");
    box.className = "login-card";
    const title = document.createElement("strong");
    title.textContent = `${login.account} · ${authLabels[login.provider]} · ${login.state}`;
    box.append(title);
    if (["starting", "pending"].includes(login.state)) {
      const link = document.createElement("a");
      link.href = login.flow === "device_code" ? login.verification_url : login.authorization_url;
      link.rel = "noreferrer noopener";
      if (login.flow === "device_code") {
        link.target = "_blank";
        link.textContent = "Open GitHub and enter this code ↗";
        const code = document.createElement("code");
        code.className = "device-code";
        code.textContent = login.user_code;
        const expiry = document.createElement("p");
        expiry.className = "hint";
        expiry.textContent = `Approve in the intended GitHub account. This page checks local status automatically. Expires ${new Date(login.expires_at).toLocaleTimeString()}.`;
        box.append(code, expiry);
      } else link.textContent = "Continue ChatGPT sign-in";
      if (login.verification_url || login.authorization_url) box.append(link);
      const cancel = document.createElement("button");
      cancel.type = "button";
      cancel.textContent = "Cancel this sign-in";
      cancel.onclick = () => cancelLogin(login.id);
      box.append(cancel);
    } else {
      const status = document.createElement("p");
      status.textContent = login.error || (login.state === "completed" ? "Account saved. Choose it above to load models or create a session." : "Start a new sign-in if needed.");
      box.append(status);
    }
    $("logins").append(box);
  }
}

async function startLogin() {
  const account = $("account-name").value.trim();
  const provider = $("login-provider").value;
  if (accountByName(account)?.status === "signed_in" && !confirm(`Sign in again as "${account}"? Completing sign-in replaces this account's saved login. Running work using it may fail with an authentication error; other accounts are untouched.`)) return null;
  let login;
  if (provider === "github-copilot") login = await api("/v1/auth/logins", { provider, account });
  else login = await api("/example/login", { request_id: crypto.randomUUID(), account });
  lastLogin = login.id;
  try { sessionStorage.setItem("sam-g-login", login.id); } catch { /* optional */ }
  clearCatalog();
  return login;
}

async function cancelLogin(id) {
  const account = state.logins.find((login) => login.id === id)?.account || id;
  const owner = messageOwner("login");
  pagePending.add("cancel-login"); controls();
  try { await api(`/v1/auth/logins/${encodeURIComponent(id)}`, undefined, "DELETE"); await refresh(); }
  catch (error) { sectionMessage(owner, `${account}: ${error.message}`); }
  finally { pagePending.delete("cancel-login"); controls(); }
}
