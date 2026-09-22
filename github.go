package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// github.go talks to the GitHub REST API for PR review (pr.go) via the
// GitHubProvider implementation of GitProvider. Like git.go, it avoids
// third-party SDKs: net/http plus a shell-out to the gh CLI as one of three
// token sources, nothing more.

const githubAPIBase = "https://api.github.com"

var githubHTTPClient = &http.Client{Timeout: 15 * time.Second}

var githubPRURLRe = regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// GitHubProvider implements GitProvider for GitHub.
type GitHubProvider struct{}

func (g *GitHubProvider) Name() string { return "github" }

func (g *GitHubProvider) MatchURL(rawURL string) bool {
	return githubPRURLRe.MatchString(strings.TrimSpace(rawURL))
}

func (g *GitHubProvider) ParseURL(rawURL string) (PRTarget, error) {
	m := githubPRURLRe.FindStringSubmatch(strings.TrimSpace(rawURL))
	if m == nil {
		return PRTarget{}, fmt.Errorf("invalid GitHub pull request URL: %q (expected format https://github.com/owner/repo/pull/123)", rawURL)
	}
	n, _ := strconv.Atoi(m[3])
	return PRTarget{
		Provider: "github",
		Owner:    m[1],
		Repo:     strings.TrimSuffix(m[2], ".git"),
		Number:   n,
		URL:      rawURL,
	}, nil
}

func (g *GitHubProvider) ResolveToken(cfg settings) (token, source string) {
	return resolveGitHubToken(cfg)
}

func (g *GitHubProvider) FetchPR(ctx context.Context, target PRTarget, token string) (PRMeta, error) {
	return fetchPRMeta(ctx, target.Owner, target.Repo, target.Number, token)
}

func (g *GitHubProvider) CheckPushAccess(ctx context.Context, target PRTarget, token string) bool {
	return checkPushAccess(ctx, target.Owner, target.Repo, token)
}

func (g *GitHubProvider) SubmitReview(ctx context.Context, target PRTarget, token, headSHA string, comments []prComment, event, body string) error {
	return submitReview(ctx, target.Owner, target.Repo, target.Number, token, headSHA, comments, event, body)
}

// resolveGitHubToken looks for a token in order: the explicit px0 setting
// (github.token), the GITHUB_TOKEN environment variable, GH_TOKEN, then the gh CLI
// if installed and logged in. An empty return means PR review stays read-only.
func resolveGitHubToken(cfg settings) (token, source string) {
	if cfg.GitHubToken != nil {
		if t := strings.TrimSpace(*cfg.GitHubToken); t != "" {
			return t, "settings"
		}
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, "env"
	}
	if t := strings.TrimSpace(os.Getenv("GH_TOKEN")); t != "" {
		return t, "env"
	}
	if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t, "gh"
		}
	}
	return "", ""
}

// githubRequest issues an authenticated (if token != "") GitHub REST call.
func githubRequest(ctx context.Context, method, path, token string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, githubAPIBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return githubHTTPClient.Do(req)
}

// checkPushAccess reports whether token has push access to owner/repo. Fails
// closed: any transport error, non-200, or missing field yields false, never
// true, so a broken check can only ever hide the write UI, not expose it.
func checkPushAccess(ctx context.Context, owner, repo, token string) bool {
	if token == "" {
		return false
	}
	resp, err := githubRequest(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, token, nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var out struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return out.Permissions.Push
}

func fetchPRMeta(ctx context.Context, owner, repo string, num int, token string) (PRMeta, error) {
	resp, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, num), token, nil)
	if err != nil {
		return PRMeta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		bodyMsg := strings.TrimSpace(string(b))
		if token == "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
			return PRMeta{}, fmt.Errorf("github: fetch PR #%d: %s (no GitHub token found; for private repos or rate limits, set GITHUB_TOKEN or run 'gh auth login'): %s", num, resp.Status, bodyMsg)
		}
		return PRMeta{}, fmt.Errorf("github: fetch PR #%d: %s: %s", num, resp.Status, bodyMsg)
	}
	var out struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		State    string `json:"state"`
		Merged   bool   `json:"merged"`
		MergedAt string `json:"merged_at"`
		Draft    bool   `json:"draft"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				CloneURL string `json:"clone_url"`
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PRMeta{}, err
	}
	m := PRMeta{
		Number:           out.Number,
		Title:            out.Title,
		Author:           out.User.Login,
		State:            out.State,
		Merged:           out.Merged || out.MergedAt != "",
		MergedAt:         out.MergedAt,
		Draft:            out.Draft,
		BaseRef:          out.Base.Ref,
		HeadRef:          out.Head.Ref,
		HeadSHA:          out.Head.SHA,
		HeadRepoCloneURL: out.Head.Repo.CloneURL,
	}
	m.HeadIsFork = out.Head.Repo.FullName != "" && !strings.EqualFold(out.Head.Repo.FullName, owner+"/"+repo)
	return m, nil
}

// prComment is a review comment held in memory only for the life of the
// process (pr.go's prSession) until submitReview posts it. Side matches
// GitHub's review-comment API: "LEFT" (the base) or "RIGHT" (the PR head).
type prComment struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// submitReview posts one review carrying every draft comment plus an overall
// verdict in a single call, mirroring GitHub's own draft-then-submit model
// so px0 never needs a per-comment endpoint.
func submitReview(ctx context.Context, owner, repo string, num int, token, commitID string, comments []prComment, event, body string) error {
	type reviewComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	payload := struct {
		CommitID string          `json:"commit_id,omitempty"`
		Body     string          `json:"body,omitempty"`
		Event    string          `json:"event"`
		Comments []reviewComment `json:"comments,omitempty"`
	}{CommitID: commitID, Body: body, Event: event}
	for _, c := range comments {
		payload.Comments = append(payload.Comments, reviewComment{Path: c.Path, Line: c.Line, Side: c.Side, Body: c.Body})
	}
	resp, err := githubRequest(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, num), token, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github: submit review: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
