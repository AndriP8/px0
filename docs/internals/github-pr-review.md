# GitHub PR Review

This document describes the design and implementation of px0's GitHub pull request review feature: checkout and lifecycle ([`pr.go`](../../pr.go)), the GitHub REST client ([`github.go`](../../github.go)), merge-base diffing ([`git.go`](../../git.go)), and the frontend ([`web/src/pr.js`](../../web/src/pr.js)).

---

## 1. Zero-Dependency, Process-Scoped Design

Two of px0's existing tenets shape this feature directly (see [`docs/agents/README.md`](../agents/README.md)):

- **Zero-dependency shell-out**: like `git.go`, which never links a Go git library, PR review never links a GitHub SDK. `github.go` is `net/http` against the plain REST API, plus one optional shell-out to the `gh` CLI as a token source. Checkout itself is `git fetch` / `git worktree add` / `git clone`, exactly the same `exec.Command` style as the rest of the codebase.
- **Stateless on disk**: px0 leaves no cache directory under `~/.px0` beyond `settings.json`. A PR checkout is not an exception: `checkoutPR` (`pr.go`) puts the worktree in `os.MkdirTemp("", "px0-pr-*")`, a system temp directory, and `prSession.Close` removes it — called from `main.go` in the same place `lsp.Close()` and `agent.Close()` already run, on both a clean `Ctrl+C` and a normal exit. This falls directly out of the "one PR per process" model: since a PR review is never shared across processes, there is nothing to gain from caching the worktree past the process's own lifetime, and every reason (per the disk-state tenet) not to.

Draft review comments follow the same rule: `prSession.comments` is an in-memory, mutex-guarded slice, identical in spirit to `agentManager.jobs` — never written to disk, gone the moment the process exits (submitted or not).

---

## 2. Target Resolution

`parsePRTarget(arg, cwd)` (`pr.go`) turns a CLI argument into `(owner, repo, num)`:

- A full `github.com/<owner>/<repo>/pull/<n>` URL (with or without a scheme) is self-contained — no filesystem or network access needed to parse it. `isGitHubPRURL` (the same regex) is what lets `main.go` treat a bare pasted URL as a PR target even without the `pr` subcommand.
- A bare number shells `git -C <cwd> remote get-url origin` and regex-matches a `github.com` host out of either the HTTPS or SSH remote form. No `origin`, or a non-`github.com` origin, is a clear CLI error rather than a silent fallback.

`main.go` dispatches on this before the normal `resolveTarget` path: `px0 pr <arg>` or a bare argument matching `isGitHubPRURL` skips directory resolution entirely and calls `checkoutPR` instead.

---

## 3. Checkout

`checkoutPR` (`pr.go`) does, in order:

1. Resolve a GitHub token (§4) and fetch PR metadata (`fetchPRMeta`, `github.go`) — title, author, draft state, base ref, head ref/SHA, and the head repo's clone URL (needed for a fork PR).
2. `os.MkdirTemp` for the worktree destination.
3. If `cwd` is already a checkout of the same `owner/repo` (its `origin` remote matches), fetch `refs/pull/<n>/head:refs/px0/pr/<n>` into that repository and `git worktree add --detach <tmp> refs/px0/pr/<n>` — the common case (`px0 pr 123` run inside the repo you're reviewing) reuses the existing clone's object store instead of downloading it twice.
4. Otherwise (a bare URL from an unrelated directory, or a fork PR), `git clone --filter=blob:none --branch <head-ref> --single-branch <clone-url> <tmp>` — a partial clone of just the PR's branch.
5. Best-effort, fetch the base branch (`refs/heads/<base>:refs/px0/base/<n>`) and compute `gitMergeBase(tmp, "HEAD", "refs/px0/base/<n>")` (§5). If either step fails (e.g. the base branch was force-pushed away since the PR opened), `diffBase` falls back to `"HEAD"` — still a correct diff, just against the PR head's own last commit rather than a clean merge-base.
6. Check push access (§4) once, up front, so the frontend's write UI never has to guess.

`prSession.Close` removes the worktree registration (`git worktree remove --force`) when one exists, then `os.RemoveAll`s the temp directory. Safe to call on a nil `*prSession`, matching `agentManager.Close`'s existing nil-receiver pattern in `main.go`.

Everything downstream — indexing, the git watcher, LSP, agent editing — runs against the checked-out worktree root exactly like it would against any other directory px0 is pointed at. PR review adds no special cases to those subsystems.

---

## 4. Authentication & Push-Access Gating

`resolveGitHubToken(cfg settings)` (`github.go`) tries, in order: the `github.token` setting, the `GITHUB_TOKEN` environment variable, then `gh auth token`. The first non-empty result wins; an all-empty result means read-only review (checkout and diffing still work unauthenticated for a public repo).

`checkPushAccess(ctx, owner, repo, token)` calls `GET /repos/{owner}/{repo}` and reads `permissions.push` off the response — the same "what can the authenticated user do here" signal most GitHub tooling uses, rather than trying to infer collaborator status from a separate endpoint. It **fails closed**: a transport error, a non-200, or a missing field all yield `false`. This matters because the result gates whether Approve/Request Changes even render (`Server.handlePRMeta`, `Server.handleMeta`) — a broken check can only ever hide write actions, never expose ones the token doesn't actually have.

`Server.handlePRSubmit` (`pr.go`) re-checks `p.writeAccess` server-side before allowing `APPROVE` or `REQUEST_CHANGES` through, independent of whatever the frontend chose to render — the frontend gate is a UX convenience, not the enforcement point.

---

## 5. Diffing Against a Merge-Base, Not HEAD

Every other px0 diff (`handleDiff`, `handleGutter`, the `Cmd/Ctrl+D` diff view) compares the working tree to `HEAD`. A PR review needs to compare the PR head to *where it diverged from the target branch* — its merge-base — so the diff shown is exactly what GitHub's own PR view shows, not the PR head's full history against whatever its own last commit is.

`git.go` splits the two diff primitives to take an explicit base:

```go
func gitDiff(root, relpath string) string { return gitDiffAgainst(root, relpath, "HEAD") }
func gitDiffAgainst(root, relpath, base string) string { /* git diff --no-color <base> -- <relpath> */ }

func gitHunksAgainst(root, relpath, base string) (added, modified, deleted []int) { /* ... */ }
```

`gitDiff` is an unchanged one-line wrapper, so every existing caller keeps diffing against `HEAD` with no code change. `Server.diffBase` (`server.go`) is the one new piece of state: it defaults to `"HEAD"` and is set to the PR's merge-base by `Server.SetPR` when `main.go` launches a review session. `handleDiff` and `handleGutter` pass `s.diffBase` through to `gitDiffAgainst`/`gitHunksAgainst` instead of calling the `HEAD`-only wrappers.

`gitMergeBase(root, a, b)` shells `git merge-base a b`, quiet-failing to `""` the same way every other `git.go` helper does (no git, no repo, or the ref not found).

---

## 6. Draft Comments & Review Submission

`prSession.comments []prComment` is the single source of truth for drafts, guarded by `prSession.mu`. The HTTP surface (`pr.go`):

- `GET /api/pr/comments` — list, used both by the frontend on load (so a browser refresh doesn't lose drafts already added — they live on the server, not in the page) and to repaint gutter markers after any change.
- `POST /api/pr/comments` — append one draft (`localPost`-gated, same local-origin check every other write endpoint in px0 uses).
- `POST /api/pr/comments/delete` — remove one by id.
- `POST /api/pr/submit` — `submitReview` (`github.go`) posts everything in one `POST /repos/{o}/{r}/pulls/{n}/reviews` call: the draft comments array, the overall review body, and the verdict (`APPROVE` / `REQUEST_CHANGES` / `COMMENT`). This mirrors GitHub's own two-step "add comments, then submit review" UI in a single API call rather than one request per comment. On success, `prSession.comments` is cleared — a submitted review has nothing left to resubmit.

`Server.handleMeta`'s response gains a `pr` object (number, title, base/head refs, `writeAccess`, `readOnly`, `draftCount`) whenever `Server.pr != nil`, which is how the frontend knows on load whether it's in a review session at all — there is no separate "is this a PR" endpoint.

---

## 7. Frontend

`web/src/pr.js` follows the same one-way registration pattern `web/src/agent.js` already uses to hook into `web/src/selbar.js` (`setAgentHandler`) and `web/src/diff.js` — neither `selbar.js` nor `diff.js` imports anything from `pr.js`; `pr.js` calls `setReviewHandler` (selbar) and `setPRSyncHandler` (diff) once at `initPR()`, so a normal (non-PR) session carries zero PR-review code paths beyond the no-op check `if (!S.meta.pr) return`.

- The draft comment composer reuses the `.agent-box` / `.agent-compose` CSS classes the agent-edit composer already defines, built the same way `openAgentEdit` builds its box (a cloned/constructed DOM node appended to a stacking list), but is a plain synchronous `POST /api/pr/comments` — there is no job to poll, unlike an agent edit, since adding a draft comment is a single request/response rather than a dispatched external process.
- Gutter markers: `diff.js`'s `renderDiff` calls the registered `prSyncHandler` after every repaint (mirroring how it already calls `syncDiffAgentTargets` for agent-edit range highlighting). `pr.js` matches draft comments to the diff's `[data-l]` elements — the same working-tree line-number anchors `selbar.js`'s `diffSelection` already produces for the agent-edit selection flow — and appends a small marker before `.diff-code`.
- Launching another PR (**Git: Open Pull Request…** in the Command Palette) posts to `POST /api/pr/launch` (`pr.go`'s `handleLaunchPR`), which re-execs `os.Executable()` as `px0 pr <target>` and returns immediately — it does not wait for that child's checkout to succeed. The child opens its own browser tab through the same `openBrowser` path any `px0` invocation uses. A checkout failure in the child is therefore only visible in that child process's own terminal output, not surfaced back to the page that launched it; this is an accepted limitation of the fire-and-forget model, not an oversight.

---

## Known Limitations (by design, not oversight)

- No live-updating a review session when new commits land on the PR while it's open — px0's git-awareness watches a local working tree's `.git` control files for changes; it has no equivalent for a remote ref. Reviewing a PR that's still being pushed to means closing and reopening `px0 pr <n>`.
- No replying to or resolving comments that already exist on the PR (only new drafts, submitted as a new review).
- No CI/checks status surfaced in the UI.
- Draft comments are only accepted from a selection made in the diff view (`info.fromDiff` in `pr.js`), so every commentable line is one px0 itself considers part of the diff. If `diffBase` fell back to `HEAD` (§3, when the base branch couldn't be fetched), px0's hunks can diverge from what GitHub computes for the PR, and GitHub's review API rejects a line comment that isn't part of *its* diff. That failure only surfaces at submit time (a `422` from `submitReview`), not when the draft is added.
