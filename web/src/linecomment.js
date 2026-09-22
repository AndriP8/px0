// web/src/linecomment.js
// Handles hovering on line numbers to show a pencil icon (✎),
// and tapping it to choose between leaving a GitHub PR review comment
// or triggering an inline AI edit.
import { $, S, doc_ } from './state.js';
import { openAgentEdit } from './agent.js';
import { openCommentComposer } from './pr.js';

let menuEl = null;
let activeTarget = null; // { path, l1, l2, text, side, fromDiff, fromSource }

export function initLineComment() {
  createMenu();

  // Listen for clicks on the line pencil button
  document.addEventListener('click', e => {
    const btn = e.target.closest('.line-btn');
    if (btn) {
      e.preventDefault();
      e.stopPropagation();
      handleLineBtnClick(btn);
      return;
    }

    // Handle clicks inside the popup menu
    const menuItem = e.target.closest('#line-comment-menu .sel-menu-item');
    if (menuItem) {
      e.preventDefault();
      e.stopPropagation();
      handleMenuAction(menuItem.dataset.action);
      return;
    }

    // Clicking outside closes the menu
    if (menuEl && !menuEl.hidden && !e.target.closest('#line-comment-menu')) {
      closeLineCommentMenu();
    }
  });

  // Close on Escape, window resize, or scroll
  window.addEventListener('keydown', e => {
    if (e.key === 'Escape' && menuEl && !menuEl.hidden) {
      closeLineCommentMenu();
    }
  });
  window.addEventListener('resize', closeLineCommentMenu);
}

function createMenu() {
  if ($('#line-comment-menu')) {
    menuEl = $('#line-comment-menu');
    return;
  }
  menuEl = document.createElement('div');
  menuEl.id = 'line-comment-menu';
  menuEl.className = 'line-comment-menu';
  menuEl.setAttribute('role', 'menu');
  menuEl.hidden = true;
  document.body.append(menuEl);
}

export function closeLineCommentMenu() {
  if (menuEl) menuEl.hidden = true;
  activeTarget = null;
}

function handleLineBtnClick(btn) {
  const d = doc_();
  if (!d) return;

  const row = btn.closest('.row');
  let line = 1;
  let text = '';
  let side = 'RIGHT';

  if (row) {
    line = +row.dataset.l || 1;
    text = (d.lines && d.lines[line - 1]) || '';
  } else {
    const diffRow = btn.closest('.diff-row, .diff-side');
    if (diffRow) {
      side = diffRow.classList.contains('diff-side-left') ? 'LEFT' : 'RIGHT';
      line = side === 'LEFT'
        ? +(diffRow.dataset.oldL || diffRow.dataset.at || diffRow.dataset.l || 1)
        : +(diffRow.dataset.l || diffRow.dataset.at || 1);
      text = diffRow.querySelector('.diff-code')?.textContent || '';
    }
  }

  activeTarget = {
    path: d.path,
    l1: line,
    l2: line,
    text,
    side,
    fromDiff: !!(d.diffMode || !row),
    fromSource: !!row,
  };

  const isPR = !!(S.meta?.pr);

  // When not in PR review mode, GitHub review comments are not applicable;
  // tapping the pencil directly opens the inline AI edit flow immediately.
  if (!isPR) {
    closeLineCommentMenu();
    openAgentEdit({
      path: activeTarget.path,
      l1: activeTarget.l1,
      l2: activeTarget.l2,
      text: activeTarget.text,
    });
    return;
  }

  // In PR review mode: present both options in a sleek popup menu
  showMenu(btn);
}

function showMenu(btn) {
  if (!menuEl) createMenu();
  menuEl.replaceChildren();

  const isPR = !!(S.meta?.pr);

  // Option 1: Leave Review Comment on GitHub (PR review only)
  if (isPR) {
    const isReadOnly = !!S.meta?.pr?.readOnly;
    const title = isReadOnly ? 'Comment on PR (Draft / Batch Apply)' : 'Comment on GitHub PR';
    const desc = isReadOnly
      ? `Draft comment for line ${activeTarget.l1} (apply with AI agent locally)`
      : `Draft review comment for line ${activeTarget.l1}`;
    const ghBtn = document.createElement('button');
    ghBtn.className = 'sel-menu-item';
    ghBtn.dataset.action = 'github-comment';
    ghBtn.setAttribute('role', 'menuitem');
    ghBtn.innerHTML = `
      <div class="menu-item-content">
        <span class="menu-item-title"><span class="menu-icon">💬</span> ${esc(title)}</span>
        <span class="menu-item-desc">${esc(desc)}</span>
      </div>
      <kbd class="footer-kbd">Alt+R</kbd>
    `;
    menuEl.append(ghBtn);
  }

  // Option 2: Edit Inline with AI
  const agentBtn = document.createElement('button');
  agentBtn.className = 'sel-menu-item';
  agentBtn.dataset.action = 'agent-edit';
  agentBtn.setAttribute('role', 'menuitem');
  agentBtn.innerHTML = `
    <div class="menu-item-content">
      <span class="menu-item-title"><span class="menu-icon">⚡</span> Edit Inline with AI</span>
      <span class="menu-item-desc">Prompt coding agent to edit this line</span>
    </div>
    <kbd class="footer-kbd">Alt+E</kbd>
  `;
  menuEl.append(agentBtn);

  menuEl.hidden = false;

  // Position adjacent to the line pencil button
  const rect = btn.getBoundingClientRect();
  const menuW = menuEl.offsetWidth || 270;
  const menuH = menuEl.offsetHeight || 95;

  let x = rect.right + 8;
  let y = rect.top - 6;

  if (x + menuW > window.innerWidth - 8) {
    x = Math.max(8, rect.left - menuW - 8);
  }
  if (y + menuH > window.innerHeight - 8) {
    y = Math.max(8, window.innerHeight - menuH - 8);
  }

  menuEl.style.left = Math.round(x) + 'px';
  menuEl.style.top = Math.round(y) + 'px';
}

function handleMenuAction(action) {
  if (!activeTarget) return;
  const target = { ...activeTarget };
  closeLineCommentMenu();

  if (action === 'github-comment') {
    openCommentComposer({
      path: target.path,
      l1: target.l1,
      l2: target.l2,
      text: target.text,
      side: target.side || 'RIGHT',
      fromDiff: target.fromDiff,
      fromSource: target.fromSource,
    });
  } else if (action === 'agent-edit') {
    openAgentEdit({
      path: target.path,
      l1: target.l1,
      l2: target.l2,
      text: target.text,
    });
  }
}
