'use strict';

let range = '30m';
let adapter = '';
let chartData = null;
const COLORS = ['#f85149', '#3fb950', '#d29922', '#bc8cff', '#39c5cf', '#ff7b72', '#7ee787', '#ffa657'];

const $ = id => document.getElementById(id);

async function j(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + ' -> ' + r.status);
  return r.json();
}

function fmtDur(s) {
  if (s == null || isNaN(s)) return '-';
  s = Math.max(0, Math.round(s));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), ss = s % 60;
  if (h > 0) return h + 'h ' + m + 'm';
  if (m > 0) return m + 'm ' + ss + 's';
  return ss + 's';
}

function fmtClock(ms) {
  return new Date(ms).toLocaleTimeString('zh-CN', { hour12: false });
}

function fmtMB(bytes) {
  if (!bytes) return '-';
  if (bytes >= 1024 * 1024 * 1024) return (bytes / 1024 / 1024 / 1024).toFixed(1) + ' GB';
  return Math.round(bytes / 1024 / 1024) + ' MB';
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// ---------- 当前状态 ----------
async function pollSnapshot() {
  let d;
  try { d = await j('/api/snapshot'); } catch (e) { return; }

  const b = $('status');
  if (d.error) { b.textContent = '⚠ ' + d.error; b.className = 'badge err'; }
  else if (d.running) { b.textContent = '监控中'; b.className = 'badge run'; }
  else { b.textContent = '监控已结束'; b.className = 'badge done'; }

  let clock = '已运行 ' + fmtDur(d.elapsed);
  if (d.running && d.remaining > 0) clock += '  |  剩余 ' + fmtDur(d.remaining);
  $('clock').textContent = clock;

  const sel = $('adapter');
  const adapters = d.adapters || [];
  if (sel.options.length !== adapters.length + 1) {
    sel.innerHTML = '<option value="">自动(主卡)</option>' +
      adapters.map(a => '<option value="' + esc(a) + '">' + esc(a) + '</option>').join('');
    sel.value = adapter;
  }

  const cur = d.current || {};
  const totals = cur.totals || {};
  let total = 0;
  if (adapter) total = totals[adapter] || 0;
  else for (const k in totals) total = Math.max(total, totals[k]);
  $('curTotal').textContent = (Math.round(total * 10) / 10) + '%';

  let procs = (cur.procs || []).filter(p => !adapter || p.adapter === adapter);
  procs.sort((a, b2) => b2.pct - a.pct);
  $('curBody').innerHTML = procs.map(p =>
    '<tr><td>' + esc(p.name) + '</td><td class="dim">' + p.pid + '</td><td class="num">' + (Math.round(p.pct * 10) / 10) + '%</td>' +
    '<td class="num dim">' + fmtMB(p.vram) + '</td>' +
    '<td><div class="bar"><i style="width:' + Math.min(100, p.pct) + '%"></i></div></td>' +
    '<td class="dim path">' + esc(p.path) + '</td></tr>'
  ).join('') || '<tr><td colspan="6" class="dim">当前没有进程在使用 GPU</td></tr>';

  $('foot').innerHTML = '累计采样 ' + d.sampleCount + ' 次  |  CSV 每进程每秒一行  |  <a href="/download">下载 CSV 数据</a>';
}

// ---------- 趋势图 ----------
async function pollChart() {
  let d;
  try {
    d = await j('/api/history?range=' + range + (adapter ? '&adapter=' + encodeURIComponent(adapter) : ''));
  } catch (e) { return; }
  chartData = d;
  drawChart(d, null);
}

function drawChart(d, hoverX) {
  const cv = $('chart');
  const dpr = window.devicePixelRatio || 1;
  const W = cv.clientWidth, H = cv.clientHeight;
  cv.width = Math.round(W * dpr); cv.height = Math.round(H * dpr);
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);

  $('chartAdapter').textContent = d.adapter ? '(' + d.adapter + ')' : '';
  const pts = d.points || [];
  if (pts.length < 2) {
    ctx.fillStyle = '#8b949e'; ctx.font = '14px sans-serif';
    ctx.fillText('等待数据...', W / 2 - 40, H / 2);
    return;
  }

  const padL = 46, padR = 14, padT = 12, padB = 26;
  const pw = W - padL - padR, ph = H - padT - padB;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;

  // 取累计占用最高的前 5 个进程画线
  const sums = {};
  for (const p of pts) for (const k in p.procs) sums[k] = (sums[k] || 0) + p.procs[k];
  const top = Object.keys(sums).sort((a, b) => sums[b] - sums[a]).slice(0, 5);

  let yMax = 10;
  for (const p of pts) {
    yMax = Math.max(yMax, p.total);
    for (const k in p.procs) yMax = Math.max(yMax, p.procs[k]);
  }
  yMax = Math.min(100, Math.ceil(yMax / 10) * 10);

  const X = t => padL + (t1 === t0 ? pw / 2 : (t - t0) / (t1 - t0) * pw);
  const Y = v => padT + ph - (v / yMax) * ph;

  // 网格 + Y 轴
  ctx.lineWidth = 1;
  ctx.font = '11px sans-serif';
  const yStep = yMax <= 20 ? 5 : yMax <= 50 ? 10 : 20;
  for (let v = 0; v <= yMax; v += yStep) {
    ctx.strokeStyle = '#21262d';
    ctx.beginPath(); ctx.moveTo(padL, Y(v)); ctx.lineTo(W - padR, Y(v)); ctx.stroke();
    ctx.fillStyle = '#8b949e';
    ctx.fillText(v + '%', 10, Y(v) + 4);
  }
  // X 轴时间
  const nLab = Math.min(6, pts.length - 1);
  for (let i = 0; i <= nLab; i++) {
    const t = t0 + (t1 - t0) * i / nLab;
    const label = fmtClock(t);
    ctx.fillStyle = '#8b949e';
    ctx.fillText(label, X(t) - ctx.measureText(label).width / 2, H - 8);
  }
  // 阈值虚线
  const th = +$('threshold').value || 10;
  if (th <= yMax) {
    ctx.strokeStyle = '#f85149';
    ctx.setLineDash([5, 4]);
    ctx.beginPath(); ctx.moveTo(padL, Y(th)); ctx.lineTo(W - padR, Y(th)); ctx.stroke();
    ctx.setLineDash([]);
  }
  // 折线
  function line(get, color, width) {
    ctx.strokeStyle = color; ctx.lineWidth = width;
    ctx.beginPath();
    let started = false;
    for (const p of pts) {
      const x = X(p.t), y = Y(get(p));
      if (!started) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y);
    }
    ctx.stroke();
  }
  top.forEach((k, i) => line(p => p.procs[k] || 0, COLORS[i % COLORS.length], 1.3));
  line(p => p.total, '#58a6ff', 2);

  // 图例
  $('legend').innerHTML =
    '<span><i style="background:#58a6ff"></i>总占用</span>' +
    top.map((k, i) => '<span><i style="background:' + COLORS[i % COLORS.length] + '"></i>' + esc(k) + '</span>').join('');

  // 悬停十字线 + 数值
  if (hoverX != null && hoverX >= padL && hoverX <= W - padR) {
    let best = pts[0], bd = Infinity;
    for (const p of pts) {
      const dd = Math.abs(X(p.t) - hoverX);
      if (dd < bd) { bd = dd; best = p; }
    }
    const x = X(best.t);
    ctx.strokeStyle = '#8b949e';
    ctx.setLineDash([3, 3]);
    ctx.beginPath(); ctx.moveTo(x, padT); ctx.lineTo(x, padT + ph); ctx.stroke();
    ctx.setLineDash([]);

    const lines = [fmtClock(best.t) + '   总 ' + (Math.round(best.total * 10) / 10) + '%'];
    for (const k of top) if (best.procs[k]) lines.push(k + '  ' + (Math.round(best.procs[k] * 10) / 10) + '%');
    ctx.font = '12px sans-serif';
    const bw = Math.max(...lines.map(l => ctx.measureText(l).width)) + 16;
    const bh = lines.length * 17 + 10;
    let bx = x + 12;
    if (bx + bw > W - padR) bx = x - bw - 12;
    ctx.fillStyle = 'rgba(13,17,23,0.95)';
    ctx.strokeStyle = '#30363d';
    ctx.fillRect(bx, padT + 4, bw, bh);
    ctx.strokeRect(bx, padT + 4, bw, bh);
    ctx.fillStyle = '#e6edf3';
    lines.forEach((l, i) => ctx.fillText(l, bx + 8, padT + 22 + i * 17));
  }
}

// ---------- 进程统计(核心: ≥阈值 时长) ----------
async function pollSummary() {
  const th = +$('threshold').value || 10;
  let d;
  try {
    d = await j('/api/summary?threshold=' + th + (adapter ? '&adapter=' + encodeURIComponent(adapter) : ''));
  } catch (e) { return; }
  $('sumTitle').textContent = 'GPU 占用 ≥ ' + th + '% 的进程统计  ' + (d.adapter ? '(' + d.adapter + ')' : '');
  $('sumBody').innerHTML = (d.items || []).map(it =>
    '<tr>' +
    '<td>' + esc(it.name) + (it.pids && it.pids.length > 1 ? ' <span class="dim">×' + it.pids.length + '进程</span>' : '') + '</td>' +
    '<td class="dim path">' + esc(it.path) + '</td>' +
    '<td class="num ' + (it.timeAbove > 0 ? 'hot' : 'dim') + '" style="font-weight:600">' + fmtDur(it.timeAbove) + '</td>' +
    '<td class="num">' + fmtDur(it.timeActive) + '</td>' +
    '<td class="num">' + (Math.round(it.max * 10) / 10) + '%</td>' +
    '<td class="num">' + (Math.round(it.avg * 10) / 10) + '%</td>' +
    '<td class="dim">' + fmtClock(it.firstSeen) + '</td>' +
    '<td class="dim">' + fmtClock(it.lastSeen) + '</td>' +
    '</tr>'
  ).join('') || '<tr><td colspan="8" class="dim">暂无进程达到阈值</td></tr>';
}

// ---------- 事件 ----------
$('rangeBtns').addEventListener('click', e => {
  if (e.target.tagName !== 'BUTTON') return;
  range = e.target.dataset.r;
  document.querySelectorAll('#rangeBtns button').forEach(b => b.classList.toggle('active', b === e.target));
  pollChart();
});
$('adapter').addEventListener('change', e => {
  adapter = e.target.value;
  pollChart(); pollSummary(); pollSnapshot();
});
$('threshold').addEventListener('change', () => {
  pollSummary();
  if (chartData) drawChart(chartData, null);
});
$('stopBtn').addEventListener('click', () => {
  if (confirm('确定停止监控吗？')) fetch('/api/stop', { method: 'POST' });
});
$('chart').addEventListener('mousemove', e => {
  if (!chartData) return;
  const rect = e.target.getBoundingClientRect();
  drawChart(chartData, e.clientX - rect.left);
});
$('chart').addEventListener('mouseleave', () => { if (chartData) drawChart(chartData, null); });
window.addEventListener('resize', () => { if (chartData) drawChart(chartData, null); });

setInterval(pollSnapshot, 2000);
setInterval(pollChart, 5000);
setInterval(pollSummary, 10000);
pollSnapshot(); pollChart(); pollSummary();
