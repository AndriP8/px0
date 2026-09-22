package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pr.go handles git forge pull/merge request reviews: checking out a PR's
// source tree into a throwaway git worktree, diffing it against the merge-base
// with its target branch instead of HEAD, and letting the reviewer leave draft
// comments and submit reviews or batch apply them with AI agents.
//
// Nothing persists past the process. The worktree lives in a system temp dir
// and is removed in prSession.Close.

// prSession is one checked-out PR review. Draft comments live only in
// memory (mu-guarded), same lifetime as an agentJob -- never written to
// disk, never surviving a restart.
type prSession struct {
	mu sync.Mutex

	provider    GitProvider
	target      PRTarget
	meta        PRMeta
	token       string
	writeAccess bool
	diffBase    string // merge-base(head, base branch), or "HEAD" if the base couldn't be resolved

	worktree string // temp checkout, removed in Close
	srcRepo  string // the repo the worktree was registered against ("" for a plain clone)

	comments []prComment
	nextID   int64
}

// ErrPRMergedCancelled is returned when opening an already-merged PR is cancelled.
var ErrPRMergedCancelled = errors.New("PR is already merged; opening cancelled")

// checkoutPR fetches a PR's head ref and checks it out into a system temp
// directory: a git worktree of cwd's origin when cwd is already a clone of
// the same repo (the common case -- opened inside the repo), or a shallow
// single-branch clone of the PR head otherwise (a bare URL opened from an
// unrelated directory).
func checkoutPR(ctx context.Context, provider GitProvider, target PRTarget, cwd string, onProgress func(string), onMerged func(meta PRMeta) (bool, error)) (*prSession, error) {
	cfg := readSettings()
	token, _ := provider.ResolveToken(cfg)

	if onProgress != nil {
		onProgress(fmt.Sprintf("Fetching PR #%d metadata from %s...", target.Number, provider.Name()))
	}
	meta, err := provider.FetchPR(ctx, target, token)
	if err != nil {
		return nil, err
	}

	if meta.Merged && onMerged != nil {
		proceed, err := onMerged(meta)
		if err != nil {
			return nil, err
		}
		if !proceed {
			return nil, ErrPRMergedCancelled
		}
	}

	tmp, err := os.MkdirTemp("", "px0-pr-*")
	if err != nil {
		return nil, err
	}
	// macOS TempDir lives under /var -> /private/var; git rev-parse
	// --show-toplevel reports the resolved path, so leaving tmp unresolved
	// makes gitStatusAgainst's toplevel-relative prefix check fail for every
	// file, silently emptying the PR's diff/status view.
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	cleanup := func() { os.RemoveAll(tmp) }

	srcRepo := ""
	if info := gitProbe(cwd); info.ok {
		if originURL, err := exec.Command("git", "-C", info.toplevel, "remote", "get-url", "origin").Output(); err == nil {
			orig := strings.ToLower(strings.TrimSpace(string(originURL)))
			if target.Owner != "" && target.Repo != "" &&
				strings.Contains(orig, strings.ToLower(target.Owner)) &&
				strings.Contains(orig, strings.ToLower(target.Repo)) {
				srcRepo = info.toplevel
			}
		}
	}

	num := target.Number
	if srcRepo != "" {
		if onProgress != nil {
			onProgress(fmt.Sprintf("Fetching PR #%d head and preparing worktree...", num))
		}
		headRefspec := fmt.Sprintf("refs/pull/%d/head:refs/px0/pr/%d", num, num)
		if out, err := exec.Command("git", "-C", srcRepo, "fetch", "--no-tags", "origin", headRefspec).CombinedOutput(); err != nil {
			cleanup()
			return nil, fmt.Errorf("git fetch PR head: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("git", "-C", srcRepo, "worktree", "add", "--detach", tmp, fmt.Sprintf("refs/px0/pr/%d", num)).CombinedOutput(); err != nil {
			cleanup()
			return nil, fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else {
		if onProgress != nil {
			onProgress(fmt.Sprintf("Cloning PR #%d (%s)...", num, meta.HeadRef))
		}
		cloneURL := meta.HeadRepoCloneURL
		if cloneURL == "" {
			cloneURL = fmt.Sprintf("https://github.com/%s/%s.git", target.Owner, target.Repo)
		}
		if out, err := exec.Command("git", "clone", "--filter=blob:none", "--branch", meta.HeadRef, "--single-branch", cloneURL, tmp).CombinedOutput(); err != nil {
			cleanup()
			return nil, fmt.Errorf("git clone PR head: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	if onProgress != nil {
		onProgress(fmt.Sprintf("Computing merge base with %s...", meta.BaseRef))
	}
	// Best-effort: fetch the base branch and compute a merge-base so review
	// diffs show exactly what the PR changes rather than the head's full
	// HEAD diff. If this fails (e.g. base branch was force-pushed away),
	// fall back to HEAD -- still correct for the PR head's own worktree.
	diffBase := "HEAD"
	baseRefspec := fmt.Sprintf("refs/heads/%s:refs/px0/base/%d", meta.BaseRef, num)
	baseRemote := "origin"
	if srcRepo == "" {
		baseRemote = fmt.Sprintf("https://github.com/%s/%s.git", target.Owner, target.Repo)
	}
	if _, err := exec.Command("git", "-C", tmp, "fetch", "--no-tags", baseRemote, baseRefspec).CombinedOutput(); err == nil {
		if mb := gitMergeBase(tmp, "HEAD", fmt.Sprintf("refs/px0/base/%d", num)); mb != "" {
			diffBase = mb
		}
	}
	if diffBase == "HEAD" && srcRepo != "" {
		if mb := gitMergeBase(tmp, "HEAD", "origin/"+meta.BaseRef); mb != "" {
			diffBase = mb
		}
	}

	writeAccess := provider.CheckPushAccess(ctx, target, token)

	return &prSession{
		provider:    provider,
		target:      target,
		meta:        meta,
		token:       token,
		writeAccess: writeAccess,
		diffBase:    diffBase,
		worktree:    tmp,
		srcRepo:     srcRepo,
	}, nil
}

func (p *prSession) Root() string { return p.worktree }

// Close removes the worktree registration (if any), cleans up temporary
// references, and deletes the temp checkout. Safe on a nil receiver.
func (p *prSession) Close() {
	if p == nil {
		return
	}
	if p.srcRepo != "" {
		exec.Command("git", "-C", p.srcRepo, "worktree", "remove", "--force", p.worktree).Run()
		exec.Command("git", "-C", p.srcRepo, "update-ref", "-d", fmt.Sprintf("refs/px0/pr/%d", p.meta.Number)).Run()
		exec.Command("git", "-C", p.srcRepo, "update-ref", "-d", fmt.Sprintf("refs/px0/base/%d", p.meta.Number)).Run()
	}
	os.RemoveAll(p.worktree)
}

// ---------------------------------------------------------------- HTTP

func (s *Server) prOrFail(w http.ResponseWriter) bool {
	if s.pr == nil {
		fail(w, http.StatusNotFound, "not a PR review session")
		return false
	}
	return true
}

func (s *Server) handlePRMeta(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	writeJSON(w, map[string]any{
		"number":      p.meta.Number,
		"title":       p.meta.Title,
		"author":      p.meta.Author,
		"base":        p.meta.BaseRef,
		"head":        p.meta.HeadRef,
		"state":       p.meta.State,
		"merged":      p.meta.Merged,
		"mergedAt":    p.meta.MergedAt,
		"writeAccess": p.writeAccess,
		"readOnly":    p.token == "",
		"draftCount":  len(p.comments),
	})
}

func (s *Server) handlePRComments(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	switch r.Method {
	case http.MethodGet:
		p.mu.Lock()
		defer p.mu.Unlock()
		comments := p.comments
		if comments == nil {
			comments = []prComment{}
		}
		writeJSON(w, map[string]any{"comments": comments})
	case http.MethodPost:
		if !localPost(w, r) {
			return
		}
		var body struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Side string `json:"side"`
			Body string `json:"body"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil ||
			body.Path == "" || body.Line <= 0 || strings.TrimSpace(body.Body) == "" {
			fail(w, http.StatusBadRequest, "path, line, and body are required")
			return
		}
		side := strings.ToUpper(body.Side)
		if side != "LEFT" {
			side = "RIGHT"
		}
		p.mu.Lock()
		p.nextID++
		c := prComment{ID: p.nextID, Path: body.Path, Line: body.Line, Side: side, Body: strings.TrimSpace(body.Body)}
		p.comments = append(p.comments, c)
		p.mu.Unlock()
		writeJSON(w, c)
	default:
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handlePRCommentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.comments {
		if c.ID == id {
			p.comments = append(p.comments[:i], p.comments[i+1:]...)
			break
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePRSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; review submission is read-only")
		return
	}
	var body struct {
		Event string `json:"event"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	event := strings.ToUpper(body.Event)
	if event != "APPROVE" && event != "REQUEST_CHANGES" && event != "COMMENT" {
		fail(w, http.StatusBadRequest, "event must be APPROVE, REQUEST_CHANGES, or COMMENT")
		return
	}
	if (event == "APPROVE" || event == "REQUEST_CHANGES") && !p.writeAccess {
		fail(w, http.StatusForbidden, "no push access on this repository; only Comment reviews are allowed")
		return
	}
	p.mu.Lock()
	comments := append([]prComment(nil), p.comments...)
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := p.provider.SubmitReview(ctx, p.target, p.token, p.meta.HeadSHA, comments, event, body.Body); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	p.mu.Lock()
	p.comments = nil
	p.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// handleLaunchPR lets an already-running px0 open another PR without
// disturbing its own session: it re-execs itself as a brand new process on
// a new port with "px0 -y <url>". The call returns as soon as the child starts.
func (s *Server) handleLaunchPR(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || strings.TrimSpace(body.Target) == "" {
		fail(w, http.StatusBadRequest, "target is required")
		return
	}
	targetURL := strings.TrimSpace(body.Target)
	if _, _, ok := DetectPRURL(targetURL); !ok {
		fail(w, http.StatusBadRequest, "target must be a valid pull request URL (e.g. https://github.com/owner/repo/pull/123)")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd := exec.Command(exe, "-y", targetURL)
	cmd.Dir = s.ix.Root()
	if err := cmd.Start(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	go cmd.Wait() // reap without blocking the handler
	writeJSON(w, map[string]any{"ok": true})
}
