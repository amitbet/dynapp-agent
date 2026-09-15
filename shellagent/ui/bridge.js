import {
  mountRemoteEnvironments,
  mountWorkspaceManagement,
} from "./clientSettingsPanels.js";

const csrfToken = document.querySelector('meta[name="dynapp-settings-csrf"]')?.content ?? "";

async function api(path, options = {}) {
  const method = options.method ?? "GET";
  const headers = { "X-Dynapp-Settings-Csrf": csrfToken };
  if (method !== "GET" && method !== "HEAD") headers["content-type"] = "application/json";
  const response = await fetch(path, {
    cache: "no-store",
    method,
    headers,
    body: method === "GET" || method === "HEAD" ? undefined : JSON.stringify(options.body ?? {}),
  });
  let payload = null;
  try { payload = await response.json(); } catch { /* HTTP status is sufficient */ }
  if (!response.ok) {
    const message = payload?.error || (typeof payload === "string" ? payload : null) || await response.text().catch(() => "");
    throw new Error(message || `Request failed: HTTP ${response.status}`);
  }
  return payload;
}

window.appShell = {
  getInfo: async () => ({ platform: "desktop", runtime: "shell-agent" }),
};

window.dynappWorkspaceManagement = {
  list: async () => {
    const payload = await api("/api/v1/workspaces");
    return Array.isArray(payload?.workspaces) ? payload.workspaces : [];
  },
  details: (workspaceId) => api(`/api/v1/workspaces/${encodeURIComponent(String(workspaceId))}/members`),
  invite: (workspaceId, options = {}) => api(
    `/api/v1/workspaces/${encodeURIComponent(String(workspaceId))}/invitations`,
    {
      method: "POST",
      body: {
        email: options.email ?? null,
        userId: options.userId ?? null,
        role: options.role ?? "editor",
        expiresInDays: options.expiresInDays ?? 7,
      },
    },
  ),
  updateRole: (workspaceId, userId, role) => api(
    `/api/v1/workspaces/${encodeURIComponent(String(workspaceId))}/members/${encodeURIComponent(String(userId))}`,
    { method: "PATCH", body: { role } },
  ),
  removeMember: (workspaceId, userId) => api(
    `/api/v1/workspaces/${encodeURIComponent(String(workspaceId))}/members/${encodeURIComponent(String(userId))}`,
    { method: "DELETE" },
  ),
};

window.dynappRemoteEnvironmentManagement = {
  list: () => api("/api/settings/remote-environments"),
  setServerEnabled: (enabled, options = {}) => api("/api/settings/remote-environments/server", {
    method: "POST",
    body: {
      enabled: Boolean(enabled),
      listenerMode: options.listenerMode,
      name: options.name,
    },
  }),
  setRelayEnabled: (enabled, options = {}) => api("/api/settings/remote-environments/relay", {
    method: "POST",
    body: { enabled: Boolean(enabled), name: options.name, provider: options.provider },
  }),
  setTailscaleEnabled: () => api("/api/settings/remote-environments/tailscale", { method: "POST", body: {} }),
  rename: (id, name) => api("/api/settings/remote-environments/rename", { method: "POST", body: { id, name } }),
  remove: (id) => api("/api/settings/remote-environments/remove", { method: "POST", body: { id } }),
};

function mountAccount(root) {
  if (!root) return;
  const render = (status, error = "") => {
    root.replaceChildren();
    const row = document.createElement("div");
    row.className = "settings-row";
    const text = document.createElement("p");
    text.className = "settings-help";
    if (status?.signedIn) {
      text.textContent = `Signed in to ${status.dynerBaseUrl || "Dyner"}. Remote environments and the relay can be enabled below.`;
      row.append(text);
    } else {
      text.textContent = error || `Not signed in. Sign in to ${status?.dynerBaseUrl || "Dyner"} to register this machine as a remote environment.`;
      const button = document.createElement("button");
      button.type = "button";
      button.className = "primary";
      button.textContent = "Sign in to Dyner";
      button.addEventListener("click", async () => {
        button.disabled = true;
        try {
          const started = await api("/api/settings/login/start", { method: "POST" });
          const popup = window.open(started.url, "_blank", "noopener");
          if (!popup) location.assign(started.url);
          const poll = setInterval(async () => {
            try {
              const latest = await api("/api/settings/status");
              if (latest?.signedIn) { clearInterval(poll); location.reload(); }
            } catch { /* keep polling */ }
          }, 2000);
          setTimeout(() => clearInterval(poll), 10 * 60 * 1000);
        } catch (failure) {
          render(status, failure?.message || String(failure));
        }
      });
      row.append(text, button);
    }
    root.append(row);
  };
  api("/api/settings/status").then((status) => render(status)).catch((failure) => render(null, failure?.message || String(failure)));
}

mountAccount(document.querySelector("#settings-account-panel"));
mountWorkspaceManagement(document.querySelector("#settings-workspace-panel"));
mountRemoteEnvironments(document.querySelector("#settings-remote-panel"), { pwaClient: false });
