// web/src/linecomment.js
// Handles hovering on line numbers to show a pencil icon (✎), and tapping
// it to trigger an inline AI edit on that line. Leaving a GitHub PR review
// comment has its own entry point (select text, Alt+R) and isn't reachable
// from the pencil.
import { doc_ } from './state.js';
import { openAgentEdit } from './agent.js';

export function initLineComment() {
  document.addEventListener('click', e => {
    const btn = e.target.closest('.line-btn');
    if (!btn) return;
    e.preventDefault();
    e.stopPropagation();
    handleLineBtnClick(btn);
  });
}

function handleLineBtnClick(btn) {
  const d = doc_();
  if (!d) return;

  const row = btn.closest('.row');
  let line = 1;
  let text = '';

  if (row) {
    line = +row.dataset.l || 1;
    text = (d.lines && d.lines[line - 1]) || '';
  } else {
    const diffRow = btn.closest('.diff-row, .diff-side');
    if (diffRow) {
      line = diffRow.classList.contains('diff-side-left')
        ? +(diffRow.dataset.oldL || diffRow.dataset.at || diffRow.dataset.l || 1)
        : +(diffRow.dataset.l || diffRow.dataset.at || 1);
      text = diffRow.querySelector('.diff-code')?.textContent || '';
    }
  }

  openAgentEdit({ path: d.path, l1: line, l2: line, text });
}
