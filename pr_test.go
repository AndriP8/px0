package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

func TestDetectPRURL(t *testing.T) {
	cases := []struct {
		arg         string
		wantOK      bool
		owner, repo string
		num         int
	}{
		{arg: "https://github.com/px0-ai/px0/pull/42", wantOK: true, owner: "px0-ai", repo: "px0", num: 42},
		{arg: "http://github.com/px0-ai/px0/pull/7", wantOK: true, owner: "px0-ai", repo: "px0", num: 7},
		{arg: "github.com/px0-ai/px0/pull/1", wantOK: true, owner: "px0-ai", repo: "px0", num: 1},
		{arg: "https://github.com/px0-ai/px0.git/pull/99", wantOK: true, owner: "px0-ai", repo: "px0", num: 99},
		// Bare numbers must NOT be accepted as PR targets
		{arg: "42", wantOK: false},
		{arg: "123", wantOK: false},
		// File paths must NOT be accepted
		{arg: "src/main.go", wantOK: false},
		{arg: ".", wantOK: false},
		// Non-PR GitHub URLs must NOT be accepted
		{arg: "https://github.com/px0-ai/px0", wantOK: false},
		{arg: "https://github.com/px0-ai/px0/issues/42", wantOK: false},
		// Invalid / empty
		{arg: "not a url", wantOK: false},
		{arg: "", wantOK: false},
	}
	for _, c := range cases {
		p, target, ok := DetectPRURL(c.arg)
		if ok != c.wantOK {
			t.Errorf("DetectPRURL(%q) ok = %v, want %v", c.arg, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if p.Name() != "github" {
			t.Errorf("DetectPRURL(%q) provider = %q, want github", c.arg, p.Name())
		}
		if target.Owner != c.owner || target.Repo != c.repo || target.Number != c.num {
			t.Errorf("DetectPRURL(%q) = (%q, %q, %d), want (%q, %q, %d)", c.arg, target.Owner, target.Repo, target.Number, c.owner, c.repo, c.num)
		}
	}
}

func TestParsePRURLUnsupported(t *testing.T) {
	if _, _, err := ParsePRURL("42"); err == nil {
		t.Error("expected error parsing bare number as PR URL")
	}
	if _, _, err := ParsePRURL("https://example.com/foo/bar"); err == nil {
		t.Error("expected error for unsupported forge URL")
	}
}

func TestResolveGitHubTokenPrecedence(t *testing.T) {
	// settings.json wins over everything, including the environment.
	t.Setenv("GITHUB_TOKEN", "env-token")
	settingsToken := "settings-token"
	token, source := resolveGitHubToken(settings{GitHubToken: &settingsToken})
	if token != "settings-token" || source != "settings" {
		t.Errorf("got (%q, %q), want (settings-token, settings)", token, source)
	}

	// With no settings token, the environment variable wins.
	token, source = resolveGitHubToken(settings{})
	if token != "env-token" || source != "env" {
		t.Errorf("got (%q, %q), want (env-token, env)", token, source)
	}
}

func TestResolveGitHubTokenNoneAvailable(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	// Force `gh` to be unresolvable so the outcome is deterministic
	// regardless of whether the test machine happens to have it installed.
	empty := t.TempDir()
	t.Setenv("PATH", empty)

	token, source := resolveGitHubToken(settings{})
	if token != "" || source != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\")", token, source)
	}
}

func TestResolveGitHubTokenBlankSettingsFallsThrough(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-token")
	blank := "   "
	token, source := resolveGitHubToken(settings{GitHubToken: &blank})
	if token != "env-token" || source != "env" {
		t.Errorf("a blank settings token must fall through to the env var; got (%q, %q)", token, source)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestSubmitReviewPayload(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()

	var capturedBody []byte
	var capturedPath string
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		capturedPath = req.URL.Path
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     make(http.Header),
		}, nil
	})

	ctx := context.Background()
	comments := []prComment{
		{ID: 1, Path: "main.go", Line: 10, Side: "RIGHT", Body: "looks good"},
		{ID: 2, Path: "old.go", Line: 5, Side: "LEFT", Body: "deleted line comment"},
	}
	err := submitReview(ctx, "px0-ai", "px0", 42, "dummy-token", "abc123sha", comments, "APPROVE", "Overall LGTM")
	if err != nil {
		t.Fatalf("submitReview failed: %v", err)
	}
	if capturedPath != "/repos/px0-ai/px0/pulls/42/reviews" {
		t.Errorf("unexpected path: %q", capturedPath)
	}
	var payload struct {
		CommitID string `json:"commit_id"`
		Body     string `json:"body"`
		Event    string `json:"event"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Side string `json:"side"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("failed to parse captured payload: %v", err)
	}
	if payload.CommitID != "abc123sha" {
		t.Errorf("payload.CommitID = %q, want abc123sha", payload.CommitID)
	}
	if payload.Event != "APPROVE" || payload.Body != "Overall LGTM" {
		t.Errorf("unexpected event or body: %+v", payload)
	}
	if len(payload.Comments) != 2 || payload.Comments[1].Side != "LEFT" {
		t.Errorf("unexpected comments in payload: %+v", payload.Comments)
	}
}

func TestPRSessionCloseRefCleanup(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.name", "test")
	run("config", "user.email", "test@test.local")
	run("commit", "--allow-empty", "-m", "initial")

	run("update-ref", "refs/px0/pr/99", "HEAD")
	run("update-ref", "refs/px0/base/99", "HEAD")

	tmpWT := t.TempDir()
	p := &prSession{
		srcRepo:  root,
		worktree: tmpWT,
		meta:     PRMeta{Number: 99},
	}
	p.Close()

	checkRef := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/px0/pr/99")
	if err := checkRef.Run(); err == nil {
		t.Errorf("refs/px0/pr/99 was not deleted on Close")
	}
	checkBase := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/px0/base/99")
	if err := checkBase.Run(); err == nil {
		t.Errorf("refs/px0/base/99 was not deleted on Close")
	}
}

func TestFetchPRMetaMerged(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()

	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := `{
			"number": 55,
			"title": "Fix login race condition",
			"state": "closed",
			"merged": true,
			"merged_at": "2026-09-20T10:00:00Z",
			"user": {"login": "alice"},
			"base": {"ref": "main"},
			"head": {
				"ref": "fix-race",
				"sha": "fedcba987654",
				"repo": {"clone_url": "https://github.com/alice/px0.git", "full_name": "alice/px0"}
			}
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	meta, err := fetchPRMeta(context.Background(), "px0-ai", "px0", 55, "")
	if err != nil {
		t.Fatalf("fetchPRMeta failed: %v", err)
	}
	if !meta.Merged {
		t.Errorf("meta.Merged = false, want true")
	}
	if meta.State != "closed" {
		t.Errorf("meta.State = %q, want closed", meta.State)
	}
	if meta.MergedAt != "2026-09-20T10:00:00Z" {
		t.Errorf("meta.MergedAt = %q, want timestamp", meta.MergedAt)
	}
}

func TestCheckoutPRMergedConfirmationDecline(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()

	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := `{
			"number": 77,
			"title": "Already merged PR",
			"state": "closed",
			"merged": true,
			"merged_at": "2026-09-21T08:00:00Z",
			"user": {"login": "bob"},
			"base": {"ref": "main"},
			"head": {
				"ref": "feature-x",
				"sha": "1234567890ab",
				"repo": {"clone_url": "https://github.com/px0-ai/px0.git", "full_name": "px0-ai/px0"}
			}
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	called := false
	target := PRTarget{Provider: "github", Owner: "px0-ai", Repo: "px0", Number: 77}
	_, err := checkoutPR(context.Background(), &GitHubProvider{}, target, t.TempDir(), nil, func(meta PRMeta) (bool, error) {
		called = true
		if meta.Number != 77 || !meta.Merged {
			t.Errorf("unexpected meta in onMerged: %+v", meta)
		}
		return false, nil // user declines
	})

	if !called {
		t.Fatal("onMerged callback was not invoked for merged PR")
	}
	if !errors.Is(err, ErrPRMergedCancelled) {
		t.Errorf("err = %v, want ErrPRMergedCancelled", err)
	}
}

func TestGitHubProviderInterface(t *testing.T) {
	var gp GitProvider = &GitHubProvider{}
	if gp.Name() != "github" {
		t.Errorf("gp.Name() = %q, want github", gp.Name())
	}
	if !gp.MatchURL("https://github.com/foo/bar/pull/12") {
		t.Error("MatchURL should be true for valid github PR URL")
	}
	if gp.MatchURL("https://gitlab.com/foo/bar/-/merge_requests/12") {
		t.Error("MatchURL should be false for gitlab URL")
	}
	tgt, err := gp.ParseURL("https://github.com/foo/bar.git/pull/99")
	if err != nil {
		t.Fatalf("ParseURL failed: %v", err)
	}
	if tgt.Owner != "foo" || tgt.Repo != "bar" || tgt.Number != 99 || tgt.Provider != "github" {
		t.Errorf("unexpected target: %+v", tgt)
	}
}


