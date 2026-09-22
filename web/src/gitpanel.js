// web/src/gitpanel.js
// Sidebar git panel: stage/commit/push/pull, shown whenever the workspace is
// a git repo (same gate as the diff-view toggle in status.js). In a PR
// review session (S.meta.pr set), Push/Pull target the PR's actual head
// branch instead of the checkout's own remote -- see pr.go's Push/Pull.
import { $, S, api, apiPostJson } from './state.js';
import { showToast } from './ui.js';
import { reindexWorkspace } from './panels.js';
import { refreshPRMeta } from './pr.js';
import { layout, render } from './renderer.js';

const panel = () => $('#git-panel');

export function initGitPanel() {
  if (!panel()) return;

  $('#git-panel-collapse')?.addEventListener('click', () => {
    panel()?.classList.toggle('collapsed');
    layout(); render();
  });

  const rz = $('#git-panel-resizer');
  if (rz && panel()) {
    let dragging = false;
    rz.addEventListener('mousedown', e => {
      dragging = true;
      rz.classList.add('drag');
      panel().classList.remove('collapsed');
      e.preventDefault();
    });
    addEventListener('mousemove', e => {
      if (!dragging) return;
      const bottom = panel().getBoundingClientRect().bottom;
      const h = Math.max(60, Math.min(window.innerHeight * 0.6, bottom - e.clientY));
      panel().style.height = h + 'px';
      layout(); render();
    });
    addEventListener('mouseup', () => {
      if (!dragging) return;
      dragging = false;
      rz.classList.remove('drag');
      layout(); render();
    });
  }

  $('#git-stage-all')?.addEventListener('click', () => stagePath('.'));
  $('#git-commit')?.addEventListener('click', doCommit);
  $('#git-push')?.addEventListener('click', doPush);
  $('#git-pull')?.addEventListener('click', doPull);

  updateGitPanelVisibility();
}

function updateGitPanelVisibility() {
  const p = panel();
  if (!p) return;
  p.hidden = !S.meta?.git;
}

// Called from gitstream.js's handleGitStatus with each SSE/refresh payload,
// so the branch name and staged/changed counts stay live without a manual
// reload.
export function updateGitPanel(payload) {
  updateGitPanelVisibility();
  const branchEl = $('#git-branch');
  if (branchEl) {
    const branch = S.meta?.pr ? S.meta.pr.head + ' (PR review)' : (payload?.branch || '');
    branchEl.textContent = branch;
    branchEl.title = branch;
  }
  const countsEl = $('#git-counts');
  if (countsEl) {
    const staged = payload?.staged ? Object.keys(payload.staged).length : 0;
    const changed = payload?.gitChanges || 0;
    countsEl.textContent = changed ? staged + ' staged / ' + changed + ' changed' : '';
  }
}

export async function stagePath(path) {
  try {
    await apiPostJson('/api/git/stage', { path });
  } catch (e) {
    showToast('!', e.message || 'Could not stage');
  }
}

export async function unstagePath(path) {
  try {
    await apiPostJson('/api/git/unstage', { path });
  } catch (e) {
    showToast('!', e.message || 'Could not unstage');
  }
}

async function doCommit() {
  const ta = $('#git-commit-msg');
  const message = ta ? ta.value.trim() : '';
  if (!message) {
    showToast('!', 'Write a commit message first');
    return;
  }
  const btn = $('#git-commit');
  if (btn) btn.disabled = true;
  try {
    await apiPostJson('/api/git/commit', { message });
    if (ta) ta.value = '';
    showToast('✓', 'Committed');
  } catch (e) {
    showToast('!', e.message || 'Commit failed');
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function doPush() {
  const btn = $('#git-push');
  if (btn) btn.disabled = true;
  try {
    await apiPostJson('/api/git/push', {});
    showToast('✓', 'Pushed');
  } catch (e) {
    showToast('!', e.message || 'Push failed');
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function doPull() {
  const btn = $('#git-pull');
  if (btn) btn.disabled = true;
  try {
    const j = await apiPostJson('/api/git/pull', {});
    showToast('✓', j.message || 'Pulled');
    await reindexWorkspace();
    if (S.meta?.pr) await refreshPRMeta();
  } catch (e) {
    showToast('!', e.message || 'Pull failed');
  } finally {
    if (btn) btn.disabled = false;
  }
}
