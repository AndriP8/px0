// web/src/thread.js
import { $, $$, esc, doc_, api, apiPost, apiPostJson, applyKeyLabels } from './state.js';
import { on } from './bus.js';
import { showToast } from './ui.js';
import { openFile } from './tabs.js';
import { setThreadHandler, hideSelectionBar } from './selbar.js';
import { showRightInspector } from './inspector.js';
import { registerAgentPicker, reloadWorkspace } from './agent.js';

/* A thread is a long-running conversation with the coding harness, kept on the
   server. It starts from a spot in the code but is not limited to it: the
   harness may read and change any file, and each turn reports the files it
   touched beside its written reply. Every message continues the same
   conversation, so this is where to ask what code does and follow up, rather
   than one-shot inline edits.

   The right sidebar has two views: the list of threads, and one open thread
   (or a new draft) with its transcript and composer. Two event streams feed it:
   one for the list, which also tells the editor when a finished turn changed
   files so open tabs reload, and one per open thread, carrying the reply as it
   is written. Both are server-sent events, so a dropped connection simply
   reconnects to a fresh snapshot. */

const thr = {
  avail: false,
  list: [],
  filter: 'all',      // all | file
  cur: null,          // the open thread, as last received
  draft: null,        // {path, l1, l2} for a thread not yet created, or {} for a workspace-level one
  listEs: null,
  threadEs: null,
  sending: false,
};

const thrUrl = p => new URL(p, document.baseURI || location.href).href;
const thrRefText = t => t.path ? t.path + ':' + (t.l1 === t.l2 ? t.l1 : t.l1 + '-' + t.l2) : 'Workspace';

function thrAgo(ms) {
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

function thrDur(ms) {
  return ms < 1000 ? ms + 'ms' : ms < 60000 ? (ms / 1000).toFixed(1) + 's' : Math.floor(ms / 60000) + 'm ' + Math.round((ms % 60000) / 1000) + 's';
}

/* ---------- a small, safe Markdown subset for replies ----------
   Everything is escaped first; only fences, inline code, bold and bullet lists
   are then recognised, so a reply can never inject markup. */
function thrInline(s) {
  const h = /^#{1,6}\s+(.*)$/.exec(s);
  if (h) s = '**' + h[1] + '**';
  return esc(s).replace(/`([^`\n]+)`/g, '<code>$1</code>').replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
}

function thrMd(src) {
  const parts = src.split('```');
  let html = '';
  parts.forEach((part, i) => {
    if (i % 2) {
      // The first line of a fence is its language, unless the fence has none.
      const nl = part.indexOf('\n');
      const body = nl >= 0 && /^[\w+#.-]*$/.test(part.slice(0, nl).trim()) ? part.slice(nl + 1) : part;
      html += '<pre><code>' + esc(body.replace(/\n$/, '')) + '</code></pre>';
      return;
    }
    for (const block of part.split(/\n{2,}/)) {
      const b = block.replace(/^\n+|\n+$/g, '');
      if (!b.trim()) continue;
      const lines = b.split('\n');
      if (lines.every(l => /^\s*[-*] /.test(l))) {
        html += '<ul>' + lines.map(l => '<li>' + thrInline(l.replace(/^\s*[-*] /, '')) + '</li>').join('') + '</ul>';
      } else {
        html += '<p>' + lines.map(thrInline).join('<br>') + '</p>';
      }
    }
  });
  return html;
}

/* ---------- list view ---------- */

const thrEl = {};

function thrShow(view) {
  thrEl.listView.hidden = view !== 'list';
  thrEl.threadView.hidden = view !== 'thread';
}

function thrVisible() {
  const d = doc_();
  return thr.list.filter(t => thr.filter !== 'file' || (d && t.path === d.path));
}

function thrDrawList() {
  if (!thrEl.list) return;
  const items = thrVisible();
  const dot = $('#tab-threads .thr-tab-dot');
  if (dot) dot.hidden = !thr.list.some(t => t.running);
  if (!items.length) {
    thrEl.list.innerHTML = '<div class="hint">' + (thr.filter === 'file'
      ? 'No threads started in this file. Select code and press <b data-keys="Alt+T"></b> to start one.'
      : 'No threads yet. Select code and press <b data-keys="Alt+T"></b>, or use + New, to start a long-running conversation.') + '</div>';
    applyKeyLabels(thrEl.list);
    return;
  }
  thrEl.list.innerHTML = items.map(t =>
    '<div class="thr-item' + (thr.cur && thr.cur.id === t.id ? ' sel' : '') + '" data-id="' + esc(t.id) + '" role="button" tabindex="0">' +
    '<div class="thr-item-title">' + (t.running ? '<span class="thr-spin" title="Working"></span>' : t.failed ? '<span class="thr-fail" title="Last reply failed">!</span>' : '') +
    '<span>' + esc(t.title) + '</span>' + (t.kind ? '<span class="thr-kind">' + (t.kind === 'batch' ? 'batch' : 'inline') + '</span>' : '') + '</div>' +
    '<div class="thr-item-meta"><span class="thr-item-ref">' + esc(thrRefText(t)) + '</span>' +
    '<span>' + t.turns + (t.turns === 1 ? ' turn' : ' turns') + ' · ' + thrAgo(t.updated) + '</span></div></div>').join('');
}

function thrApplyList(next) {
  const was = new Map(thr.list.map(t => [t.id, t.running]));
  thr.list = next;
  // A turn that just finished may have rewritten files under open tabs.
  if (next.some(t => was.get(t.id) && !t.running && t.touched)) reloadWorkspace(null, 'Thread');
  thrDrawList();
}

function thrConnectList() {
  if (thr.listEs) return;
  const es = thr.listEs = new EventSource(thrUrl('api/threads/stream'));
  es.addEventListener('list', e => thrApplyList(JSON.parse(e.data)));
  es.addEventListener('summary', e => {
    const s = JSON.parse(e.data);
    const next = thr.list.filter(t => t.id !== s.id);
    next.push(s);
    next.sort((a, b) => b.updated - a.updated);
    thrApplyList(next);
  });
  es.addEventListener('deleted', e => {
    const { id } = JSON.parse(e.data);
    thrApplyList(thr.list.filter(t => t.id !== id));
  });
}

/* ---------- thread view ---------- */

function thrCloseStream() {
  if (thr.threadEs) { thr.threadEs.close(); thr.threadEs = null; }
}

const thrRunning = () => !!(thr.cur && thr.cur.turns.length && thr.cur.turns[thr.cur.turns.length - 1].running);

function thrRenderAnchor() {
  const t = thr.cur || thr.draft;
  const el = thrEl.anchor;
  if (!t || !t.path) {
    el.hidden = !thr.draft;
    el.textContent = 'Workspace thread: not tied to a file';
    el.classList.remove('jump');
    delete el.dataset.path;
    return;
  }
  el.hidden = false;
  el.classList.add('jump');
  el.dataset.path = t.path;
  el.dataset.line = t.l1;
  el.textContent = thrRefText(t);
  el.title = 'Jump to this code';
}

function thrTurnHtml(turn) {
  let h = '<div class="thr-msg thr-user"><div class="thr-who">You</div><div class="thr-body">' + thrMd(turn.prompt) + '</div></div>';
  h += '<div class="thr-msg thr-agent" data-turn="' + turn.id + '"><div class="thr-who">' + esc(turn.harness || 'agent') +
    (turn.model ? ' · ' + esc(turn.model) : '') +
    (turn.running ? ' · <span class="thr-live">working…</span>' : turn.ms ? ' · ' + thrDur(turn.ms) : '') + '</div>';
  if (turn.tools && turn.tools.length) {
    h += '<details class="thr-steps"' + (turn.running ? ' open' : '') + '><summary>' + turn.tools.length + (turn.tools.length === 1 ? ' step' : ' steps') + '</summary>' +
      '<div class="thr-step-list">' + turn.tools.map(x => '<div>' + esc(x) + '</div>').join('') + '</div></details>';
  }
  h += '<div class="thr-body thr-reply">' + thrMd(turn.reply || '') + '</div>';
  if (turn.error) h += '<div class="thr-err">' + esc(turn.error) + '</div>';
  if (!turn.running) {
    if (turn.changed && turn.changed.length) {
      h += '<div class="thr-changed"><span class="thr-changed-label">Changed ' + turn.changed.length + (turn.changed.length === 1 ? ' file' : ' files') + '</span>' +
        turn.changed.map(p => '<button class="thr-file" data-path="' + esc(p) + '" title="Open ' + esc(p) + '">' + esc(p) + '</button>').join('') + '</div>';
    } else if (turn.tracked === false) {
      h += '<div class="thr-changed"><span class="thr-changed-label">Changes unknown outside a git repository; the workspace was reloaded.</span></div>';
    }
  }
  return h + '</div>';
}

function thrRenderMsgs() {
  const box = thrEl.msgs;
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 60;
  const turns = thr.cur ? thr.cur.turns : [];
  box.innerHTML = turns.length ? turns.map(thrTurnHtml).join('')
    : '<div class="hint">' + (thr.draft && thr.draft.path
      ? 'Ask a question about this code, or ask for changes. The reply can touch any file.'
      : 'Ask anything about this workspace. The reply can touch any file.') + '</div>';
  if (stick) box.scrollTop = box.scrollHeight;
}

function thrRenderState() {
  const running = thrRunning();
  thrEl.title.textContent = thr.cur ? thr.cur.title : 'New thread';
  thrEl.del.hidden = !thr.cur;
  thrEl.stop.hidden = !running;
  thrEl.send.disabled = running || thr.sending;
  thrEl.hint.textContent = running ? 'Working. You can send the next message once it replies.' : 'Enter to send, Shift+Enter for a new line';
  thrRenderAnchor();
  thrRenderMsgs();
  thrDrawList();
}

function thrOpenStream(id) {
  thrCloseStream();
  const es = thr.threadEs = new EventSource(thrUrl('api/threads/stream?id=' + encodeURIComponent(id)));
  es.addEventListener('thread', e => {
    if (thr.threadEs !== es) return;
    thr.cur = JSON.parse(e.data);
    thrRenderState();
  });
  es.addEventListener('delta', e => {
    if (thr.threadEs !== es || !thr.cur) return;
    const d = JSON.parse(e.data);
    const turn = thr.cur.turns.find(x => x.id === d.turn);
    if (!turn) return;
    turn.reply = (turn.reply || '') + d.text;
    const body = thrEl.msgs.querySelector('.thr-agent[data-turn="' + d.turn + '"] .thr-reply');
    if (!body) return;
    const box = thrEl.msgs;
    const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 60;
    body.innerHTML = thrMd(turn.reply);
    if (stick) box.scrollTop = box.scrollHeight;
  });
  es.addEventListener('tool', e => {
    if (thr.threadEs !== es || !thr.cur) return;
    const d = JSON.parse(e.data);
    const turn = thr.cur.turns.find(x => x.id === d.turn);
    if (!turn) return;
    (turn.tools = turn.tools || []).push(d.text);
    const box = thrEl.msgs;
    const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 60;
    const msg = box.querySelector('.thr-agent[data-turn="' + d.turn + '"]');
    if (msg) {
      msg.querySelector('.thr-steps')?.remove();
      msg.querySelector('.thr-who').insertAdjacentHTML('afterend',
        '<details class="thr-steps" open><summary>' + turn.tools.length + (turn.tools.length === 1 ? ' step' : ' steps') + '</summary>' +
        '<div class="thr-step-list">' + turn.tools.map(x => '<div>' + esc(x) + '</div>').join('') + '</div></details>');
    }
    if (stick) box.scrollTop = box.scrollHeight;
  });
  es.addEventListener('deleted', () => { if (thr.threadEs === es) thrBack(); });
  es.onerror = () => { /* EventSource reconnects on its own and resends a snapshot */ };
}

export function openThread(id) {
  thr.draft = null;
  thr.cur = null;
  showRightInspector('threads');
  thrShow('thread');
  thrEl.msgs.innerHTML = '<div class="hint">Loading…</div>';
  thrEl.title.textContent = 'Thread';
  thrEl.del.hidden = true;
  thrOpenStream(id);
}

function thrBack() {
  thrCloseStream();
  thr.cur = null;
  thr.draft = null;
  thrShow('list');
  thrDrawList();
}

/* Starts a draft: anchored to info's range, or to the workspace when info is
   omitted. Nothing is created on the server until the first message is sent. */
export function newThread(info) {
  if (!thr.avail) { showToast('!', 'Threads need a coding harness: run px0 without -no-agent'); return; }
  thrCloseStream();
  thr.cur = null;
  thr.draft = info && info.path ? { path: info.path, l1: info.l1, l2: info.l2 } : {};
  showRightInspector('threads');
  thrShow('thread');
  thrRenderState();
  if (info) hideSelectionBar();
  thrEl.input.focus();
}

async function thrSend() {
  const message = thrEl.input.value.trim();
  if (!message || thr.sending || thrRunning()) return;
  thr.sending = true;
  thrEl.send.disabled = true;
  try {
    if (thr.draft) {
      const t = await apiPostJson('/api/threads/create', { ...thr.draft, message });
      thr.draft = null;
      thr.cur = t;
      thrOpenStream(t.id);
    } else if (thr.cur) {
      await apiPostJson('/api/threads/send', { id: thr.cur.id, message });
    } else {
      return;
    }
    thrEl.input.value = '';
  } catch (e) {
    showToast('!', e.message);
  } finally {
    thr.sending = false;
    thrRenderState();
  }
}

export function initThreads() {
  thrEl.listView = $('#thr-list-view');
  thrEl.threadView = $('#thr-thread-view');
  thrEl.list = $('#thr-list');
  thrEl.title = $('#thr-title');
  thrEl.del = $('#thr-delete');
  thrEl.anchor = $('#thr-anchor');
  thrEl.msgs = $('#thr-msgs');
  thrEl.input = $('#thr-input');
  thrEl.send = $('#thr-send');
  thrEl.stop = $('#thr-stop');
  thrEl.hint = $('#thr-hint');
  if (!thrEl.listView) return;

  setThreadHandler(newThread);
  registerAgentPicker({ el: $('.thr-compose'), harnessSelect: $('#thr-harness'), modelSelect: $('#thr-model') });

  $('#thr-new').addEventListener('click', () => newThread(null));
  $('#thr-back').addEventListener('click', thrBack);
  thrEl.send.addEventListener('click', thrSend);
  thrEl.stop.addEventListener('click', () => {
    if (thr.cur) apiPost('/api/threads/cancel', { id: thr.cur.id }).catch(e => showToast('!', e.message));
  });
  thrEl.del.addEventListener('click', async () => {
    if (!thr.cur || !confirm('Delete this thread and its transcript?')) return;
    try {
      await apiPost('/api/threads/delete', { id: thr.cur.id });
      thrBack();
    } catch (e) { showToast('!', e.message); }
  });
  thrEl.input.addEventListener('keydown', e => {
    // Typing here must not trigger editor shortcuts, or Esc closing the sidebar.
    e.stopPropagation();
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); thrSend(); }
    else if (e.key === 'Escape') thrEl.input.blur();
  });
  thrEl.anchor.addEventListener('click', () => {
    const p = thrEl.anchor.dataset.path;
    if (p) openFile(p, { line: +thrEl.anchor.dataset.line || 1 });
  });
  thrEl.msgs.addEventListener('click', e => {
    const f = /** @type {HTMLElement|null} */ (e.target)?.closest('.thr-file');
    if (f) openFile(f.dataset.path);
  });
  const openFromList = e => {
    const item = /** @type {HTMLElement|null} */ (e.target)?.closest('.thr-item');
    if (item) openThread(item.dataset.id);
  };
  thrEl.list.addEventListener('click', openFromList);
  thrEl.list.addEventListener('keydown', e => { if (e.key === 'Enter') openFromList(e); });
  $$('[data-thr-filter]').forEach(b => b.addEventListener('click', () => {
    thr.filter = b.dataset.thrFilter;
    $$('[data-thr-filter]').forEach(x => x.classList.toggle('on', x === b));
    thrDrawList();
  }));
  on('tab:activated', () => { if (thr.filter === 'file') thrDrawList(); });
  // agent.js keeps its inline edit comments at the top of this pane and hands off here.
  on('threads:reveal', () => showRightInspector('threads'));
  on('threads:open', id => { if (id) openThread(id); });
  on('threads:drafts', n => {
    const c = $('#tab-threads .thr-tab-count');
    if (!c) return;
    c.hidden = !n;
    c.textContent = n;
    c.title = n + (n === 1 ? ' comment' : ' comments') + ' waiting to be applied';
  });
  on('threads:shown', () => { if (!thr.cur && !thr.draft) thrShow('list'); thrDrawList(); });

  // Threads exist whenever a harness manager does; -no-agent removes them.
  api('/api/threads').then(j => {
    thr.avail = true;
    $('#tab-threads').hidden = false;
    thr.list = j.threads || [];
    thrDrawList();
    thrConnectList();
  }).catch(() => {
    $('[data-sel="thread"]')?.setAttribute('hidden', '');
  });
}
