package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func gitInstalled() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// gitRepo builds a real repository under a fresh temp dir: keep/mod/del/ren are
// committed, then the tree is dirtied (modify, stage-new, untrack, delete,
// rename) so every status code is exercised. Returns the served root.
func gitRepo(tb testing.TB) string {
	tb.Helper()
	root := tb.TempDir()
	// macOS TempDir lives under /var -> /private/var; git reports the real path.
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write("keep.go", "keep\n")
	write("sub/mod.go", "line one\n")
	write("del.go", "del\n")
	write("sub/ren.go", "old\n")
	run("init")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false")
	run("add", "-A")
	run("commit", "-qm", "init")
	// Dirty it: modify, stage a new file, leave one untracked, delete, rename.
	write("sub/mod.go", "line two\n")
	write("add.go", "added\n")
	run("add", "add.go")
	write("untr.go", "untracked\n")
	os.Remove(filepath.Join(root, "del.go"))
	run("mv", "sub/ren.go", "sub/ren2.go")
	return root
}

func TestGitStatus(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)

	if !gitAvailable(root) {
		t.Fatal("gitAvailable false for a real repo")
	}
	st := gitStatus(root)
	want := map[string]string{
		"sub/mod.go":  "M",
		"add.go":      "A",
		"untr.go":     "U",
		"del.go":      "D",
		"sub/ren2.go": "R",
	}
	for path, code := range want {
		if st[path] != code {
			t.Errorf("status[%q] = %q, want %q (full: %v)", path, st[path], code, st)
		}
	}
	if _, ok := st["keep.go"]; ok {
		t.Errorf("keep.go should have no status, got %q", st["keep.go"])
	}

	// Overlay onto tree nodes. Deleted/old-rename paths have no node on disk.
	ix := NewIndex(root)
	ix.Build()
	byName := map[string]Node{}
	kids, _ := ix.Children("")
	for _, k := range kids {
		byName[k.Name] = k
	}
	sub, _ := ix.Children("sub")
	for _, k := range sub {
		byName[k.Name] = k
	}
	nodeWant := map[string]string{
		"mod.go":  "M",
		"add.go":  "A",
		"untr.go": "U",
		"ren2.go": "R",
		"keep.go": "",
	}
	for name, code := range nodeWant {
		if byName[name].Status != code {
			t.Errorf("node %q status = %q, want %q", name, byName[name].Status, code)
		}
	}
	// The sub/ dir holds changed files, so its (collapsed) folder node is dirty.
	if !byName["sub"].Dir || !byName["sub"].Dirty {
		t.Errorf("sub node = %+v, want dir with Dirty=true", byName["sub"])
	}
}

func TestGitDiff(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)

	code, body := get(t, s, "/api/diff?path=sub/mod.go")
	if code != 200 {
		t.Fatalf("diff status %d", code)
	}
	if body["available"] != true {
		t.Fatalf("available = %v, want true (%v)", body["available"], body)
	}
	diff := body["diff"].(string)
	if !strings.Contains(diff, "-line one") || !strings.Contains(diff, "+line two") {
		t.Errorf("diff missing expected +/- lines:\n%s", diff)
	}

	// A clean, committed file yields no diff -> available:false.
	_, body = get(t, s, "/api/diff?path=keep.go")
	if body["available"] != false || body["diff"] != "" {
		t.Errorf("clean file: available=%v diff=%q, want false/empty", body["available"], body["diff"])
	}

	// Meta reports git availability and git changes.
	_, meta := get(t, s, "/api/meta")
	if meta["git"] != true {
		t.Errorf("meta git = %v, want true", meta["git"])
	}
	if changes, ok := meta["gitChanges"].(float64); !ok || changes < 1 {
		t.Errorf("meta gitChanges = %v, want >= 1", meta["gitChanges"])
	}
	files, ok := meta["gitFiles"].([]any)
	found := false
	for _, f := range files {
		if f == "sub/mod.go" {
			found = true
			break
		}
	}
	if !ok || !found {
		t.Errorf("meta gitFiles = %v, want to contain sub/mod.go", meta["gitFiles"])
	}
}

func TestGitCleanRepo(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runCmd := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runCmd("init")
	runCmd("config", "user.email", "test@test.com")
	runCmd("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "clean.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd("add", "clean.go")
	runCmd("commit", "-m", "init")

	ix := NewIndex(dir)
	ix.Build()
	s := NewServer(ix, nil)

	_, meta := get(t, s, "/api/meta")
	if meta["git"] != true {
		t.Errorf("git = %v, want true", meta["git"])
	}
	if changes, ok := meta["gitChanges"].(float64); !ok || changes != 0 {
		t.Errorf("gitChanges = %v, want 0", meta["gitChanges"])
	}
	files, ok := meta["gitFiles"].([]any)
	if !ok || len(files) != 0 {
		t.Errorf("gitFiles = %v, want []", meta["gitFiles"])
	}
}

func TestGitDisabled(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	gitDisabled = true
	defer func() { gitDisabled = false }()

	root := gitRepo(t)
	if gitAvailable(root) {
		t.Fatal("gitAvailable true with -no-git")
	}
	if st := gitStatus(root); st != nil {
		t.Errorf("gitStatus returned %v with -no-git", st)
	}
	ix := NewIndex(root)
	ix.Build()
	kids, _ := ix.Children("sub")
	for _, k := range kids {
		if k.Status != "" {
			t.Errorf("node %q has status %q with -no-git", k.Name, k.Status)
		}
	}
	s := NewServer(ix, nil)
	_, body := get(t, s, "/api/diff?path=sub/mod.go")
	if body["available"] != false {
		t.Errorf("diff available = %v with -no-git, want false", body["available"])
	}
	_, meta := get(t, s, "/api/meta")
	if meta["git"] != false {
		t.Errorf("meta git = %v with -no-git, want false", meta["git"])
	}
	if meta["gitChanges"].(float64) != 0 {
		t.Errorf("meta gitChanges = %v with -no-git, want 0", meta["gitChanges"])
	}
}

func TestGitGutter(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Committed baseline.
	write("f.go", "alpha\nbravo\ncharlie\ndelta\necho\nfoxtrot\ngolf\nhotel\nindia\njuliet\n")
	write("clean.go", "stable\n")
	run("init")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false")
	run("add", "-A")
	run("commit", "-qm", "init")
	// Dirty f.go: replace line 2 (modify), insert a line before echo (pure add),
	// remove golf (pure delete). Leave clean.go untouched, add an untracked file.
	write("f.go", "alpha\nBRAVO\ncharlie\ndelta\nNEWLINE1\necho\nfoxtrot\nhotel\nindia\njuliet\n")
	write("untr.go", "new\n")

	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)

	ints := func(body map[string]any, key string) []int {
		var out []int
		for _, v := range body[key].([]any) {
			out = append(out, int(v.(float64)))
		}
		return out
	}

	code, body := get(t, s, "/api/gutter?path=f.go")
	if code != 200 {
		t.Fatalf("gutter status %d", code)
	}
	if body["available"] != true {
		t.Fatalf("available = %v, want true (%v)", body["available"], body)
	}
	if got := ints(body, "modified"); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("modified = %v, want [2]", got)
	}
	if got := ints(body, "added"); !reflect.DeepEqual(got, []int{5}) {
		t.Errorf("added = %v, want [5]", got)
	}
	if got := ints(body, "deleted"); !reflect.DeepEqual(got, []int{7}) {
		t.Errorf("deleted = %v, want [7] (marker on new line just before removed run)", got)
	}

	// A clean committed file: 200, available:false, empty (non-null) arrays.
	_, body = get(t, s, "/api/gutter?path=clean.go")
	if body["available"] != false {
		t.Errorf("clean file available = %v, want false", body["available"])
	}
	for _, key := range []string{"added", "modified", "deleted"} {
		if got := ints(body, key); len(got) != 0 {
			t.Errorf("clean file %s = %v, want empty", key, got)
		}
	}

	// An untracked file has no diff against HEAD -> available:false, never 500.
	code, body = get(t, s, "/api/gutter?path=untr.go")
	if code != 200 || body["available"] != false {
		t.Errorf("untracked: status=%d available=%v, want 200/false", code, body["available"])
	}

	// Verify /api/file also reports diffAvailable immediately on file load.
	_, fileBody := get(t, s, "/api/file?path=f.go")
	if fileBody["diffAvailable"] != true {
		t.Errorf("f.go diffAvailable = %v, want true", fileBody["diffAvailable"])
	}
	_, cleanBody := get(t, s, "/api/file?path=clean.go")
	if cleanBody["diffAvailable"] != false {
		t.Errorf("clean.go diffAvailable = %v, want false", cleanBody["diffAvailable"])
	}
}

func BenchmarkGitStatus(b *testing.B) {
	if !gitInstalled() {
		b.Skip("git not installed")
	}
	root := gitRepo(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gitStatus(root)
	}
}

func TestUpdateGitStatus(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	ix := NewIndex(root)
	ix.Build()

	count, files, changed, statuses, dirtyDirs, _ := ix.UpdateGitStatus()
	// Should be unchanged because Build() just ran
	if changed {
		t.Errorf("expected changed=false immediately after Build(), got true")
	}
	if count == 0 || len(files) == 0 {
		t.Errorf("expected non-zero git changes, got count=%d, files=%v", count, files)
	}
	if !dirtyDirs["sub"] {
		t.Errorf("expected 'sub' to be in dirtyDirs, got %v", dirtyDirs)
	}
	if statuses["add.go"] != "A" {
		t.Errorf("expected add.go status 'A', got %q", statuses["add.go"])
	}

	// Now modify another file
	if err := os.WriteFile(filepath.Join(root, "keep.go"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	count2, _, changed2, statuses2, _, _ := ix.UpdateGitStatus()
	if !changed2 {
		t.Errorf("expected changed=true after modifying keep.go")
	}
	if count2 <= count {
		t.Errorf("expected count to increase, got count2=%d vs count=%d", count2, count)
	}
	if statuses2["keep.go"] != "M" {
		t.Errorf("expected keep.go to have status 'M', got %q", statuses2["keep.go"])
	}

	// Calling it again without changes should report changed=false
	_, _, changed3, _, _, _ := ix.UpdateGitStatus()
	if changed3 {
		t.Errorf("expected changed=false when worktree has not changed")
	}
}

func TestGitWatcherStreamAndRefresh(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	ix := NewIndex(root)
	ix.Build()

	s := NewServer(ix, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	// 1. Connect to SSE stream
	req, err := http.NewRequest("GET", ts.URL+"/api/git/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	readSSEEvent := func() (string, map[string]any) {
		var eventName string
		var data string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("failed reading SSE stream: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "event: ") {
				eventName = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			} else if line == "" && data != "" {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(data), &parsed); err != nil {
					t.Fatalf("malformed json in SSE event: %v", err)
				}
				return eventName, parsed
			}
		}
	}

	// Initial status should be sent immediately upon connection
	evName, initialData := readSSEEvent()
	if evName != "git-status" {
		t.Errorf("expected event 'git-status', got %q", evName)
	}
	if initialData["git"] != true {
		t.Errorf("expected git=true, got %v", initialData["git"])
	}

	// 2. Modify a file and call POST /api/git/refresh
	if err := os.WriteFile(filepath.Join(root, "keep.go"), []byte("streamed change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	refreshResp, err := http.Post(ts.URL+"/api/git/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var refreshData map[string]any
	json.NewDecoder(refreshResp.Body).Decode(&refreshData)
	refreshResp.Body.Close()

	if refreshData["git"] != true {
		t.Errorf("refresh expected git=true, got %v", refreshData["git"])
	}
	statuses, ok := refreshData["statuses"].(map[string]any)
	if !ok || statuses["keep.go"] != "M" {
		t.Errorf("expected keep.go 'M' in refresh response, got %v", refreshData)
	}

	// 3. SSE stream must receive the broadcasted update
	done := make(chan struct{})
	var streamedEv string
	var streamedData map[string]any
	go func() {
		streamedEv, streamedData = readSSEEvent()
		close(done)
	}()

	select {
	case <-done:
		if streamedEv != "git-status" {
			t.Errorf("expected event 'git-status', got %q", streamedEv)
		}
		streamedStatuses, _ := streamedData["statuses"].(map[string]any)
		if streamedStatuses["keep.go"] != "M" {
			t.Errorf("expected keep.go 'M' on stream, got %v", streamedData)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE update event")
	}
}

func TestGitWatcherCLICommitDetection(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	ix := NewIndex(root)
	ix.Build()

	gw := NewGitWatcher(ix)
	gitdir := gitDir(root)
	if !gw.recordGitMeta(gitdir) {
		// First recording initialized the metadata
	}

	// Commit existing changes via CLI
	cmd := exec.Command("git", "commit", "-am", "commit all")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v\n%s", err, out)
	}

	// Watcher's metadata stat check should instantly see the index/HEAD modtime update
	if !gw.recordGitMeta(gitdir) {
		t.Errorf("expected recordGitMeta to report changed=true after CLI commit")
	}
}

func TestGitStatusAgainstAndPRDiff(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("foo.go", "package main\n\nfunc Foo() int { return 1 }\n")
	run("init")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false")
	run("add", "-A")
	run("commit", "-m", "initial commit")
	baseSHA := run("rev-parse", "HEAD")

	// Commit a PR change: modifies foo.go
	write("foo.go", "package main\n\nfunc Foo() int { return 2 }\n")
	run("commit", "-am", "pr commit")

	// Working tree is clean relative to HEAD
	stHead := gitStatus(root)
	if stHead != nil && stHead["foo.go"] != "" {
		t.Fatalf("expected gitStatus(root) to be clean, got: %v", stHead)
	}

	// But gitStatusAgainst baseSHA must report foo.go as "M"
	stBase := gitStatusAgainst(root, baseSHA)
	if stBase == nil || stBase["foo.go"] != "M" {
		t.Fatalf("expected gitStatusAgainst(root, baseSHA) to report foo.go as M, got: %v", stBase)
	}

	// Index should also pick up the diffBase
	ix := NewIndex(root)
	ix.SetDiffBase(baseSHA)
	ix.Build()

	count, files, _, statuses, _, _ := ix.UpdateGitStatus()
	if count == 0 || statuses["foo.go"] != "M" {
		t.Fatalf("expected Index to report foo.go as M against diffBase, got count=%d statuses=%v files=%v", count, statuses, files)
	}

	// Server should mark diffAvailable=true for foo.go
	srv := NewServer(ix, newLSPManager(root, false))
	srv.diffBase = baseSHA

	req := httptest.NewRequest("GET", "/api/file?path=foo.go", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var fileResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &fileResp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if diffAvail, ok := fileResp["diffAvailable"].(bool); !ok || !diffAvail {
		t.Errorf("expected diffAvailable=true for foo.go against baseSHA, got %v", fileResp["diffAvailable"])
	}

	// /api/diff should return the diff against baseSHA
	diffReq := httptest.NewRequest("GET", "/api/diff?path=foo.go", nil)
	dw := httptest.NewRecorder()
	srv.ServeHTTP(dw, diffReq)
	if dw.Code != 200 {
		t.Fatalf("expected 200 from /api/diff, got %d: %s", dw.Code, dw.Body.String())
	}
	diffBody := dw.Body.String()
	if !strings.Contains(diffBody, "-func Foo() int { return 1 }") || !strings.Contains(diffBody, "+func Foo() int { return 2 }") {
		t.Errorf("unexpected diff against baseSHA:\n%s", diffBody)
	}

	// Now simulate an external terminal agent modifying foo.go
	write("foo.go", "package main\n\nfunc Foo() int { return 99 }\n")

	// ix.UpdateGitStatus() must catch the modification
	_, _, changed, statuses, _, _ := ix.UpdateGitStatus()
	if !changed && statuses["foo.go"] != "M" {
		t.Errorf("expected UpdateGitStatus to report foo.go as changed/M, got changed=%v statuses=%v", changed, statuses)
	}

	// Server /api/diff against baseSHA must reflect the external edit in real time
	diffReq2 := httptest.NewRequest("GET", "/api/diff?path=foo.go", nil)
	dw2 := httptest.NewRecorder()
	srv.ServeHTTP(dw2, diffReq2)
	if dw2.Code != 200 {
		t.Fatalf("expected 200 from /api/diff, got %d: %s", dw2.Code, dw2.Body.String())
	}
	diffBody2 := dw2.Body.String()
	if !strings.Contains(diffBody2, "-func Foo() int { return 1 }") || !strings.Contains(diffBody2, "+func Foo() int { return 99 }") {
		t.Errorf("unexpected diff against baseSHA after external edit:\n%s", diffBody2)
	}
}

func TestGitStagedPaths(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	staged := gitStagedPaths(root)
	for _, p := range []string{"add.go", "sub/ren2.go"} {
		if !staged[p] {
			t.Errorf("expected %q to be staged, got %v", p, staged)
		}
	}
	for _, p := range []string{"sub/mod.go", "del.go", "untr.go"} {
		if staged[p] {
			t.Errorf("expected %q to not be staged, got %v", p, staged)
		}
	}
}

func TestGitStageUnstageCommit(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)

	// gitRepo leaves add.go staged, and sub/ren.go -> sub/ren2.go staged as a
	// rename (both the old and new path are index entries). Unstage all three
	// so this test controls what's staged from here.
	for _, p := range []string{"add.go", "sub/ren.go", "sub/ren2.go"} {
		if err := gitUnstage(root, p); err != nil {
			t.Fatalf("gitUnstage %s: %v", p, err)
		}
	}
	if staged := gitStagedPaths(root); len(staged) != 0 {
		t.Fatalf("expected nothing staged after unstaging, got %v", staged)
	}

	if err := gitCommit(root, "should fail"); err == nil {
		t.Fatal("expected gitCommit to fail with nothing staged")
	}

	if err := gitStage(root, "add.go"); err != nil {
		t.Fatalf("gitStage add.go: %v", err)
	}
	if staged := gitStagedPaths(root); !staged["add.go"] {
		t.Fatalf("expected add.go staged after gitStage, got %v", staged)
	}

	if err := gitCommit(root, "commit add.go"); err != nil {
		t.Fatalf("gitCommit: %v", err)
	}
	if staged := gitStagedPaths(root); staged["add.go"] {
		t.Fatalf("expected add.go no longer staged after commit, got %v", staged)
	}
	if !gitHasUncommittedChanges(root) {
		t.Fatal("expected the repo's other dirty files to still show uncommitted changes")
	}
}

// gitTestRun runs a git command in dir with a hermetic config, failing the
// test on error. Mirrors gitRepo's own run() closure for tests that need
// more than one working tree.
func gitTestRun(tb testing.TB, dir string, args ...string) string {
	tb.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// TestGitFFOnlyPull exercises gitFFOnlyPull against a bare "remote" shared by
// two clones: a clean fast-forward must succeed, and a diverged history (a
// local commit in cloneB that the remote doesn't have) must be refused with
// errNotFastForward, leaving cloneB's tree untouched.
func TestGitFFOnlyPull(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	remote := filepath.Join(base, "remote.git")
	cloneA := filepath.Join(base, "a")
	cloneB := filepath.Join(base, "b")

	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, remote, "init", "--bare", "-b", "main")

	gitTestRun(t, base, "clone", remote, "a")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, cloneA, "config", cfg[0], cfg[1])
	}
	if err := os.WriteFile(filepath.Join(cloneA, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, cloneA, "add", "f.txt")
	gitTestRun(t, cloneA, "commit", "-qm", "init")
	gitTestRun(t, cloneA, "push", "origin", "main")

	gitTestRun(t, base, "clone", remote, "b")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, cloneB, "config", cfg[0], cfg[1])
	}

	// cloneA pushes a second commit; cloneB fast-forward-pulls it cleanly.
	if err := os.WriteFile(filepath.Join(cloneA, "f.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, cloneA, "commit", "-aqm", "second")
	gitTestRun(t, cloneA, "push", "origin", "main")

	if err := gitFFOnlyPull(cloneB, "origin", "main"); err != nil {
		t.Fatalf("expected a clean fast-forward pull, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(cloneB, "f.txt")); err != nil || string(got) != "two\n" {
		t.Fatalf("expected cloneB to fast-forward to %q, got %q, err=%v", "two\n", got, err)
	}

	// cloneB commits locally without pushing, then cloneA pushes again:
	// cloneB can no longer fast-forward and must refuse rather than merge.
	if err := os.WriteFile(filepath.Join(cloneB, "g.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, cloneB, "add", "g.txt")
	gitTestRun(t, cloneB, "commit", "-qm", "local only")

	if err := os.WriteFile(filepath.Join(cloneA, "f.txt"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, cloneA, "commit", "-aqm", "third")
	gitTestRun(t, cloneA, "push", "origin", "main")

	if err := gitFFOnlyPull(cloneB, "origin", "main"); !errors.Is(err, errNotFastForward) {
		t.Fatalf("expected errNotFastForward for a diverged pull, got %v", err)
	}
	// Must not have touched cloneB's working tree on refusal.
	if got, err := os.ReadFile(filepath.Join(cloneB, "f.txt")); err != nil || string(got) != "two\n" {
		t.Fatalf("expected cloneB's f.txt untouched by the refused pull, got %q, err=%v", got, err)
	}
}
