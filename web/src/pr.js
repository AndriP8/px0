// web/src/pr.js
// GitHub PR review: shown only when this process was launched as `px0 pr ...`
// (S.meta.pr, set by main.go/pr.go). A persistent bar above the tabs shows
// the PR and hosts Approve/Request Changes/Comment; selecting a diff line and
// pressing Alt+R (or the footer/context-menu action) drafts an inline review
// comment. Everything here talks to /api/pr/*; nothing is stored client-side
// beyond what's needed to repaint -- a page refresh re-fetches the server's
// in-memory draft list (pr.go's prSession), which is the only source of truth.
import { $, S, doc_, esc, api, apiPostJson, keyLabel, withKeys } from './state.js';
import { showToast } from './ui.js';
import { setReviewHandler, SEL_MENU_ITEMS } from './selbar.js';
import { diffview, setPRSyncHandler } from './diff.js';

let meta = null;      // this session's PR info: {number, title, base, head, writeAccess, readOnly}
let comments = [];    // draft comments known to the server

const bar = () => $('#pr-bar');
const list = () => $('#pr-comment-list');

export function initPR() {
  if (!S.meta || !S.meta.pr) return;
  meta = S.meta.pr;
  document.body.classList.add('pr-mode');

  SEL_MENU_ITEMS.push({ sel: 'review-comment', label: 'Add Review Comment', keys: 'Alt+R' });
  setReviewHandler(openCommentComposer);
  setPRSyncHandler(renderMarkersForActiveDoc);
  injectFooterButton();
  wireBarButtons();
  renderBar();
  refreshComments();
}

async function refreshComments() {
  try {
    const j = await api('/api/pr/comments');
    comments = j.comments || [];
    renderBar();
    renderMarkersForActiveDoc();
  } catch {
    // Read-only or a transient error: the bar still shows PR metadata.
  }
}

function renderBar() {
  const b = bar();
  if (!b || !meta) return;
  b.hidden = false;
  $('#pr-badge').textContent = '#' + meta.number;
  $('#pr-title').textContent = meta.title;
  $('#pr-title').title = meta.title;
  $('#pr-refs').textContent = meta.base + ' ← ' + meta.head;
  $('#pr-draft-count').textContent = comments.length
    ? (comments.length + (comments.length === 1 ? ' draft comment' : ' draft comments'))
    : '';
  const ro = $('#pr-readonly-note');
  if (ro) ro.hidden = !meta.readOnly;
  const reqBtn = $('#pr-submit-request-changes');
  const appBtn = $('#pr-submit-approve');
  if (reqBtn) reqBtn.hidden = !meta.writeAccess;
  if (appBtn) appBtn.hidden = !meta.writeAccess;
  const cmtBtn = $('#pr-submit-comment');
  if (cmtBtn) cmtBtn.disabled = meta.readOnly;
}

function wireBarButtons() {
  $('#pr-submit-comment')?.addEventListener('click', () => submitReview('COMMENT'));
  $('#pr-submit-request-changes')?.addEventListener('click', () => submitReview('REQUEST_CHANGES'));
  $('#pr-submit-approve')?.addEventListener('click', () => submitReview('APPROVE'));
}

async function submitReview(event) {
  const bodyEl = $('#pr-review-body');
  const body = bodyEl ? bodyEl.value.trim() : '';
  if (event === 'REQUEST_CHANGES' && !body && !comments.length) {
    showToast('!', 'Add a comment or review body before requesting changes');
    return;
  }
  try {
    await apiPostJson('/api/pr/submit', { event, body });
    comments = [];
    if (bodyEl) bodyEl.value = '';
    closeAllComposers();
    renderBar();
    renderMarkersForActiveDoc();
    showToast('✓', event === 'APPROVE' ? 'Review approved'
      : event === 'REQUEST_CHANGES' ? 'Changes requested'
      : 'Review comment submitted');
  } catch (e) {
    showToast('!', e.message || 'Could not submit review');
  }
}

/* ---------- inline draft comment composer ---------- */

let seq = 0;

function openCommentComposer(info) {
  if (!meta) return;
  if (meta.readOnly) { showToast('!', 'Read-only: no GitHub token configured'); return; }
  if (!info.fromDiff) {
    showToast('!', 'Open this file’s diff (Mod+D) to leave a review comment');
    return;
  }
  const id = 'prc' + (++seq);
  const box = document.createElement('div');
  box.className = 'agent-box';
  box.dataset.id = id;
  const ref = info.path + ':' + (info.l1 === info.l2 ? info.l1 : info.l1 + '-' + info.l2);
  const modEnter = keyLabel('Mod+Enter');
  box.innerHTML =
    '<div class="agent-head"><span class="sel-chip">Review Comment</span>' +
    '<span class="agent-ref">' + esc(ref) + '</span>' +
    '<span class="grow"></span><button class="agent-close" title="Close (Esc)">✕</button></div>' +
    '<div class="agent-compose">' +
    '<textarea class="agent-input" rows="3" spellcheck="false" autocomplete="off" placeholder="Leave a comment on this line... (' + esc(modEnter) + ' to add)"></textarea>' +
    '<div class="agent-err" hidden></div>' +
    '<div class="agent-foot"><span class="agent-hint">' + esc(modEnter) + ' to add, Esc to cancel</span>' +
    '<button class="agent-send" title="Add comment (' + esc(modEnter) + ')">Add Comment</button></div></div>';

  list().hidden = false;
  list().append(box);
  const ta = box.querySelector('.agent-input');
  ta.focus();

  const close = () => { box.remove(); if (!list().children.length) list().hidden = true; };
  box.querySelector('.agent-close').addEventListener('click', close);

  const send = async () => {
    const body = ta.value.trim();
    if (!body) return;
    const errEl = box.querySelector('.agent-err');
    errEl.hidden = true;
    try {
      const c = await apiPostJson('/api/pr/comments', { path: info.path, line: info.l1, side: 'RIGHT', body });
      comments.push(c);
      close();
      renderBar();
      renderMarkersForActiveDoc();
    } catch (e) {
      errEl.hidden = false;
      errEl.textContent = e.message || 'Could not add comment';
    }
  };
  box.querySelector('.agent-send').addEventListener('click', send);
  ta.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); close(); }
    else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); send(); }
  });
}

function closeAllComposers() {
  const l = list();
  if (!l) return;
  l.replaceChildren();
  l.hidden = true;
}

/* ---------- gutter markers on the active diff ---------- */

function renderMarkersForActiveDoc() {
  if (!diffview || diffview.hidden || !meta) return;
  const d = doc_();
  if (!d) return;
  const byLine = new Map();
  for (const c of comments) {
    if (c.path !== d.path) continue;
    if (!byLine.has(c.line)) byLine.set(c.line, []);
    byLine.get(c.line).push(c);
  }
  for (const el of diffview.querySelectorAll('.pr-comment-mark')) el.remove();
  for (const el of diffview.querySelectorAll('[data-l]')) {
    const cs = byLine.get(+el.dataset.l);
    el.classList.toggle('pr-has-comment', !!cs);
    if (!cs) continue;
    const badge = document.createElement('span');
    badge.className = 'pr-comment-mark';
    badge.title = cs.map(c => c.body).join('\n\n');
    badge.textContent = '💬';
    el.querySelector('.diff-code')?.before(badge);
  }
}

/* ---------- launching another PR from a running session ---------- */

// Called from palette.js's "Git: Open Pull Request..." command. Fire and
// forget: the server re-execs a brand new px0 process (pr.go's
// handleLaunchPR), which opens its own browser tab the same way any px0
// invocation does. A failed checkout only ever shows in that child's own
// terminal, not here -- see docs/internals/github-pr-review.md.
export async function launchPR(target) {
  try {
    await apiPostJson('/api/pr/launch', { target });
    showToast('✓', 'Opening PR in a new tab…');
  } catch (e) {
    showToast('!', e.message || 'Could not launch PR review');
  }
}

function injectFooterButton() {
  const sel = $('#footer-sel');
  if (!sel || sel.querySelector('[data-sel="review-comment"]')) return;
  const btn = document.createElement('button');
  btn.className = 'footer-btn';
  btn.dataset.sel = 'review-comment';
  btn.title = withKeys('Add a review comment on this selection ({Alt+R})');
  btn.innerHTML = '<span class="footer-btn-label">Comment</span><kbd class="footer-kbd">' + esc(keyLabel('Alt+R')) + '</kbd>';
  sel.append(btn);
}
