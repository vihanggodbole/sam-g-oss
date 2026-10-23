/* Local teaching model only: no fetch, sockets, storage, tools, or provider calls. */
(() => {
  "use strict";

  const root = document.documentElement;
  const byID = (id) => document.getElementById(id);
  const button = (action) => document.querySelector(`[data-action="${action}"]`);
  const initial = () => ({
    host: "remote", journey: "home", tool: "read", overflow: false,
    flow: "running", guidance: "none", compacted: false,
    history: "none", summary: "summary", compactedWith: "",
    preview: "off", previewPaused: false,
  });
  let state = initial();

  const tools = {
    read: ["read main.go · lines 1–20", "A small window into a UTF-8 text file. Read only the lines you need.", "read a few lines"],
    write: ["write notes/plan.md · “Next steps”", "Create or replace a text file. Missing parent folders are created too.", "make a new page"],
    edit: ["edit · replace “Next steps” with “Plan”", "The old text must match exactly once. Every replacement is checked before the file changes.", "a precise replacement"],
    bash: ["bash · go test ./...", "Run a command in the workspace. Its stdout and stderr share one output budget; the result includes exit status.", "try the tests"],
    history: ["history · search “callback”, then read #124", "Read visible history or diagnostic events in this session or an owned child—even after compaction.", "find the original words"],
    subagents: ["subagents · start a backend review", "An ordinary child session on the same runtime. Inspect, steer or cancel it; completion enters the parent's steering queue. Shared edits are not coordinated.", "delegate a task"],
  };

  function say(id, message, category = "output") {
    const element = byID(id);
    element.textContent = message;
    element.classList.toggle("is-error", category === "error");
    element.classList.toggle("is-diagnostic", category === "diagnostic");
  }

  function render() {
    for (const key of ["host", "journey", "tool", "overflow", "flow", "guidance", "compacted", "history", "preview"]) {
      root.dataset[key] = state[key];
    }
    document.querySelectorAll("[data-tool]").forEach((item) => {
      item.setAttribute("aria-pressed", String(item.dataset.tool === state.tool));
    });
    for (const host of ["remote", "embedded"]) {
      button(host).setAttribute("aria-pressed", String(state.host === host));
    }
    byID("host-label").textContent = state.host === "remote" ? "optional remote host" : "library inside your app";
    button("send").disabled = state.journey !== "home";
    button("sleep").disabled = state.journey !== "sent" || state.host !== "remote";
    button("replay").textContent = state.journey === "sleep" ? "Return & replay missed events ↶" : "Try a sleep & replay ↶";
    button("replay").disabled = state.journey === "replayed";
    button("steer").disabled = state.guidance === "pending";
    button("boundary").disabled = state.guidance !== "pending" || state.flow === "paused";
    button("interrupt").disabled = state.flow === "paused";
    button("continue").disabled = state.flow !== "paused";
    byID("flow-art-label").textContent = state.flow === "paused" ? "A pauses. B carries on." : "two conversations, their own pace";
    button("compact").disabled = state.compacted;
    button("history").textContent = state.history === "found" ? "Read original context #124" : "Find an old detail";
    byID("summary-art-label").textContent = state.compacted ? `packed by “${state.compactedWith}”` : "working context";
    const running = state.preview === "launched" || state.preview === "ready";
    button("preview").disabled = running;
    button("preview").textContent = state.preview === "stopped" ? "Start a new pretend session" : "Launch a pretend preview";
    button("ready").disabled = state.preview !== "launched";
    button("preview-interrupt").disabled = !running || state.previewPaused;
    button("delete").disabled = !running || !state.previewPaused;
    byID("preview-art-label").textContent = {
      off: "a place for the night shift", launched: "launched ≠ ready",
      ready: "checked separately: ready", stopped: "session gone; log file stays",
    }[state.preview];
    byID("cursor-label").textContent = state.journey === "replayed" ? "cursor: 45" : "cursor: 42";
  }

  function chooseHost(host) {
    state.host = host;
    say("departure-response", host === "embedded"
      ? "Here sam runs in your own Go process. No server or listener is opened. That host process must stay alive for work to continue."
      : "The remote host owns the work. Your laptop is just a visitor; it can stop watching without cancelling the task.");
  }

  const actions = {
    remote: () => chooseHost("remote"),
    embedded: () => chooseHost("embedded"),
    send() {
      state.journey = "sent";
      say("departure-response", "Task accepted and saved. The host can now ask the configured model, run its chosen tools, and save results. The envelope is our pretend request.");
    },
    sleep() {
      state.journey = "sleep";
      say("departure-response", "Laptop asleep. Remote work continues and events are saved. Walk to the return dock to see what you missed.", "diagnostic");
      say("replay-response", "While you were away: sample events 43, 44, and 45 were saved on the remote machine. Your last seen number is still 42.");
    },
    overflow() {
      state.overflow = !state.overflow;
      button("overflow").textContent = state.overflow ? "Back to a smaller result ↶" : "What if the result is enormous? ↗";
      say("overflow-response", state.overflow
        ? "tool_output_limit: ask for fewer lines or filter the command. Only this call fails; no overflow archive or automatic retry. Earlier effects may already have happened."
        : "Normal tool results have a default 256 KiB limit per call.", state.overflow ? "error" : "output");
    },
    steer() {
      state.guidance = "pending";
      say("flow-response", state.flow === "paused"
        ? "Guidance saved, but A is paused. It will wait for your explicit Continue; steering alone does not restart interrupted work."
        : "Guidance saved: “Also update the notes.” The current tool can finish. The note is waiting at the next model-request boundary.");
    },
    boundary() {
      state.guidance = "applied";
      say("flow-response", "The next model request now includes your note. Guidance was accepted first, then applied—not injected halfway through a tool.");
    },
    interrupt() {
      state.flow = "paused";
      say("flow-response", "A's current work is cancelled and waiting guidance pauses. The service and B keep going. This picture skips the short cleanup period; in real life, wait for A to become idle.", "diagnostic");
    },
    continue() {
      state.flow = "running";
      const pending = state.guidance === "pending";
      if (pending) state.guidance = "applied";
      say("flow-response", pending
        ? "Explicit continuation opens A's track and applies its waiting note in order. B was never interrupted."
        : "Explicit continuation opens A's track again. Completed changes were kept; interruption was never a rollback.");
    },
    compact() {
      state.compacted = true;
      state.compactedWith = state.summary;
      say("archive-response", `“${state.summary}” made a smaller working summary in this illustration. Recent context stays alongside it. Every original book is still on the shelf.`, "diagnostic");
    },
    history() {
      if (state.history === "found") {
        state.history = "read";
        say("archive-response", "Original entry #124, with its neighbours: “Use /auth/callback as the callback path.” The exact detail came from the retained conversation, not a guess.");
      } else {
        state.history = "found";
        say("archive-response", "Search “callback” → one sample reference: #124, “Use /auth/callback…” A page holds at most 10 matching entries, newest first. Now read the original context.");
      }
    },
    preview() {
      state.preview = "launched";
      state.previewPaused = false;
      say("preview-response", "Pretend launch: background=true, log_file=logs/dev.log. Output appends to that file, not the event stream. The launch acknowledgement does not prove readiness.", "diagnostic");
    },
    ready() {
      state.preview = "ready";
      say("preview-response", "Our separate pretend check succeeded. The preview is ready on the remote machine. In real use, inspect the log and check the service with ordinary commands.");
    },
    "preview-interrupt"() {
      state.previewPaused = true;
      say("preview-response", "Session work has settled to idle in this simulation. The background preview stays on: interrupt is not shutdown. You can now delete the idle session to stop owned background work.", "diagnostic");
    },
    delete() {
      state.preview = "stopped";
      say("preview-response", "The idle session was deleted in our illustration; its owned preview stopped. Workspace and log files remain. Clean runtime shutdown also stops owned servers.", "diagnostic");
    },
    replay() {
      state.host = "remote";
      state.journey = "replayed";
      say("departure-response", "Your laptop returned to the remote host. Reconnecting changes what you see, not who owns the task.");
      say("replay-response", "Simulated replay after 42: 43 tool output → 44 answer text → 45 turn completed. Now follow live events. Reconnects may repeat events; ignore numbers already handled.");
    },
    reset() {
      state = initial();
      byID("summary-model").value = state.summary;
      chooseTool("read");
      button("overflow").textContent = "What if the result is enormous? ↗";
      say("departure-response", "Our pretend task: “Fix the tests in my project.”");
      say("overflow-response", "Normal tool results have a default 256 KiB limit per call.");
      say("flow-response", "Both tracks are open. Try guiding or stopping just A.");
      say("archive-response", "This pretend session is idle, so manual compaction is available.");
      say("preview-response", "The greenhouse is quiet. A launch is not a readiness check.");
      say("replay-response", "World reset. An example cursor of 42 means: “Show me events after 42.”");
    },
  };

  function chooseTool(name) {
    state.tool = name;
    const [request, result, label] = tools[name];
    byID("tool-request").textContent = request;
    byID("tool-result").textContent = result;
    byID("tool-art-label").textContent = label;
  }

  document.querySelectorAll("[data-action]").forEach((element) => {
    element.addEventListener("click", () => {
      actions[element.dataset.action]();
      render();
    });
  });
  document.querySelectorAll("[data-tool]").forEach((element) => {
    element.addEventListener("click", () => { chooseTool(element.dataset.tool); render(); });
  });
  byID("summary-model").addEventListener("change", (event) => {
    state.summary = event.target.value;
    say("archive-response", `Illustration only: the next compaction will use “${state.summary}”. In a real runtime, change it through UpdateConfig when that session is fully idle; other sessions may keep working. The main model “coding” stays locked after the first accepted message, and a saved summary is not rewritten.`);
  });

  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
  let motionPaused = reducedMotion.matches;
  function updateMotion() {
    root.dataset.motion = motionPaused || reducedMotion.matches ? "paused" : "playing";
    byID("motion").textContent = reducedMotion.matches ? "Motion reduced" : motionPaused ? "Resume motion" : "Pause motion";
    byID("motion").setAttribute("aria-pressed", String(motionPaused || reducedMotion.matches));
    byID("motion").disabled = reducedMotion.matches;
  }
  byID("motion").addEventListener("click", () => { motionPaused = !motionPaused; updateMotion(); });
  reducedMotion.addEventListener("change", updateMotion);

  // Native scrolling does the travel; this only marks the nearest landmark.
  if ("IntersectionObserver" in window) {
    const links = [...document.querySelectorAll(".trail-map a")];
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        for (const link of links) {
          if (link.hash === `#${entry.target.id}`) link.setAttribute("aria-current", "location");
          else link.removeAttribute("aria-current");
        }
      }
    }, { rootMargin: "-20% 0px -65% 0px" });
    document.querySelectorAll(".stop").forEach((stop) => observer.observe(stop));
  }
  updateMotion();
  render();
})();
