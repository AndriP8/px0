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
	"strings"
	"time"
)

// github.go talks to the GitHub REST API for PR review (pr.go). Like git.go,
// it avoids third-party SDKs: net/http plus a shell-out to the gh CLI as one
// of three token sources, nothing more.

const githubAPIBase = "https://api.github.com"

var githubHTTPClient = &http.Client{Timeout: 15 * time.Second}

// resolveGitHubToken looks for a token in order: the explicit px0 setting
// (github.token), the GITHUB_TOKEN environment variable, then the gh CLI if
// installed and logged in. An empty return means PR review stays read-only.
func resolveGitHubToken(cfg settings) (token, source string) {
	if cfg.GitHubToken != nil {
		if t := strings.TrimSpace(*cfg.GitHubToken); t != "" {
			return t, "settings"
		}
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
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

// prMeta is the subset of a GitHub pull request px0 needs to check it out
// and label the review UI.
type prMeta struct {
	Number           int
	Title            string
	Author           string
	Draft            bool
	BaseRef          string
	HeadRef          string
	HeadSHA          string
	HeadRepoCloneURL string
	HeadIsFork       bool
}

func fetchPRMeta(ctx context.Context, owner, repo string, num int, token string) (prMeta, error) {
	resp, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, num), token, nil)
	if err != nil {
		return prMeta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return prMeta{}, fmt.Errorf("github: fetch PR #%d: %s: %s", num, resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Draft  bool   `json:"draft"`
		User   struct {
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
		return prMeta{}, err
	}
	m := prMeta{
		Number:           out.Number,
		Title:            out.Title,
		Author:           out.User.Login,
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
func submitReview(ctx context.Context, owner, repo string, num int, token string, comments []prComment, event, body string) error {
	type reviewComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	payload := struct {
		Body     string          `json:"body,omitempty"`
		Event    string          `json:"event"`
		Comments []reviewComment `json:"comments,omitempty"`
	}{Body: body, Event: event}
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
