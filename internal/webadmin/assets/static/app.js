"use strict";

const pageState = {
  csrf: "",
  username: "",
  credentialsCursor: "",
  registrationsCursor: "",
  connectionsCursor: "",
  sessionsCursor: "",
  auditCursor: "",
  auditQuery: "",
  debugEnabled: false,
};

document.addEventListener("DOMContentLoaded", () => {
  void startPage();
});

async function startPage() {
  const page = document.body.dataset.page || "";
  if (page === "login") {
    bindLogin();
    return;
  }
  try {
    const session = await api("/admin/api/v1/auth/session");
    pageState.csrf = session.csrf_token;
    pageState.username = session.username;
  } catch (error) {
    window.location.assign("/admin/login");
    return;
  }
  bindLogout();
  if (document.querySelector("#password-change-form")) {
    bindPasswordChange();
    return;
  }
  bindRefreshButtons();
  const loaders = {
    overview: loadOverview,
    credentials: setupCredentials,
    registrations: setupRegistrations,
    connections: setupConnections,
    sessions: setupSessions,
    audit: setupAudit,
    administrators: setupAdministrators,
    system: setupSystem,
  };
  if (loaders[page]) {
    await loaders[page]();
  }
}

async function api(path, options = {}) {
  const method = options.method || "GET";
  const headers = { Accept: "application/json", ...(options.headers || {}) };
  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (method !== "GET" && method !== "HEAD" && pageState.csrf) {
    headers["X-CSRF-Token"] = pageState.csrf;
  }
  const response = await fetch(path, { method, headers, body: options.body === undefined ? undefined : JSON.stringify(options.body) });
  const envelope = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(envelope.error?.message || `请求失败（HTTP ${response.status}）`);
    error.status = response.status;
    throw error;
  }
  return envelope.data;
}

function bindLogin() {
  const form = document.querySelector("#login-form");
  form?.addEventListener("submit", async (event) => {
    event.preventDefault();
    clearFormError();
    const data = new FormData(form);
    try {
      await api("/admin/api/v1/auth/login", { method: "POST", body: { username: data.get("username"), password: data.get("password") } });
      window.location.assign("/admin");
    } catch (error) {
      showFormError(error.message);
    }
  });
}

function bindPasswordChange() {
  const form = document.querySelector("#password-change-form");
  form?.addEventListener("submit", async (event) => {
    event.preventDefault();
    clearFormError();
    const data = new FormData(form);
    try {
      if (data.get("new_password") !== data.get("confirm_password")) throw new Error("两次输入的新密码不一致");
      await api("/admin/api/v1/auth/password", { method: "POST", body: { current_password: data.get("current_password"), new_password: data.get("new_password") } });
      form.reset();
      window.location.assign("/admin/login");
    } catch (error) {
      showFormError(error.message);
    }
  });
}

function bindLogout() {
  document.querySelector("#logout-button")?.addEventListener("click", async () => {
    try {
      await api("/admin/api/v1/auth/logout", { method: "POST", body: {} });
    } finally {
      window.location.assign("/admin/login");
    }
  });
}

function bindRefreshButtons() {
  document.addEventListener("click", (event) => {
    const action = event.target.closest("[data-action]")?.dataset.action;
    const actions = {
      "refresh-overview": loadOverview,
      "refresh-credentials": () => loadCredentials(true),
      "refresh-registrations": () => loadRegistrations(true),
      "refresh-connections": () => loadConnections(true),
      "refresh-sessions": () => loadSessions(true),
      "refresh-administrators": loadAdministrators,
      "refresh-system": loadSystem,
    };
    if (actions[action]) {
      void actions[action]();
    }
  });
}

async function loadOverview() {
  await runVisible(async () => {
    const status = await api("/admin/api/v1/overview");
    const cards = document.querySelector("#overview-cards");
    cards?.replaceChildren(
      metric("在线用户", status.principal_count),
      metric("HPRP 连接", status.connection_count),
      metric("Agent 会话", status.session_count),
      metric("可用凭据", status.credentials?.enabled ?? 0),
    );
    fillDetails("#overview-details", {
      "版本": status.version || "-", "运行时长": formatDuration(status.uptime_ms), "企业微信": status.wecom?.state || "-",
      "Relay": status.relay_listen || "-", "Web 管理": status.web_admin_listen || "-", "证书到期": formatTime(status.tls?.not_after),
    });
  });
}

async function setupCredentials() {
  document.querySelector("#credential-form")?.addEventListener("submit", issueCredential);
  document.querySelector("#credentials-next")?.addEventListener("click", () => void loadCredentials(false));
  await loadCredentials(true);
}

async function issueCredential(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  const expiresValue = String(data.get("expires_at") || "");
  const body = {
    principal_id: String(data.get("principal_id") || ""), machine_id: String(data.get("machine_id") || ""),
    sources: splitSources(data.get("sources")),
  };
  if (expiresValue) body.expires_at = new Date(expiresValue).toISOString();
  await runVisible(async () => {
    const issued = await api("/admin/api/v1/credentials", { method: "POST", body });
    showSecret("机器 Key（仅显示一次）", issued.token);
    form.reset();
    await loadCredentials(true);
  });
}

async function loadCredentials(reset) {
  if (reset) pageState.credentialsCursor = "";
  await runVisible(async () => {
    const suffix = pageState.credentialsCursor ? `?limit=100&page_token=${encodeURIComponent(pageState.credentialsCursor)}` : "?limit=100";
    const page = await api(`/admin/api/v1/credentials${suffix}`);
    const body = document.querySelector("#credentials-body");
    body?.replaceChildren(...page.items.map((item) => credentialRow(item)));
    pageState.credentialsCursor = page.next_page_token || "";
    toggleNext("#credentials-next", pageState.credentialsCursor);
  });
}

function credentialRow(item) {
  const actions = actionCell();
  actions.append(
    actionButton(item.status === "enabled" ? "禁用" : "启用", async () => {
      await api(`/admin/api/v1/credentials/${item.credential_id}/${item.status === "enabled" ? "disable" : "enable"}`, { method: "POST", body: {} });
      await loadCredentials(true);
    }),
    actionButton("来源", async () => {
      const value = window.prompt(`设置凭据 ${item.credential_id} 的来源地址（逗号分隔）`, (item.allowed_sources || []).join(", "));
      if (value === null) return;
      await api(`/admin/api/v1/credentials/${item.credential_id}/sources`, { method: "PUT", body: { sources: splitSources(value) } });
      await loadCredentials(true);
    }),
    actionButton("删除", async () => {
      if (!window.confirm(`确认删除凭据 ${item.credential_id}（${item.principal_id}/${item.machine_id}）？`)) return;
      await api(`/admin/api/v1/credentials/${item.credential_id}`, { method: "DELETE", body: { confirm: true } });
      await loadCredentials(true);
    }, "danger"),
  );
  return row(item.credential_id, item.principal_id, item.machine_id, statusBadge(item.status), (item.allowed_sources || []).join(", "), actions);
}

async function setupRegistrations() {
  document.querySelector("#registrations-next")?.addEventListener("click", () => void loadRegistrations(false));
  await loadRegistrations(true);
}

async function loadRegistrations(reset) {
  if (reset) pageState.registrationsCursor = "";
  await runVisible(async () => {
    const suffix = pageState.registrationsCursor ? `?limit=100&page_token=${encodeURIComponent(pageState.registrationsCursor)}` : "?limit=100";
    const page = await api(`/admin/api/v1/registrations${suffix}`);
    document.querySelector("#registrations-body")?.replaceChildren(...page.items.map(registrationRow));
    pageState.registrationsCursor = page.next_page_token || "";
    toggleNext("#registrations-next", pageState.registrationsCursor);
  });
}

function registrationRow(item) {
  const actions = actionCell(
    actionButton("批准", () => approveRegistration(item), "primary"),
    actionButton("拒绝", () => rejectRegistration(item), "danger"),
  );
  return row(item.registration_id, item.principal_id, item.machine_id, (item.allowed_sources || []).join(", "), formatTime(item.requested_at), actions);
}

async function approveRegistration(item) {
  if (!window.confirm(`确认批准 ${item.principal_id}/${item.machine_id}？`)) return;
  const result = await api(`/admin/api/v1/registrations/${encodeURIComponent(item.registration_id)}/approve`, { method: "POST", body: {} });
  notify(`已批准，凭据 ID：${result.credential_id}`);
  await loadRegistrations(true);
}

async function rejectRegistration(item) {
  const reason = window.prompt(`拒绝 ${item.principal_id}/${item.machine_id}，可填写原因`, "");
  if (reason === null) return;
  const result = await api(`/admin/api/v1/registrations/${encodeURIComponent(item.registration_id)}/reject`, { method: "POST", body: { reason } });
  notify(result.notification_sent ? "已拒绝并通知用户" : "已拒绝，但通知发送失败", !result.notification_sent);
  await loadRegistrations(true);
}

async function setupConnections() {
  document.querySelector("#connections-next")?.addEventListener("click", () => void loadConnections(false));
  await loadConnections(true);
}

async function loadConnections(reset) {
  if (reset) pageState.connectionsCursor = "";
  await runVisible(async () => {
    const suffix = pageState.connectionsCursor ? `?limit=100&page_token=${encodeURIComponent(pageState.connectionsCursor)}` : "?limit=100";
    const page = await api(`/admin/api/v1/connections${suffix}`);
    document.querySelector("#connections-body")?.replaceChildren(...page.items.map((item) => {
      const actions = actionCell(actionButton("断开", async () => {
        if (!window.confirm(`确认断开 ${item.principal_id}/${item.machine_id} 的连接 ${item.connection_id}？`)) return;
        await api(`/admin/api/v1/connections/${encodeURIComponent(item.connection_id)}/disconnect`, { method: "POST", body: { confirm: true } });
        await loadConnections(true);
      }, "danger"));
      return row(item.connection_id, `${item.principal_id} / ${item.machine_id}`, `${item.implementation?.name || "-"} ${item.implementation?.version || ""}`, item.source_ip, item.session_count, formatTime(item.last_heartbeat_at), actions);
    }));
    pageState.connectionsCursor = page.next_page_token || "";
    toggleNext("#connections-next", pageState.connectionsCursor);
  });
}

async function setupSessions() {
  document.querySelector("#session-filter")?.addEventListener("submit", (event) => { event.preventDefault(); void loadSessions(true); });
  document.querySelector("#sessions-next")?.addEventListener("click", () => void loadSessions(false));
  await loadSessions(true);
}

async function loadSessions(reset) {
  if (reset) pageState.sessionsCursor = "";
  await runVisible(async () => {
    const params = new URLSearchParams({ limit: "100" });
    const form = document.querySelector("#session-filter");
    if (form) {
      const data = new FormData(form);
      if (data.get("userid")) params.set("userid", data.get("userid"));
      if (data.get("machine_id")) params.set("machine_id", data.get("machine_id"));
    }
    if (pageState.sessionsCursor) params.set("page_token", pageState.sessionsCursor);
    const page = await api(`/admin/api/v1/sessions?${params}`);
    document.querySelector("#sessions-body")?.replaceChildren(...page.items.map((item) => row(
      item.principal_id, item.number, item.target?.machine_id, item.workspace_label, item.display_agent || item.agent || "-", item.pane, statusBadge(item.status_label || item.status),
    )));
    pageState.sessionsCursor = page.next_page_token || "";
    toggleNext("#sessions-next", pageState.sessionsCursor);
  });
}

async function setupAudit() {
  const detail = document.querySelector("#audit-detail");
  detail?.addEventListener("close", clearAuditDetail);
  document.querySelector("#audit-filter")?.addEventListener("submit", (event) => { event.preventDefault(); void loadAudit(true); });
  document.querySelector("#audit-next")?.addEventListener("click", () => void loadAudit(false));
}

async function loadAudit(reset) {
  if (reset) {
    pageState.auditCursor = "";
    clearAuditDetail();
    const data = new FormData(document.querySelector("#audit-filter"));
    const params = new URLSearchParams({ limit: "100" });
    for (const name of ["userid", "machine_id", "keyword"]) {
      if (data.get(name)) params.set(name, data.get(name));
    }
    for (const name of ["start", "end"]) {
      if (data.get(name)) params.set(name, new Date(data.get(name)).toISOString());
    }
    pageState.auditQuery = params.toString();
  }
  await runVisible(async () => {
    const params = new URLSearchParams(pageState.auditQuery || "limit=100");
    if (pageState.auditCursor) params.set("cursor", pageState.auditCursor);
    const page = await api(`/admin/api/v1/audit/logs?${params}`);
    document.querySelector("#audit-body")?.replaceChildren(...page.items.map(auditRow));
    pageState.auditCursor = page.next_cursor || "";
    toggleNext("#audit-next", pageState.auditCursor);
  });
}

function auditRow(item) {
  const preview = document.createElement("button");
  preview.type = "button";
  preview.className = "text-button body-preview";
  preview.textContent = item.body || "（空正文）";
  preview.addEventListener("click", () => {
    const value = document.querySelector("#audit-detail-body");
    value?.replaceChildren(document.createTextNode(item.body || ""));
    document.querySelector("#audit-detail")?.showModal();
  });
  return row(formatTime(item.timestamp), item.event_name, item.userid, item.machine_id || "-", item.agent || "-", item.outcome || "-", preview);
}

function clearAuditDetail() {
  document.querySelector("#audit-detail-body")?.replaceChildren();
}

async function setupAdministrators() {
  document.querySelector("#self-password-form")?.addEventListener("submit", changeOwnPassword);
  document.querySelector("#administrator-form")?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const username = String(new FormData(form).get("username") || "");
    await runVisible(async () => {
      const created = await api("/admin/api/v1/administrators", { method: "POST", body: { username } });
      showSecret(`管理员 ${created.admin.username} 的初始凭据`, `初始密码: ${created.initial_password}\n自动化 Token: ${created.automation_token}`);
      form.reset();
      await loadAdministrators();
    });
  });
  await loadAdministrators();
}

async function changeOwnPassword(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  await runVisible(async () => {
    if (data.get("new_password") !== data.get("confirm_password")) throw new Error("两次输入的新密码不一致");
    await api("/admin/api/v1/auth/password", {
      method: "POST",
      body: { current_password: data.get("current_password"), new_password: data.get("new_password") },
    });
    form.reset();
    window.location.assign("/admin/login");
  });
}

async function loadAdministrators() {
  await runVisible(async () => {
    const result = await api("/admin/api/v1/administrators");
    document.querySelector("#administrators-body")?.replaceChildren(...result.items.map(administratorRow));
  });
}

function administratorRow(item) {
  const username = item.username;
  const enabled = item.automation_token?.enabled;
  const actions = actionCell();
  if (username !== pageState.username) {
    actions.append(actionButton("重置密码", async () => {
      if (!window.confirm(`确认重置管理员 ${username} 的密码？`)) return;
      const result = await api(`/admin/api/v1/administrators/${encodeURIComponent(username)}/reset-password`, { method: "POST", body: { confirm: true } });
      showSecret(`管理员 ${username} 的新初始密码`, result.initial_password);
      await loadAdministrators();
    }));
  }
  actions.append(
    actionButton("轮换 Token", async () => {
      if (!window.confirm(`确认轮换管理员 ${username} 的自动化 Token？旧 Token 将立即失效。`)) return;
      const result = await api(`/admin/api/v1/administrators/${encodeURIComponent(username)}/token/rotate`, { method: "POST", body: { confirm: true } });
      showSecret(`管理员 ${username} 的新自动化 Token`, result.automation_token);
      await loadAdministrators();
    }),
    actionButton(enabled ? "禁用 Token" : "启用 Token", async () => {
      await api(`/admin/api/v1/administrators/${encodeURIComponent(username)}/token/${enabled ? "disable" : "enable"}`, { method: "POST", body: {} });
      await loadAdministrators();
    }),
    actionButton("删除", async () => {
      if (!window.confirm(`确认删除管理员 ${username}？`)) return;
      await api(`/admin/api/v1/administrators/${encodeURIComponent(username)}`, { method: "DELETE", body: { confirm: true } });
      await loadAdministrators();
    }, "danger"),
  );
  return row(username, item.must_change_password ? "需要修改" : "正常", enabled ? "已启用" : "已禁用", item.automation_token?.token_id || "-", actions);
}

async function setupSystem() {
  document.querySelector("#debug-toggle")?.addEventListener("click", () => void toggleDebug());
  document.querySelector("#stop-server")?.addEventListener("click", () => void stopServer());
  renderAutomationGuide();
  await loadSystem();
}

function renderAutomationGuide() {
  const baseURL = window.location.origin;
  const base = document.querySelector("#automation-base-url");
  if (base) base.textContent = baseURL;
  const issue = document.querySelector("#automation-issue-example");
  if (issue) issue.textContent = `export HERDR_PAL_ADMIN_TOKEN='hpa_...'

curl --fail-with-body -X POST '${baseURL}/admin/api/v1/automation/credentials' \\
  -H "Authorization: Bearer $HERDR_PAL_ADMIN_TOKEN" \\
  -H 'Content-Type: application/json' \\
  --data '{
    "principal_id": "企业微信用户 ID",
    "machine_id": "当前运行 Herdr 的机器标识",
    "sources": ["192.168.1.20", "192.168.2.0/24", "10.0.0.10-10.0.0.20"]
  }'`;
  const remove = document.querySelector("#automation-delete-example");
  if (remove) remove.textContent = `curl --fail-with-body -X DELETE '${baseURL}/admin/api/v1/automation/credentials/凭据ID' \\
  -H "Authorization: Bearer $HERDR_PAL_ADMIN_TOKEN"`;
}

async function loadSystem() {
  await runVisible(async () => {
    const status = await api("/admin/api/v1/server/status");
    pageState.debugEnabled = Boolean(status.debug_enabled);
    document.querySelector("#debug-toggle").textContent = pageState.debugEnabled ? "关闭调试日志" : "开启调试日志";
    fillDetails("#system-details", {
      "版本": `${status.version || "-"} (${status.commit || "-"})`, "构建时间": status.built_at || "-", "进程": `${status.pid || "-"} / ${status.os || "-"}-${status.arch || "-"}`,
      "协议": `HPAP ${status.hpap || "-"} / HPRP ${status.hprp || "-"}`, "Relay": status.relay_listen || "-", "管理 Socket": status.admin_socket || "-",
      "Web 管理": status.web_admin_listen || "-", "TLS 指纹": status.tls?.sha256_fingerprint || "-", "基础日志级别": status.base_log_level || "-",
    });
  });
}

async function toggleDebug() {
  await runVisible(async () => {
    await api("/admin/api/v1/server/debug", { method: "POST", body: { enabled: !pageState.debugEnabled } });
    await loadSystem();
  });
}

async function stopServer() {
  if (!window.confirm("确认停止 herdr-pal-server？所有在线连接将断开。")) return;
  await runVisible(async () => {
    await api("/admin/api/v1/server/stop", { method: "POST", body: { confirm: true } });
    notify("Server 正在停止");
  });
}

function metric(label, value) {
  const item = document.createElement("div");
  item.className = "metric";
  item.append(textNode("span", label), textNode("strong", String(value ?? 0)));
  return item;
}

function fillDetails(selector, values) {
  const target = document.querySelector(selector);
  const nodes = [];
  for (const [name, value] of Object.entries(values)) {
    nodes.push(textNode("dt", name), textNode("dd", String(value ?? "-")));
  }
  target?.replaceChildren(...nodes);
}

function row(...values) {
  const tr = document.createElement("tr");
  for (const value of values) {
    const td = document.createElement("td");
    if (value instanceof Node) td.append(value); else td.textContent = value ?? "-";
    tr.append(td);
  }
  return tr;
}

function actionCell(...buttons) {
  const wrapper = document.createElement("div");
  wrapper.append(...buttons);
  return wrapper;
}

function actionButton(label, action, style = "secondary") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `${style} small`;
  button.textContent = label;
  button.addEventListener("click", () => void runVisible(action));
  return button;
}

function statusBadge(value) {
  const badge = textNode("span", value || "unknown");
  badge.className = `status ${String(value || "unknown").toLowerCase().replace(/[^a-z0-9_-]/g, "-")}`;
  return badge;
}

function textNode(name, value) {
  const node = document.createElement(name);
  node.textContent = value;
  return node;
}

function splitSources(value) {
  return String(value || "").split(",").map((item) => item.trim()).filter(Boolean);
}

function toggleNext(selector, cursor) {
  const button = document.querySelector(selector);
  if (button) button.hidden = !cursor;
}

function formatTime(value) {
  if (!value) return "-";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? String(value) : parsed.toLocaleString();
}

function formatDuration(milliseconds) {
  const seconds = Math.max(0, Math.floor((milliseconds || 0) / 1000));
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  return `${days}天 ${hours}小时 ${minutes}分钟`;
}

function showSecret(title, value, onClose) {
  const dialog = document.querySelector("#secret-modal");
  const titleNode = document.querySelector("#secret-title");
  const valueNode = document.querySelector("#secret-value");
  if (!dialog || !titleNode || !valueNode) return;
  titleNode.textContent = title;
  valueNode.replaceChildren(document.createTextNode(value));
  const copy = document.querySelector("#secret-copy");
  const copyValue = async () => {
    try {
      await navigator.clipboard.writeText(valueNode.textContent || "");
      notify("已复制到剪贴板");
    } catch (_) {
      notify("浏览器拒绝访问剪贴板，请手工复制", true);
    }
  };
  copy?.addEventListener("click", copyValue);
  dialog.addEventListener("close", () => {
    copy?.removeEventListener("click", copyValue);
    valueNode.replaceChildren();
    if (onClose) onClose();
  }, { once: true });
  dialog.showModal();
}

async function runVisible(action) {
  try {
    await action();
  } catch (error) {
    if (error.status === 401) {
      window.location.assign("/admin/login");
      return;
    }
    notify(error.message, true);
  }
}

function notify(message, isError = false) {
  const toast = document.querySelector("#toast");
  if (!toast) return;
  toast.textContent = message;
  toast.classList.toggle("error", isError);
  toast.hidden = false;
  window.setTimeout(() => { toast.hidden = true; toast.classList.remove("error"); toast.replaceChildren(); }, 4500);
}

function showFormError(message) {
  const target = document.querySelector("#form-error");
  if (target) target.textContent = message;
}

function clearFormError() {
  const target = document.querySelector("#form-error");
  if (target) target.replaceChildren();
}
