"use strict";

let inspectCursor = "", inspectNext = "", inspectionRevision = 0;

function resetInspector() {
  inspectionRevision++;
  inspectCursor = inspectNext = "";
  $("inspection").textContent = "";
  $("inspect-caption").textContent = "";
  $("inspect-next").disabled = true;
}

async function inspect(next = false) {
  const owner = view, id = owner.id, operation = ++inspectionRevision;
  const message = messageOwner("inspection");
  sectionMessage(message);
  const kind = $("inspect-kind").value;
  inspectCursor = next ? inspectNext : "";
  let path;
  if (kind === "diagnostics") path = "/v1/diagnostics";
  else if (kind === "events") path = `/example/events?session_id=${encodeURIComponent(id)}&after=${inspectCursor || 0}`;
  else path = `/v1/sessions/${encodeURIComponent(id)}/${kind}${kind === "turns" ? `?after=${encodeURIComponent(inspectCursor)}` : ""}`;
  try {
    const data = await api(path);
    if (owner !== view || operation !== inspectionRevision) return;
    inspectNext = kind === "events" && data.events?.length === 100 ? String(data.events.at(-1).seq) : kind === "turns" ? data.next || "" : "";
    $("inspection").textContent = JSON.stringify(data, null, 2);
    $("inspect-next").disabled = !inspectNext;
    $("inspect-caption").textContent = kind === "events" ? `Durable events after ${inspectCursor || 0}; up to 100 per page. Next checks for more.` : kind === "turns" ? "Turn records; use Next when another page is available." : "Read-only snapshot. No provider request was made.";
  } catch (error) { if (operation === inspectionRevision) sectionMessage(message, error.message); }
}

async function exportHistory(id) {
  const saved = await api(`/v1/sessions/${encodeURIComponent(id)}`);
  const items = [];
  let after = 0;
  do {
    const page = await api(`/v1/sessions/${encodeURIComponent(id)}/history?after=${after}`);
    items.push(...page.items);
    after = page.next;
  } while (after);
  const blob = new Blob([JSON.stringify({ session: saved, history: items }, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob), link = document.createElement("a");
  link.href = url; link.download = `${id}-history.json`; link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function initializeInspector() {
  $("inspect-refresh").onclick = () => inspect();
  $("inspect-next").onclick = () => inspect(true);
  $("inspect-kind").onchange = () => { resetInspector(); controls(); };
  $("inspector-panel").ontoggle = () => { if ($("inspector-panel").open && (view.id || $("inspect-kind").value === "diagnostics")) inspect(); };
}
