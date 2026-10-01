const { api, shortID, setMessage, withBusy } = window.LockFlow;

const listHost = document.querySelector("#book-list");
const detailHost = document.querySelector("#detail");
const message = document.querySelector("#message");
let selectedID = null;
let cache = new Map();

const escapeHTML = (value) => String(value ?? "").replace(/[&<>"']/g, (ch) => ({
  "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
}[ch]));

const fmt = (value) => value ? new Date(value).toLocaleString() : "-";
const fmtTime = (value) => value ? new Date(value).toLocaleTimeString() : "-";

async function query() {
  const params = new URLSearchParams();
  const chamber = document.querySelector("#q-chamber").value;
  const from = document.querySelector("#q-from").value;
  const to = document.querySelector("#q-to").value;
  if (chamber) params.set("chamber_id", chamber);
  if (from) params.set("from", from);
  if (to) params.set("to", to + "T23:59:59");
  const books = await api(`/api/handovers?${params}`);
  books.forEach((book) => cache.set(book.id, book));
  renderList(books);
  if (selectedID && books.some((b) => b.id === selectedID)) {
    renderDetail(cache.get(selectedID));
  } else if (books.length) {
    selectBook(books[0].id);
  } else {
    selectedID = null;
    detailHost.className = "muted";
    detailHost.innerHTML = "该时间/闸室范围内没有交接记录。";
  }
}

function renderList(books) {
  if (!books.length) {
    listHost.innerHTML = '<div class="muted">无记录</div>';
    return;
  }
  listHost.innerHTML = books.map((book) => `
    <button type="button" class="book-item ${book.id === selectedID ? "active" : ""} ${book.status}" data-id="${book.id}">
      <span class="book-meta">${escapeHTML(book.chamber_id)} · ${escapeHTML(book.shift_label || "自定义时段")}</span>
      <span class="book-state"><span class="status ${book.status === "confirmed" ? "critical" : "warning"}"></span>${book.status === "confirmed" ? "已确认" : "草稿"}</span>
    </button>`).join("");
  listHost.querySelectorAll(".book-item").forEach((button) => {
    button.addEventListener("click", () => selectBook(button.dataset.id));
  });
}

async function selectBook(id) {
  selectedID = id;
  try {
    const book = await api(`/api/handovers/${id}`);
    cache.set(id, book);
    renderListFromCache();
    renderDetail(book);
  } catch (error) {
    setMessage(message, error.message);
  }
}

function renderListFromCache() {
  const books = Array.from(cache.values()).sort((a, b) =>
    new Date(b.window_start) - new Date(a.window_start));
  renderList(books);
}

function renderDetail(book) {
  detailHost.className = "";
  const snap = book.snapshot;
  const confirmed = book.status === "confirmed";
  const cycles = snap?.cycles || [];
  const plans = snap?.unfinished_plans || [];
  const commands = snap?.pending_commands || [];
  const leases = snap?.leases || [];
  const incidents = snap?.incident_reviews || [];
  const chambers = snap?.chambers || [];

  detailHost.innerHTML = `
    <div class="detail-head">
      <div>
        <h3>${escapeHTML(book.shift_label || "自定义时段")} <span class="muted">(${escapeHTML(book.chamber_id)})</span></h3>
        <div class="muted">窗口 ${fmt(book.window_start)} ~ ${fmt(book.window_end)} · 快照生成于 ${fmt(snap?.captured_at)}</div>
      </div>
      <span class="status ${confirmed ? "critical" : "warning"}">${confirmed ? "已确认封存" : "草稿（可更新）"}</span>
    </div>

    <div class="metrics">
      <div class="metric">班内闸次<strong>${snap?.cycle_count ?? 0}</strong></div>
      <div class="metric">未完成计划<strong>${plans.length}</strong></div>
      <div class="metric">待确认命令<strong>${commands.length}</strong></div>
      <div class="metric">有效租约<strong>${leases.length}</strong></div>
      <div class="metric">复盘项<strong>${incidents.length}</strong></div>
    </div>

    ${chambersSection(chambers)}
    ${cyclesSection(cycles)}
    ${plansSection(plans)}
    ${commandsSection(commands)}
    ${leasesSection(leases)}
    ${incidentsSection(incidents)}

    <div class="notes-block">
      <h4>值班员补充说明</h4>
      ${confirmed ? `
        <div class="notes-readonly">${escapeHTML(book.notes || "（无）")}</div>
        <div class="muted">确认人：${escapeHTML(book.confirmed_by)} · 确认时间：${fmt(book.confirmed_at)}</div>
      ` : `
        <label for="operator">值班员</label>
        <input id="operator" value="${escapeHTML(book.operator || "")}" placeholder="工号或姓名">
        <label for="notes">补充说明（下一班需关注事项）</label>
        <textarea id="notes" rows="4">${escapeHTML(book.notes || "")}</textarea>
        <div class="actions">
          <button id="refresh-btn" type="button">重新汇总当前状态</button>
          <button id="save-notes-btn" type="button">保存说明</button>
          <button id="confirm-btn" class="danger" type="button">确认交接并封存</button>
        </div>
      `}
    </div>`;

  if (!confirmed) {
    document.querySelector("#refresh-btn").addEventListener("click", () =>
      action(book.id, "refresh", null, "快照已更新"));
    document.querySelector("#save-notes-btn").addEventListener("click", () =>
      action(book.id, "notes", {
        operator: document.querySelector("#operator").value,
        notes: document.querySelector("#notes").value,
      }, "说明已保存"));
    document.querySelector("#confirm-btn").addEventListener("click", async () => {
      const operator = document.querySelector("#operator").value.trim();
      const name = window.prompt("请输入确认人（值班员）标识以封存交接簿：", operator);
      if (!name) return;
      await withBusy(document.querySelector("#confirm-btn"), async () => {
        try {
          const updated = await api(`/api/handovers/${book.id}/confirm`, {
            method: "POST",
            body: JSON.stringify({
              confirmed_by: name,
              operator: operator || name,
              notes: document.querySelector("#notes").value,
            }),
          });
          cache.set(book.id, updated);
          setMessage(message, "交接簿已确认封存，后续状态变化不会修改本记录。");
          renderListFromCache();
          renderDetail(updated);
        } catch (error) { setMessage(message, error.message); }
      });
    });
  }
}

async function action(id, kind, extra, okText) {
  const buttons = detailHost.querySelectorAll("button");
  buttons.forEach((b) => { b.disabled = true; });
  try {
    let updated;
    if (kind === "refresh") {
      updated = await api(`/api/handovers/${id}/refresh`, { method: "POST" });
    } else {
      updated = await api(`/api/handovers/${id}/notes`, { method: "POST", body: JSON.stringify(extra) });
    }
    cache.set(id, updated);
    setMessage(message, okText);
    renderListFromCache();
    renderDetail(updated);
  } catch (error) {
    setMessage(message, error.message);
    buttons.forEach((b) => { b.disabled = false; });
  }
}

function chambersSection(chambers) {
  if (!chambers.length) return "";
  const rows = chambers.map((c) => `
    <tr><td>${escapeHTML(c.cycle_id ? shortID(c.cycle_id) : "-")}</td>
    <td>${escapeHTML(c.stage || "idle")}</td>
    <td>上 ${escapeHTML(c.gates?.upstream || "-")} / 下 ${escapeHTML(c.gates?.downstream || "-")}</td>
    <td>${c.queue_size}</td><td>${(c.level_m || 0).toFixed(2)} m</td>
    <td>${escapeHTML(c.emergency || "idle")}</td><td>${c.pending_commands}</td><td>#${c.journal_sequence}</td></tr>`).join("");
  return `<h4>闸室状态快照</h4><table><thead><tr><th>闸次</th><th>阶段</th><th>闸门</th><th>队列</th><th>水位</th><th>应急</th><th>待确认</th><th>事件序号</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function cyclesSection(cycles) {
  if (!cycles.length) return `<h4>班内闸次</h4><div class="muted">本班无闸次。</div>`;
  const rows = cycles.map((c) => `
    <tr><td>${shortID(c.cycle_id)}</td><td>${escapeHTML(c.direction)}</td><td>${escapeHTML(c.stage)}</td>
    <td>${fmtTime(c.started_at)}</td>
    <td>${c.completed_at ? fmtTime(c.completed_at) : '<span class="status warning"></span> 进行中'}</td>
    <td>${(c.events || []).map((e) => eventTag(e)).join(" ") || "-"}</td></tr>`).join("");
  return `<h4>班内闸次（${cycles.length}）</h4><table><thead><tr><th>闸次</th><th>方向</th><th>阶段</th><th>开始</th><th>结束/状态</th><th>事件引用</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function plansSection(plans) {
  if (!plans.length) return `<h4>未完成计划</h4><div class="muted">无未完成计划。</div>`;
  const rows = plans.map((p) => `
    <tr><td>${shortID(p.plan_id)}</td><td>${escapeHTML(p.direction)}</td>
    <td>${escapeHTML((p.vessel_names || []).join(", ") || "-")}</td>
    <td>${p.queued ? '<span class="status warning"></span> 待闸队列中' : "-"}</td>
    <td>${p.cycle_id ? `${shortID(p.cycle_id)} (${escapeHTML(p.cycle_stage)})` : "-"}</td>
    <td>${(p.events || []).map((e) => eventTag(e)).join(" ") || "-"}</td></tr>`).join("");
  return `<h4>未完成计划（${plans.length}）</h4><table><thead><tr><th>计划</th><th>方向</th><th>船舶</th><th>队列</th><th>闸次</th><th>事件引用</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function commandsSection(commands) {
  if (!commands.length) return `<h4>设备待确认命令</h4><div class="muted">无待确认命令。</div>`;
  const rows = commands.map((c) => `
    <tr><td>${shortID(c.command_id)}</td><td>${escapeHTML(c.device)}</td><td>${escapeHTML(c.action)}${c.target ? " → " + c.target : ""}</td>
    <td><span class="status warning"></span> ${escapeHTML(c.state)}</td>
    <td>${fmtTime(c.issued_at)}</td>
    <td>${c.event ? eventTag(c.event) : "-"}</td></tr>`).join("");
  return `<h4>设备待确认命令（${commands.length}）</h4><table><thead><tr><th>命令</th><th>设备</th><th>动作</th><th>状态</th><th>下发时间</th><th>原始命令事件</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function leasesSection(leases) {
  if (!leases.length) return `<h4>有效租约</h4><div class="muted">本班相关船舶无有效租约。</div>`;
  const rows = leases.map((l) => `
    <tr><td>${shortID(l.vessel_id)}</td><td>${escapeHTML(l.owner)}</td><td>${l.fencing_token}</td>
    <td>${fmt(l.expires_at)}</td><td>${l.event ? eventTag(l.event) : "-"}</td></tr>`).join("");
  return `<h4>有效租约（${leases.length}）· fencing token 交接后继续有效</h4><table><thead><tr><th>船舶</th><th>持有者</th><th>fencing token</th><th>过期时间</th><th>原始事件</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function incidentsSection(incidents) {
  if (!incidents.length) return `<h4>事件复盘项</h4><div class="muted">本班无需复盘事件。</div>`;
  const rows = incidents.map((i) => `
    <tr><td><span class="status ${escapeHTML(i.severity)}"></span> ${escapeHTML(i.severity)}</td>
    <td>${escapeHTML(i.summary)}</td><td>${eventTag(i.event)}</td><td>${fmt(i.event.occurred_at)}</td></tr>`).join("");
  return `<h4>事件复盘项（${incidents.length}）</h4><table><thead><tr><th>级别</th><th>摘要</th><th>原始事件引用</th><th>发生时间</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function eventTag(event) {
  if (!event) return "-";
  const payload = event.payload ? encodeURIComponent(typeof event.payload === "string" ? event.payload : JSON.stringify(event.payload)) : "";
  return `<span class="event-ref" title="${escapeHTML(event.type)} #${event.sequence} @ ${fmt(event.occurred_at)}">
    <a href="#" data-event="${escapeHTML(event.id)}" data-payload="${escapeHTML(payload)}">${escapeHTML(event.type)} #${event.sequence}</a></span>`;
}

detailHost.addEventListener("click", async (event) => {
  const link = event.target.closest("a[data-event]");
  if (!link) return;
  event.preventDefault();
  let payloadText = "";
  if (link.dataset.payload) {
    try { payloadText = JSON.stringify(JSON.parse(decodeURIComponent(link.dataset.payload)), null, 2); }
    catch { payloadText = decodeURIComponent(link.dataset.payload); }
  }
  window.alert(`事件 ID: ${link.dataset.event}\n类型/序号见链接文字\n\n原始载荷:\n${payloadText || "(空)"}`);
});

document.querySelector("#query-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  cache = new Map();
  try { await query(); setMessage(message, ""); }
  catch (error) { setMessage(message, error.message); }
});

document.querySelector("#open-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.target.querySelector("button");
  await withBusy(button, async () => {
    try {
      const book = await api("/api/handovers/open", {
        method: "POST",
        body: JSON.stringify({
          chamber_id: document.querySelector("#open-chamber").value,
          operator: document.querySelector("#open-operator").value,
        }),
      });
      cache.set(book.id, book);
      selectedID = book.id;
      setMessage(message, `已生成 ${book.shift_label} 交接簿，快照时间 ${fmt(book.snapshot?.captured_at)}。`);
      renderListFromCache();
      renderDetail(book);
    } catch (error) { setMessage(message, error.message); }
  });
});

query().catch((error) => setMessage(message, error.message));
setInterval(() => { if (!selectedID) return; api(`/api/handovers/${selectedID}`).then((book) => {
  if (book.status === "open") { cache.set(book.id, book); }
}).catch(() => {}); }, 15000);
