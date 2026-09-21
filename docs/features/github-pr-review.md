# GitHub Pull Request Review

px0 can check out a GitHub pull request's full source tree and review it like any other workspace: full navigation, symbol outline, LSP, and search, alongside a diff scoped to exactly what the PR changes and an Approve / Request Changes / Comment review you can submit without leaving the browser.

---

## Overview & Core Purpose

GitHub's own PR view shows you the diff, but not the codebase around it: jumping to a caller three files away, or checking how a changed function is used elsewhere, means either trusting memory or cloning the branch yourself. `px0 pr` does the checkout for you and hands you the same fast, zero-config reading experience px0 already gives a local repository — just pointed at a PR's head commit instead of your working tree.

---

## Opening a PR

```bash
px0 pr 123                                       # PR number; resolves your cwd's "origin" remote
px0 pr https://github.com/owner/repo/pull/123     # a full URL works from anywhere, no cwd repo needed
px0 https://github.com/owner/repo/pull/123        # the "pr" subcommand is optional for a full URL
```

Each PR review is its own process on its own port — running `px0 pr 456` while another PR review (or your local repo) is already open in px0 does not disturb it. From inside a running px0 session, the Command Palette's **Git: Open Pull Request…** does the same thing: it launches a new px0 process for that PR and opens it in a new browser tab, leaving your current session exactly as it was.

What happens under the hood:
- px0 fetches the PR's head ref (`refs/pull/<n>/head`) into a throwaway git worktree — the same repository, a different checkout, not a second clone, when px0 is launched from inside a checkout of that repo. From anywhere else (or a PR from a fork), px0 does a shallow single-branch clone of the PR head instead.
- The diff view and gutter show changes against the PR's merge-base with its target branch, not the PR head's own `HEAD` — the same PR-scoped diff you'd see on github.com, not a diff against whatever the branch's own last commit happens to be.
- The checkout lives only for the life of the process: closing px0 (`Ctrl+C`) removes it. Nothing is written to `~/.px0` or any cache directory — reopening the same PR later checks it out fresh.

---

## Reviewing

- The **Changes** toggle in the sidebar (the same one used for a local working tree) defaults to the PR's changed files.
- Open a file's diff (`Cmd/Ctrl+D`) and select a line to leave a review comment: the footer selection bar gains a **Comment** button (`Alt+R`), alongside the usual Copy Ref / Copy with Context / Edit Inline / Find Usages actions, and the same action is in the right-click menu.
- Comments are **drafts** until you submit the review — nothing is posted to GitHub as you write them. A PR bar above the tabs tracks the draft count and hosts the overall review body and the three submit actions: **Comment**, **Request Changes**, and **Approve**.
- **Approve** and **Request Changes** only appear if your resolved GitHub credentials have push access to the repository; everyone with read access still gets **Comment**. With no credentials at all, PR review is read-only: you get the checkout and the diff, with no comment or review UI.

Submitting posts one GitHub review carrying every draft comment plus your overall verdict, the same way GitHub's own "finish your review" button works — not one API call per comment.

---

## Authentication

px0 needs a GitHub token to check push access and to post comments/reviews (checkout itself works unauthenticated for public repos). It looks in this order, using the first one it finds:

1. **`github.token`** in Settings (`Cmd/Ctrl+,` → GitHub, or directly in `~/.px0/settings.json`) — the most specific to px0, useful on a headless/remote box with no `gh` installed.
2. **`GITHUB_TOKEN`** environment variable.
3. **`gh auth token`** — if the [GitHub CLI](https://cli.github.com/) is installed and logged in, px0 reuses it with no further setup.

If none resolve, `px0 pr` still works — checkout and diffing don't need a token for a public repository — but the review UI stays hidden.

---

## Keyboard Shortcuts & Controls

| Shortcut / Control | Context | Action |
| :--- | :--- | :--- |
| `Alt+R` | Selection in a diff view, PR review only | Draft a review comment on the selected line |
| Right-Click | Selection in a diff view, PR review only | **Add Review Comment** in the context menu |
| Command Palette → **Git: Open Pull Request…** | Any session | Launch another PR review in a new tab |

---

## Configuration & Preferences

- **`github.token`**: personal access token used for PR review, stored like any other px0 setting. See [Settings & Configuration](settings-and-configuration.md).
- **CLI**: `px0 pr <number-or-url>`. All the usual flags (`-port`, `-no-open`, `-host`, …) apply the same way they do to a normal `px0 <directory>` invocation.

---

## Technical Architecture Deep Dive

For the checkout and worktree lifecycle, the auth resolution and push-access check, the merge-base diffing, and the in-memory (never persisted) draft comment model, see [GitHub PR Review Internals](../internals/github-pr-review.md).
