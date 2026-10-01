const { api, shortID, setMessage, withBusy } = window.LockFlow;

const listBody = document.querySelector('#books');
const detailPanel = document.querySelector('#detail-panel');
const detailBody = document.querySelector('#detail-body');
const detailTitle = document.querySelector('#detail-title');
const message = document.querySelector('#message');

const esc = (value) => String(value ?? '').replace(/[&<>"']/g, (ch) => (
  { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]
));
const when = (value) => value ? new Date(value).toLocaleString() : '-';
const shiftLabel = (shift) => `${shift.kind === 'day' ? '白班' : '夜班'} ${when(shift.start)} → ${when(shift.end)}`;
const toISO = (value) => value ? new Date(value).toISOString() : '';

function queryString() {
  const params = new URLSearchParams();
  const chamber = document.querySelector('#q-chamber').value;
  const status = document.querySelector('#q-status').value;
  const from = toISO(document.querySelector('#q-from').value);
  const to = toISO(document.querySelector('#q-to').value);
  if (chamber) params.set('chamber', chamber);
  if (status) params.set('status', status);
  if (from) params.set('from', from);
  if (to) params.set('to', to);
  const text = params.toString();
  return text ? `?${text}` : '';
}

async function loadList() {
  const books = await api(`/api/handovers${queryString()}`);
  listBody.innerHTML = books.map((book) => {
    const snap = book.snapshot || {};
    const officers = book.status === 'confirmed'
      ? `${esc(book.outgoing_officer)} → ${esc(book.incoming_officer)}`
      : '<span class="muted">未确认</span>';
    const badge = book.status === 'confirmed'
      ? '<span class="status">已确认</span>'
      : '<span class="status warning">草稿</span>';
    return `<tr data-id="${book.id}" style="cursor:pointer">
      <td>${shiftLabel(book.shift)}</td><td>${esc(book.chamber_id)}</td><td>${badge}</td>
      <td>${(snap.transits || []).length}</td>
      <td>${(snap.unfinished_plans || []).length}</td>
      <td>${(snap.pending_commands || []).length}</td>
      <td>${(snap.review_items || []).length}</td>
      <td>${officers}</td><td>${when(book.updated_at)}</td></tr>`;
  }).join('') || '<tr><td colspan="9" class="muted">暂无交接记录，可先生成当前班次草稿</td></tr>';
  listBody.querySelectorAll('tr[data-id]').forEach((row) => {
    row.addEventListener('click', () => openDetail(row.dataset.id));
  });
  setMessage(message, '');
}

function transitsTable(transits) {
  const rows = (transits || []).map((t) => `<tr>
    <td>${shortID(t.cycle_id)}</td><td>${t.direction}</td><td>${t.generation}</td>
    <td>${t.last_stage}</td><td>${t.terminal ? '已结束' : '进行中'}</td>
    <td>${t.vessel_count}</td><td>${when(t.started_at)}</td></tr>`).join('');
  return `<table><thead><tr><th>闸次</th><th>方向</th><th>代次</th><th>最终阶段</th><th>状态</th><th>船舶</th><th>开始</th></tr></thead>
    <tbody>${rows || '<tr><td colspan="7" class="muted">本班次无闸次</td></tr>'}</tbody></table>`;
}

function plansTable(plans) {
  const rows = (plans || []).map((p) => `<tr>
    <td>${shortID(p.id)}</td><td>${p.direction}</td>
    <td>${(p.vessels || []).map((v) => esc(v.name)).join(', ') || '-'}</td>
    <td>${when(p.created_at)}</td></tr>`).join('');
  return `<table><thead><tr><th>计划</th><th>方向</th><th>船舶</th><th>创建</th></tr></thead>
    <tbody>${rows || '<tr><td colspan="4" class="muted">无未完成计划</td></tr>'}</tbody></table>`;
}

function commandsTable(commands) {
  const rows = (commands || []).map((c) => `<tr>
    <td>${shortID(c.id)}</td><td>${c.device}</td><td>${c.action}</td>
    <td><span class="status warning">${c.state}</span></td><td>${c.epoch}</td><td>${when(c.created_at)}</td></tr>`).join('');
  return `<table><thead><tr><th>命令</th><th>设备</th><th>动作</th><th>状态</th><th>Epoch</th><th>下发</th></tr></thead>
    <tbody>${rows || '<tr><td colspan="6" class="muted">无待确认命令</td></tr>'}</tbody></table>`;
}

function leasesTable(leases) {
  const rows = (leases || []).map((l) => `<tr>
    <td>${shortID(l.vessel_id)}</td><td>${esc(l.owner)}</td><td>${l.fencing_token}</td><td>${when(l.expires_at)}</td></tr>`).join('');
  return `<table><thead><tr><th>船舶</th><th>持有者</th><th>令牌</th><th>到期</th></tr></thead>
    <tbody>${rows || '<tr><td colspan="4" class="muted">无相关租约</td></tr>'}</tbody></table>`;
}

function reviewsTable(items) {
  const rows = (items || []).map((r) => `<tr>
    <td><span class="status ${r.severity === 'critical' ? 'critical' : 'warning'}">${r.severity}</span></td>
    <td>${esc(r.summary)}</td><td>${r.type}</td><td>${r.sequence}</td><td>${when(r.occurred_at)}</td></tr>`).join('');
  return `<table><thead><tr><th>级别</th><th>事项</th><th>事件</th><th>序号</th><th>时间</th></tr></thead>
    <tbody>${rows || '<tr><td colspan="5" class="muted">无复盘项</td></tr>'}</tbody></table>`;
}

function chamberBlock(state) {
  if (!state) return '';
  const gates = state.gates ? `上游 ${state.gates.upstream || '-'} ／ 下游 ${state.gates.downstream || '-'}` : '-';
  const queued = (state.queued_vessels || []).map((v) => esc(v.name)).join(', ') || '-';
  return `<table>
    <tr><th>当前闸次</th><td>${state.cycle ? `${state.cycle.stage} (${shortID(state.cycle.id)})` : '空闲'}</td></tr>
    <tr><th>闸门</th><td>${gates}</td></tr>
    <tr><th>水位</th><td>${state.level_m ?? 0} m</td></tr>
    <tr><th>排队船舶</th><td>${queued}</td></tr>
    <tr><th>应急状态</th><td>${state.emergency?.status || 'idle'}</td></tr></table>`;
}

async function openDetail(id) {
  const data = await api(`/api/handovers/${id}`);
  const book = data.book;
  const snap = book.snapshot || {};
  detailTitle.textContent = `交接详情 · ${book.chamber_id} · ${book.shift.kind === 'day' ? '白班' : '夜班'}`;

  const seal = book.seal ? `<table>
    <tr><th>封存算法</th><td>${esc(book.seal.algorithm)}</td></tr>
    <tr><th>封存时间</th><td>${when(book.seal.sealed_at)}</td></tr>
    <tr><th>完整性校验</th><td>${data.seal_intact ? '<span class="status">通过</span>' : '<span class="status critical">异常</span>'}</td></tr>
    <tr><th>摘要</th><td style="word-break:break-all">${esc(book.seal.digest)}</td></tr></table>` : '';

  let actions;
  if (book.status === 'confirmed') {
    actions = `<h2>交班说明（已封存）</h2><table>
      <tr><th>交班值班员</th><td>${esc(book.outgoing_officer)}</td></tr>
      <tr><th>接班值班员</th><td>${esc(book.incoming_officer)}</td></tr>
      <tr><th>说明</th><td style="white-space:pre-wrap">${esc(book.notes) || '-'}</td></tr></table>${seal}`;
  } else {
    actions = `<h2>交班说明</h2>
      <div class="form-row"><div><label>交班值班员</label><input id="officer" value="${esc(book.outgoing_officer)}"></div></div>
      <label>补充说明</label><textarea id="notes" rows="4" placeholder="设备异常、未竟事项、接班注意事项…">${esc(book.notes)}</textarea>
      <div class="form-row" style="margin-top:12px">
        <div><label>接班值班员</label><input id="incoming" placeholder="确认交接时填写"></div>
        <div><label>&nbsp;</label><button id="save-notes">保存说明</button></div>
        <div><label>&nbsp;</label><button id="confirm" class="danger">确认交接并封存</button></div>
      </div>`;
  }

  detailBody.innerHTML = `
    <div class="grid">
      <section class="panel third"><h2>班次与状态</h2><table>
        <tr><th>班次</th><td>${shiftLabel(book.shift)}</td></tr>
        <tr><th>状态</th><td>${book.status === 'confirmed' ? '已确认封存' : '草稿（可补充）'}</td></tr>
        <tr><th>快照时间</th><td>${when(snap.taken_at)}</td></tr>
        <tr><th>原始事件引用</th><td>${(snap.event_refs || []).length} 条</td></tr></table></section>
      <section class="panel third"><h2>闸室当时状态</h2>${chamberBlock(snap.chamber)}</section>
      <section class="panel third"><h2>事件复盘项</h2><div style="max-height:260px;overflow:auto">${reviewsTable(snap.review_items)}</div></section>
    </div>
    <section class="panel wide" style="margin-top:14px"><h2>班内闸次</h2>${transitsTable(snap.transits)}</section>
    <section class="panel wide" style="margin-top:14px"><h2>未完成计划</h2>${plansTable(snap.unfinished_plans)}</section>
    <section class="panel wide" style="margin-top:14px"><h2>设备待确认命令</h2>${commandsTable(snap.pending_commands)}</section>
    <section class="panel wide" style="margin-top:14px"><h2>租约</h2>${leasesTable(snap.leases)}</section>
    <section class="panel wide" style="margin-top:14px">${actions}</section>`;
  detailPanel.hidden = false;
  detailPanel.scrollIntoView({ behavior: 'smooth' });

  if (book.status === 'draft') {
    document.querySelector('#save-notes').addEventListener('click', () => withBusy(
      document.querySelector('#save-notes'),
      async () => {
        try {
          await api(`/api/handovers/${book.id}/notes`, {
            method: 'POST',
            body: JSON.stringify({ notes: document.querySelector('#notes').value, officer: document.querySelector('#officer').value }),
          });
          setMessage(message, '交班说明已保存');
          await loadList();
          await openDetail(id);
        } catch (e) { setMessage(message, e.message); }
      },
    ));
    document.querySelector('#confirm').addEventListener('click', () => withBusy(
      document.querySelector('#confirm'),
      async () => {
        const incoming = document.querySelector('#incoming').value.trim();
        if (!incoming) { setMessage(message, '请填写接班值班员'); return; }
        try {
          const result = await api(`/api/handovers/${book.id}/confirm`, {
            method: 'POST',
            body: JSON.stringify({
              outgoing_officer: document.querySelector('#officer').value.trim(),
              incoming_officer: incoming,
            }),
          });
          setMessage(message, result.seal_intact ? '交接已确认并封存，完整性校验通过' : '交接已确认');
          await loadList();
          await openDetail(id);
        } catch (e) { setMessage(message, e.message); }
      },
    ));
  }
}

document.querySelector('#detail-close').addEventListener('click', () => { detailPanel.hidden = true; });
document.querySelector('#refresh').addEventListener('click', () => loadList().catch((e) => setMessage(message, e.message)));
['q-chamber', 'q-status', 'q-from', 'q-to'].forEach((id) => {
  document.querySelector(`#${id}`).addEventListener('change', () => loadList().catch((e) => setMessage(message, e.message)));
});
document.querySelector('#generate').addEventListener('click', () => withBusy(
  document.querySelector('#generate'),
  async () => {
    const chamber = document.querySelector('#gen-chamber').value;
    try {
      await api(`/api/chambers/${chamber}/handovers`, { method: 'POST', body: '{}' });
      document.querySelector('#q-chamber').value = chamber;
      await loadList();
      setMessage(message, '已生成当前班次交接草稿');
    } catch (e) { setMessage(message, e.message); }
  },
));

loadList().catch((e) => setMessage(message, e.message));
