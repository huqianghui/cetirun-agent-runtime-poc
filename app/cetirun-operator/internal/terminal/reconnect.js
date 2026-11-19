// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

(() => {
  const agentUID = document.currentScript.dataset.agentUid;
  const originalWebSocket = window.WebSocket;
  const base = new URL(".", window.location.href);
  let timer;
  let retryGeneration = 0;
  let attempts = 0;
  let stopped = false;
  let connected = false;
  let currentSocket;
  let notice;

  function show(message) {
    if (!notice) {
      notice = document.createElement("div");
      notice.setAttribute("role", "status");
      Object.assign(notice.style, {
        position: "fixed", top: "0", left: "0", right: "0", zIndex: "10000",
        padding: "12px", background: "#202124", color: "#ffffff", fontFamily: "sans-serif",
      });
      (document.body || document.documentElement).appendChild(notice);
    }
    notice.textContent = message;
  }

  function schedule() {
    if (stopped || timer || connected) return;
    show("Terminal connection lost. Waiting to reconnect…");
    timer = window.setTimeout(check, Math.min(1000 * 2 ** attempts++, 10000));
  }

  function cancelRetry() {
    retryGeneration++;
    window.clearTimeout(timer);
    timer = undefined;
  }

  async function check() {
    timer = undefined;
    const generation = retryGeneration;
    const isCurrent = () => !stopped && !connected && generation === retryGeneration;
    if (!isCurrent()) return;
    try {
      const response = await fetch(new URL("readyz", base), {
        cache: "no-store", redirect: "manual", signal: AbortSignal.timeout(5000),
      });
      if (!isCurrent()) return;
      if (response.status === 401 || response.status === 403 || response.type === "opaqueredirect") {
        stopped = true;
        show("Terminal access ended. Return to the Portal and sign in again.");
        return;
      }
      if (response.ok) {
        const state = await response.json();
        if (!isCurrent()) return;
        if (state.agentUID !== agentUID) {
          stopped = true;
          show("This Agent was replaced. Reopen its terminal from the Portal.");
          return;
        }
        if (state.ready) {
          window.location.reload();
          return;
        }
      }
    } catch {
      // A restarting terminal Pod or temporary network failure is retryable.
    }
    if (isCurrent()) schedule();
  }

  window.WebSocket = class extends originalWebSocket {
    constructor(...args) {
      super(...args);
      cancelRetry();
      currentSocket = this;
      connected = false;
      this.addEventListener("open", () => {
        if (currentSocket !== this) return;
        connected = true;
        attempts = 0;
        cancelRetry();
        notice?.remove();
        notice = undefined;
      });
      this.addEventListener("error", () => {
        if (currentSocket !== this) return;
        connected = false;
        schedule();
      });
      this.addEventListener("close", (event) => {
        if (currentSocket !== this) return;
        connected = false;
        if (event.code === 1000 || event.code === 1008) {
          // User exit, denial or session expiry requires a fresh explicit connection.
          cancelRetry();
          return;
        }
        schedule();
      });
    }
  };
})();
