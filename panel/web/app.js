'use strict';

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

// X-Panel es lo que el servidor exige para cualquier peticion que cambie algo:
// otra web abierta en el navegador no puede añadir cabeceras propias a
// 127.0.0.1 sin un preflight que el panel no responde.
async function api(method, path, body) {
  const opts = { method, headers: { 'X-Panel': '1' } };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = {};
  if (text) { try { data = JSON.parse(text); } catch { data = { error: text }; } }
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

function toast(msg, kind = 'ok') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = msg;
  $('#toasts').append(el);
  setTimeout(() => el.remove(), kind === 'err' ? 8000 : 4000);
}

const run = (fn) => fn().catch((e) => toast(e.message, 'err'));

/* ------------------------------------------------------------------ pestañas */

let activeTab = 'consola';
$$('.tab').forEach((tab) => {
  tab.addEventListener('click', () => {
    activeTab = tab.dataset.tab;
    $$('.tab').forEach((t) => t.classList.toggle('active', t === tab));
    $$('.page').forEach((p) => p.classList.toggle('active', p.id === `page-${activeTab}`));
    if (activeTab === 'jugadores') loadPlayers();
    if (activeTab === 'ficheros') loadFileList();
    if (activeTab === 'backups') loadBackups();
  });
});

/* ------------------------------------------------------------------ consola */

const consoleEl = $('#console');
let lastSeq = 0;
let source = null;
let filterText = '';

// Paper etiqueta la salida como "[13:00:07 WARN]" cuando escribe a un pipe (sin
// terminal no pone el nombre del hilo), y la JVM manda sus propios avisos a
// stderr en texto plano. Por eso se clasifica por contenido y no por el flujo:
// stderr esta lleno de WARNING que no son errores.
function lineClass(line) {
  const t = line.text;
  if (line.kind === 'panel') return t.startsWith('>') ? 'ln cmd' : 'ln panel';
  if (/(ERROR|FATAL|SEVERE)\]|\bException\b|Caused by:|^\s+at [\w.$/]+\(/.test(t)) return 'ln error';
  if (/WARN\]|\bWARN\b|^WARNING:/.test(t)) return 'ln warn';
  if (line.kind === 'err') return 'ln warn';
  return 'ln';
}

function appendLine(line) {
  if (line.seq <= lastSeq) return;
  lastSeq = line.seq;

  const nearBottom = consoleEl.scrollHeight - consoleEl.scrollTop - consoleEl.clientHeight < 60;
  const div = document.createElement('div');
  div.className = lineClass(line);
  div.textContent = line.text;
  if (filterText && !line.text.toLowerCase().includes(filterText)) div.classList.add('hidden');
  consoleEl.append(div);

  // El anillo del servidor guarda 3000 lineas; mantener mas en el DOM solo
  // hace que el navegador se arrastre.
  while (consoleEl.childElementCount > 3000) consoleEl.firstElementChild.remove();

  if ($('#autoscroll').checked && nearBottom) consoleEl.scrollTop = consoleEl.scrollHeight;
}

function connectConsole() {
  if (source) source.close();
  source = new EventSource(`/api/console/stream?since=${lastSeq}`);

  source.onopen = () => { $('#console-meta').textContent = 'consola en vivo'; };
  source.onmessage = (ev) => {
    const line = JSON.parse(ev.data);
    // Un hueco en la secuencia significa que se perdieron lineas (cliente
    // lento): se piden por HTTP en vez de dejar la consola incompleta.
    if (lastSeq && line.seq > lastSeq + 1) {
      const from = lastSeq;
      lastSeq = line.seq - 1;
      run(async () => {
        const gap = await api('GET', `/api/console?since=${from}`);
        const seq = lastSeq;
        lastSeq = from;
        gap.forEach(appendLine);
        lastSeq = Math.max(lastSeq, seq);
      });
    }
    appendLine(line);
  };
  source.onerror = () => {
    $('#console-meta').textContent = 'consola desconectada, reintentando…';
    source.close();
    setTimeout(connectConsole, 2000);
  };
}

$('#filter').addEventListener('input', (e) => {
  filterText = e.target.value.trim().toLowerCase();
  $$('.ln', consoleEl).forEach((el) => {
    el.classList.toggle('hidden', filterText && !el.textContent.toLowerCase().includes(filterText));
  });
});

const history = [];
let historyPos = -1;
const cmdInput = $('#cmd');

$('#cmd-form').addEventListener('submit', (e) => {
  e.preventDefault();
  const cmd = cmdInput.value.trim();
  if (!cmd) return;
  history.unshift(cmd);
  historyPos = -1;
  cmdInput.value = '';
  run(() => api('POST', '/api/command', { cmd }));
});

cmdInput.addEventListener('keydown', (e) => {
  if (e.key === 'ArrowUp' && historyPos + 1 < history.length) {
    historyPos += 1;
    cmdInput.value = history[historyPos];
    e.preventDefault();
  } else if (e.key === 'ArrowDown') {
    historyPos -= 1;
    cmdInput.value = historyPos >= 0 ? history[historyPos] : '';
    if (historyPos < -1) historyPos = -1;
    e.preventDefault();
  }
});

/* ------------------------------------------------------------------ control */

$('#btn-start').addEventListener('click', () => run(() => api('POST', '/api/server/start').then((r) => toast(r.ok))));
$('#btn-stop').addEventListener('click', () => run(() => api('POST', '/api/server/stop').then((r) => toast(r.ok))));
$('#btn-restart').addEventListener('click', () => run(() => api('POST', '/api/server/restart').then((r) => toast(r.ok))));
$('#btn-kill').addEventListener('click', () => {
  if (!confirm('SIGKILL mata la JVM al instante: lo que no esté guardado se pierde. ¿Seguir?')) return;
  run(() => api('POST', '/api/server/kill').then((r) => toast(r.ok, 'err')));
});

/* ------------------------------------------------------------------ estado */

const fmtUptime = (s) => {
  if (!s) return '';
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60), sec = s % 60;
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${sec}s`;
  return `${sec}s`;
};

const STATE_TEXT = { running: 'en marcha', starting: 'arrancando', stopping: 'parando', stopped: 'parado' };

function statCard({ label, value, unit = '', foot = '', level = '', pct = null }) {
  const el = document.createElement('div');
  el.className = `stat ${level}`;
  const v = document.createElement('div');
  v.className = 'value';
  v.textContent = value;
  if (unit) {
    const u = document.createElement('span');
    u.className = 'unit';
    u.textContent = ` ${unit}`;
    v.append(u);
  }
  const l = document.createElement('div');
  l.className = 'label';
  l.textContent = label;
  el.append(l, v);
  if (pct !== null) {
    const bar = document.createElement('div');
    bar.className = 'bar';
    const i = document.createElement('i');
    i.className = level === 'bad' ? 'bad' : level === 'mid' ? 'mid' : '';
    i.style.width = `${Math.max(0, Math.min(100, pct))}%`;
    bar.append(i);
    el.append(bar);
  }
  if (foot) {
    const f = document.createElement('div');
    f.className = 'foot';
    f.textContent = foot;
    el.append(f);
  }
  return el;
}

function kv(box, pairs) {
  box.textContent = '';
  pairs.forEach(([k, v, cls]) => {
    const a = document.createElement('span');
    a.className = 'k';
    a.textContent = k;
    const b = document.createElement('span');
    if (cls) b.className = cls;
    b.textContent = v;
    box.append(a, b);
  });
}

let lastState = null;

function renderStatus(s) {
  const st = s.server.state;
  const pill = $('#state-pill');
  pill.className = `pill ${st}`;
  pill.textContent = STATE_TEXT[st] || st;

  const bits = [];
  if (s.server.pid) bits.push(`pid ${s.server.pid}`);
  if (s.server.uptimeSec) bits.push(fmtUptime(s.server.uptimeSec));
  if (st === 'starting') bits.push('cargando mundo y plugins');
  if (st === 'stopped' && s.server.lastExit) bits.push(s.server.lastExit);
  $('#state-meta').textContent = bits.join(' · ');

  $('#btn-start').disabled = st !== 'stopped';
  $('#btn-stop').disabled = st === 'stopped' || s.server.busy;
  $('#btn-restart').disabled = s.server.busy;
  $('#btn-kill').disabled = !s.server.pid;

  if (s.java.ok && s.java.version) $('#sub').textContent = `${s.java.version} · ${s.java.motd || ''}`.trim();

  const banner = $('#banner');
  if (s.foreignPid) {
    banner.textContent = `Hay otro servidor corriendo en este directorio (pid ${s.foreignPid}) que no ha arrancado el panel: no puede pararlo ni leer su consola. Ciérralo antes de usar los botones.`;
    banner.classList.remove('hidden');
  } else {
    banner.classList.add('hidden');
  }

  // Recarga la lista de jugadores cuando el servidor cambia de estado, para
  // que "op" no siga ofreciendo la vía de ficheros con el servidor ya arriba.
  if (lastState !== st && activeTab === 'jugadores') loadPlayers();
  lastState = st;

  const cards = $('#cards');
  cards.textContent = '';

  const tps = s.tps && s.tps.length ? s.tps[0] : null;
  cards.append(statCard({
    label: 'TPS (1 min)',
    value: tps === null ? '—' : tps.toFixed(1),
    foot: tps === null ? (s.server.ready ? 'midiendo…' : 'servidor no listo') : `medido a las ${s.tpsAt}`,
    level: tps === null ? '' : tps >= 19 ? 'ok' : tps >= 15 ? 'mid' : 'bad',
  }));

  const cpu = s.proc.valid ? s.proc.cpuPercent : null;
  cards.append(statCard({
    label: 'CPU de la JVM',
    value: cpu === null ? '—' : cpu.toFixed(0),
    unit: '%',
    foot: `${s.sys.cpus} núcleos · carga ${s.sys.load1.toFixed(2)}`,
    level: cpu === null ? '' : cpu < 100 ? 'ok' : cpu < 100 * s.sys.cpus * 0.8 ? 'mid' : 'bad',
    pct: cpu === null ? null : cpu / s.sys.cpus,
  }));

  const rss = s.proc.valid ? s.proc.rssMB : null;
  const maxMB = parseMem(s.server.memMax);
  cards.append(statCard({
    label: 'RAM de la JVM',
    value: rss === null ? '—' : (rss / 1024).toFixed(2),
    unit: 'GB',
    foot: `límite ${s.server.memMax} · ${s.proc.threads || 0} hilos`,
    level: rss === null || !maxMB ? '' : rss / maxMB < 0.8 ? 'ok' : rss / maxMB < 0.95 ? 'mid' : 'bad',
    pct: rss === null || !maxMB ? null : (rss / maxMB) * 100,
  }));

  cards.append(statCard({
    label: 'Jugadores',
    value: String(s.server.players.length),
    unit: s.java.ok && s.java.max ? `/ ${s.java.max}` : '',
    foot: s.server.players.length ? s.server.players.map((p) => p.name).join(', ') : 'servidor vacío',
    level: s.server.players.length ? 'ok' : '',
  }));

  cards.append(statCard({
    label: 'RAM de la máquina',
    value: ((s.sys.memTotalMB - s.sys.memFreeMB) / 1024).toFixed(1),
    unit: `/ ${(s.sys.memTotalMB / 1024).toFixed(0)} GB`,
    foot: `${(s.sys.memFreeMB / 1024).toFixed(1)} GB disponibles`,
    level: s.sys.memFreeMB > 1024 ? 'ok' : 'bad',
    pct: ((s.sys.memTotalMB - s.sys.memFreeMB) / s.sys.memTotalMB) * 100,
  }));

  cards.append(statCard({
    label: 'Mundo en disco',
    value: s.worldMB < 1024 ? s.worldMB.toFixed(0) : (s.worldMB / 1024).toFixed(2),
    unit: s.worldMB < 1024 ? 'MB' : 'GB',
    foot: `${s.sys.diskFreeGB.toFixed(1)} GB libres en disco`,
  }));

  $('#java-addr').textContent = s.javaAddr;
  $('#bedrock-addr').textContent = s.bedrockAddr;

  if (s.java.ok) {
    kv($('#java-box'), [
      ['estado', 'responde al ping de la lista de servidores', 'ok'],
      ['versión', s.java.version || '—'],
      ['jugadores', `${s.java.online} / ${s.java.max}`],
      ['MOTD', s.java.motd || '—'],
      ['latencia', `${s.java.latencyMs} ms`],
    ]);
  } else {
    kv($('#java-box'), [['estado', s.java.error || 'sin datos', 'bad']]);
  }

  if (s.bedrock.ok) {
    kv($('#bedrock-box'), [
      ['estado', 'Geyser responde en UDP', 'ok'],
      ['edición', s.bedrock.edition || '—'],
      ['versión', s.bedrock.version || '—'],
      ['jugadores', `${s.bedrock.online} / ${s.bedrock.max}`],
      ['MOTD', s.bedrock.motd || '—'],
      ['latencia', `${s.bedrock.latencyMs} ms`],
    ]);
  } else {
    kv($('#bedrock-box'), [['estado', s.bedrock.error || 'sin datos', 'bad']]);
  }

  // Backups
  const bs = s.backup;
  $('#btn-backup').disabled = bs.running;
  $('#backup-state').textContent = bs.running
    ? `en curso: ${bs.step || 'trabajando'}…`
    : bs.lastErr ? `último intento falló: ${bs.lastErr}` : bs.last ? `último: ${bs.last}` : '';
  if (backupsRunning && !bs.running) loadBackups();
  backupsRunning = bs.running;
  $('#backup-disk').textContent = `${s.sys.diskFreeGB.toFixed(1)} GB libres`;
}

function parseMem(v) {
  if (!v) return 0;
  const m = /^(\d+)([GgMm])$/.exec(v.trim());
  if (!m) return 0;
  return m[2].toUpperCase() === 'G' ? Number(m[1]) * 1024 : Number(m[1]);
}

let backupsRunning = false;

// Un fallo de red se ignora (el ciclo siguiente lo reintenta), pero un error al
// pintar es un bug del panel y hay que verlo: se avisa una sola vez para no
// llenar la pantalla de toasts cada dos segundos.
let renderBroken = false;

async function poll() {
  let data;
  try {
    data = await api('GET', '/api/status');
  } catch {
    return;
  }
  try {
    renderStatus(data);
  } catch (e) {
    if (!renderBroken) {
      renderBroken = true;
      toast(`fallo al pintar el estado: ${e.message}`, 'err');
      console.error(e);
    }
  }
}

/* ------------------------------------------------------------------ jugadores */

function playerItem(name, extra, actions, bedrock) {
  const el = document.createElement('div');
  el.className = 'item';
  const n = document.createElement('span');
  n.className = 'name';
  n.textContent = name;
  el.append(n);
  if (bedrock !== undefined) {
    const b = document.createElement('span');
    b.className = `badge ${bedrock ? 'bedrock' : 'java'}`;
    b.textContent = bedrock ? 'bedrock' : 'java';
    el.append(b);
  }
  if (extra) {
    const w = document.createElement('span');
    w.className = 'why';
    w.textContent = extra;
    el.append(w);
  }
  const sp = document.createElement('span');
  sp.className = 'sp';
  el.append(sp);
  actions.forEach(([label, action, cls]) => {
    const b = document.createElement('button');
    b.className = `btn mini ${cls || ''}`;
    b.textContent = label;
    b.addEventListener('click', () => doAction(action, name));
    el.append(b);
  });
  return el;
}

function doAction(action, player, reason = '') {
  if ((action === 'ban' || action === 'kick') && !confirm(`¿${action} a ${player}?`)) return;
  run(async () => {
    const r = await api('POST', '/api/players/action', { action, player, reason });
    toast(r.ok);
    loadPlayers();
  });
}

function fill(box, items, emptyText) {
  box.textContent = '';
  if (!items.length) {
    const e = document.createElement('div');
    e.className = 'empty';
    e.textContent = emptyText;
    box.append(e);
    return;
  }
  items.forEach((el) => box.append(el));
}

async function loadPlayers() {
  const v = await api('GET', '/api/players').catch((e) => { toast(e.message, 'err'); return null; });
  if (!v) return;

  $('#online-count').textContent = String(v.online.length);
  fill($('#online-list'), v.online.map((p) => playerItem(
    p.name, '', [['op', 'op'], ['expulsar', 'kick', 'warn'], ['banear', 'ban', 'bad']], p.bedrock,
  )), 'nadie conectado');

  fill($('#ops-list'), v.ops.map((o) => playerItem(
    o.name, `nivel ${o.level}`, [['quitar op', 'deop', 'warn']], o.uuid.startsWith('00000000-0000-0000-0'),
  )), 'sin operadores en ops.json');

  fill($('#wl-list'), v.whitelist.map((wp) => playerItem(
    wp.name, '', [['quitar', 'whitelist-remove', 'warn']], wp.uuid.startsWith('00000000-0000-0000-0'),
  )), v.whitelistOn ? 'whitelist activada y VACÍA: nadie puede entrar' : 'whitelist vacía');

  fill($('#bans-list'), v.bans.map((b) => playerItem(
    b.name, b.reason, [['perdonar', 'pardon']],
  )), 'nadie baneado');

  $('#wl-toggle').checked = v.whitelistOn;

  const dl = $('#known-players');
  dl.textContent = '';
  v.known.forEach((k) => {
    const o = document.createElement('option');
    o.value = k.name;
    dl.append(o);
  });

  $('#players-mode').textContent = v.serverUp
    ? 'El servidor está arriba: las acciones se envían como comandos de consola y tienen efecto inmediato.'
    : 'El servidor está parado: se editan ops.json, whitelist.json y banned-players.json directamente. Solo funciona con jugadores que ya hayan entrado alguna vez (hace falta su UUID).';
}

$('#pl-run').addEventListener('click', () => {
  const name = $('#pl-name').value.trim();
  if (!name) return toast('escribe un nombre', 'err');
  doAction($('#pl-action').value, name, $('#pl-reason').value.trim());
});

$('#wl-toggle').addEventListener('change', (e) => {
  const on = e.target.checked;
  run(async () => {
    const r = await api('POST', '/api/players/whitelist', { on });
    toast(r.ok);
    loadPlayers();
  });
});

/* ------------------------------------------------------------------ ficheros */

let currentFile = null;

async function loadFileList() {
  const files = await api('GET', '/api/files').catch((e) => { toast(e.message, 'err'); return null; });
  if (!files) return;

  const box = $('#filelist');
  box.textContent = '';
  let group = null;
  files.forEach((f) => {
    if (f.group !== group) {
      group = f.group;
      const h = document.createElement('div');
      h.className = 'fgroup';
      h.textContent = group;
      box.append(h);
    }
    const b = document.createElement('button');
    b.className = 'fitem';
    if (f.path === currentFile) b.classList.add('active');
    const sz = document.createElement('span');
    sz.className = 'sz';
    sz.textContent = f.human;
    b.textContent = f.path.includes('/') ? f.path.split('/').pop() : f.path;
    b.title = f.path;
    b.append(sz);
    b.addEventListener('click', () => openFile(f.path));
    box.append(b);
  });
}

function openFile(path) {
  run(async () => {
    const r = await api('GET', `/api/file?path=${encodeURIComponent(path)}`);
    currentFile = path;
    $('#edit-path').textContent = path;
    $('#editor').value = r.content;
    $('#editor').disabled = false;
    $('#btn-save').disabled = false;
    $$('.fitem', $('#filelist')).forEach((b) => b.classList.toggle('active', b.title === path));
  });
}

$('#btn-save').addEventListener('click', () => {
  if (!currentFile) return;
  run(async () => {
    const r = await api('PUT', `/api/file?path=${encodeURIComponent(currentFile)}`, { content: $('#editor').value });
    toast(r.ok);
  });
});

/* ------------------------------------------------------------------ backups */

async function loadBackups() {
  const list = await api('GET', '/api/backups').catch((e) => { toast(e.message, 'err'); return null; });
  if (!list) return;

  fill($('#backup-list'), list.map((b) => {
    const el = document.createElement('div');
    el.className = 'item';
    const n = document.createElement('span');
    n.className = 'name';
    n.textContent = b.name;
    const w = document.createElement('span');
    w.className = 'why';
    w.textContent = `${b.human} · ${b.at}`;
    const sp = document.createElement('span');
    sp.className = 'sp';
    const dl = document.createElement('a');
    dl.className = 'btn mini';
    dl.textContent = 'descargar';
    dl.href = `/api/backups/download?name=${encodeURIComponent(b.name)}`;
    const del = document.createElement('button');
    del.className = 'btn mini bad';
    del.textContent = 'borrar';
    del.addEventListener('click', () => {
      if (!confirm(`¿Borrar ${b.name}? No hay vuelta atrás.`)) return;
      run(async () => {
        await api('DELETE', `/api/backups?name=${encodeURIComponent(b.name)}`);
        toast('backup borrado');
        loadBackups();
      });
    });
    el.append(n, w, sp, dl, del);
    return el;
  }), 'todavía no hay backups');
}

$('#btn-backup').addEventListener('click', () => {
  run(async () => {
    const r = await api('POST', '/api/backups');
    toast(r.ok);
    backupsRunning = true;
  });
});

/* ------------------------------------------------------------------ arranque */

connectConsole();
poll();
setInterval(poll, 2000);
setInterval(() => { if (activeTab === 'jugadores') loadPlayers(); }, 5000);
