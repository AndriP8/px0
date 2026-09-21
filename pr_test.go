package main

import (
	"os/exec"
	"testing"
)

func TestParsePRTargetURL(t *testing.T) {
	cases := []struct {
		arg         string
		owner, repo string
		num         int
		wantErr     bool
	}{
		{arg: "https://github.com/px0-ai/px0/pull/42", owner: "px0-ai", repo: "px0", num: 42},
		{arg: "http://github.com/px0-ai/px0/pull/7", owner: "px0-ai", repo: "px0", num: 7},
		{arg: "github.com/px0-ai/px0/pull/1", owner: "px0-ai", repo: "px0", num: 1},
		{arg: "not a url", wantErr: true},
		{arg: "", wantErr: true},
	}
	for _, c := range cases {
		owner, repo, num, err := parsePRTarget(c.arg, ".")
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePRTarget(%q): expected error, got owner=%q repo=%q num=%d", c.arg, owner, repo, num)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parsePRTarget(%q): unexpected error: %v", c.arg, err)
		}
		if owner != c.owner || repo != c.repo || num != c.num {
			t.Errorf("parsePRTarget(%q) = (%q, %q, %d), want (%q, %q, %d)", c.arg, owner, repo, num, c.owner, c.repo, c.num)
		}
	}
}

func TestIsGitHubPRURL(t *testing.T) {
	if !isGitHubPRURL("https://github.com/px0-ai/px0/pull/42") {
		t.Error("expected a github.com pull URL to be recognized")
	}
	if isGitHubPRURL("42") {
		t.Error("a bare number must not be treated as a URL target")
	}
	if isGitHubPRURL("src/main.go") {
		t.Error("a file path must not be treated as a PR target")
	}
}

func TestParsePRTargetBareNumber(t *testing.T) {
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
	run("remote", "add", "origin", "git@github.com:px0-ai/px0.git")

	owner, repo, num, err := parsePRTarget("123", root)
	if err != nil {
		t.Fatalf("parsePRTarget(123, %s): %v", root, err)
	}
	if owner != "px0-ai" || repo != "px0" || num != 123 {
		t.Errorf("parsePRTarget(123) = (%q, %q, %d), want (px0-ai, px0, 123)", owner, repo, num)
	}
}

func TestParsePRTargetBareNumberNoOrigin(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if _, _, _, err := parsePRTarget("123", root); err == nil {
		t.Error("expected an error when there is no origin remote")
	}
}

func TestParsePRTargetBareNumberNonGitHubOrigin(t *testing.T) {
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
	run("remote", "add", "origin", "https://gitlab.com/px0-ai/px0.git")
	if _, _, _, err := parsePRTarget("123", root); err == nil {
		t.Error("expected an error for a non-github.com origin")
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
