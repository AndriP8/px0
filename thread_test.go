package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeEvent(t *testing.T) {
	evs := parseClaudeEvent(`{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Edit","input":{"file_path":"a.go"}}]}}`)
	kinds := []string{}
	for _, e := range evs {
		kinds = append(kinds, e.kind+":"+e.text)
	}
	want := "session: text:Looking. tool:Edit a.go"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if evs := parseClaudeEvent(`{"type":"result","is_error":true,"result":"bad key"}`); len(evs) != 1 || evs[0].kind != "error" || evs[0].text != "bad key" {
		t.Fatalf("error result = %+v", evs)
	}
	if evs := parseClaudeEvent("not json"); len(evs) != 0 {
		t.Fatalf("non-JSON yielded %+v", evs)
	}
}

func TestThreadArgv(t *testing.T) {
	tpl := []string{"/bin/claude", "--permission-mode", "acceptEdits", "-p", "{prompt}"}
	first := strings.Join(threadArgv("claude", tpl, "U", false), " ")
	if !strings.Contains(first, "--session-id U") || strings.Contains(first, "--resume") || !strings.Contains(first, "stream-json") {
		t.Fatalf("first turn argv = %s", first)
	}
	next := strings.Join(threadArgv("claude", tpl, "U", true), " ")
	if !strings.Contains(next, "--resume U") || strings.Contains(next, "--session-id") {
		t.Fatalf("resume argv = %s", next)
	}
	if got := strings.Join(threadArgv("cursor-agent", []string{"/bin/cursor-agent", "-p", "{prompt}"}, "C", true), " "); !strings.Contains(got, "--resume C") {
		t.Fatalf("cursor argv = %s", got)
	}
	if got := threadArgv("codex", tpl, "", false); len(got) != len(tpl) {
		t.Fatalf("unsupported harness argv changed: %v", got)
	}
	if tpl[1] != "--permission-mode" {
		t.Fatal("threadArgv mutated its template")
	}
}

func TestThreadPrompt(t *testing.T) {
	th := &thread{Path: "main.go", L1: 3, L2: 4, Snippet: "func main() {}"}
	first := threadPrompt(th, nil, "why?", false)
	if !strings.Contains(first, "@main.go lines 3-4") || !strings.Contains(first, "func main() {}") || !strings.HasSuffix(first, "why?") {
		t.Fatalf("first prompt = %q", first)
	}
	prior := []*threadTurn{{Prompt: "why?", Reply: "because", Changed: []string{"a.go"}}}
	if got := threadPrompt(th, prior, "and now?", false); got != "and now?" {
		t.Fatalf("a live session should get the bare message, got %q", got)
	}
	replay := threadPrompt(th, prior, "and now?", true)
	for _, want := range []string{"@main.go", "User: why?", "Assistant: because", "a.go", "and now?"} {
		if !strings.Contains(replay, want) {
			t.Fatalf("replay prompt missing %q:\n%s", want, replay)
		}
	}
}

func waitThreadIdle(t *testing.T, s *Server, id string) *thread {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if th := s.threads.Get(id); th != nil && !th.running() {
			return th
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("thread turn did not finish")
	return nil
}

func TestThreadConversationWithPlainHarness(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	// Echoes the prompt it was given, so the test can see what a replay carries.
	h := writeHarness(t, `printf 'saw: %s\n' "$1"`)
	s := agentServer(t, root, h)

	code, m := agentPostJSON(t, s, "/api/threads/create", map[string]any{"path": "main.go", "l1": 3, "l2": 3, "message": "what does main do?"})
	if code != 200 {
		t.Fatalf("create = %d %v", code, m)
	}
	id := m["id"].(string)
	th := waitThreadIdle(t, s, id)
	if th.Error() != "" || len(th.Turns) != 1 {
		t.Fatalf("turn 1 = %+v", th.Turns)
	}
	if !strings.Contains(th.Turns[0].Reply, "saw:") || !strings.Contains(th.Turns[0].Reply, "func main() {}") {
		t.Fatalf("reply should echo an anchored prompt, got %q", th.Turns[0].Reply)
	}

	if code, _ := agentPostJSON(t, s, "/api/threads/send", map[string]any{"id": id, "message": "and its tests?"}); code != 200 {
		t.Fatalf("send = %d", code)
	}
	th = waitThreadIdle(t, s, id)
	if len(th.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(th.Turns))
	}
	second := th.Turns[1].Reply
	if !strings.Contains(second, "what does main do?") || !strings.Contains(second, "and its tests?") {
		t.Fatalf("a harness without sessions must be replayed the transcript, got %q", second)
	}

	// Threads survive a restart, with the same transcript.
	again := newThreadManager(s.agent, root)
	got := again.Get(id)
	if got == nil || len(got.Turns) != 2 || got.Title != "what does main do?" {
		t.Fatalf("reloaded thread = %+v", got)
	}

	if !s.threads.Delete(id) || s.threads.Get(id) != nil {
		t.Fatal("delete did not remove the thread")
	}
}

func (t *thread) Error() string {
	if n := len(t.Turns); n > 0 {
		return t.Turns[n-1].Error
	}
	return ""
}

func TestThreadClaudeSessionResume(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	// A stand-in named claude: records its argv and emits stream-json.
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		`printf '{"type":"system","subtype":"init","session_id":"x"}\n'` + "\n" +
		`printf '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}\n'` + "\n" +
		`printf '{"type":"assistant","message":{"content":[{"type":"text","text":"All good."}]}}\n'` + "\n" +
		`printf '{"type":"result","is_error":false,"result":"All good."}\n'` + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := agentServer(t, root, bin)

	_, m := agentPostJSON(t, s, "/api/threads/create", map[string]any{"message": "look around"})
	id := m["id"].(string)
	th := waitThreadIdle(t, s, id)
	if th.Turns[0].Reply != "All good." || len(th.Turns[0].Tools) != 1 || th.Turns[0].Tools[0] != "Read a.go" {
		t.Fatalf("parsed turn = %+v", th.Turns[0])
	}
	agentPostJSON(t, s, "/api/threads/send", map[string]any{"id": id, "message": "again"})
	th = waitThreadIdle(t, s, id)
	if len(th.Turns) != 2 || th.Turns[1].Error != "" {
		t.Fatalf("turn 2 = %+v", th.Turns)
	}

	data, _ := os.ReadFile(log)
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "--output-format") { // the first prompt spans several lines
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("harness ran %d times, want 2:\n%s", len(lines), data)
	}
	sid := th.SessionID
	if sid == "" || !strings.Contains(lines[0], "--session-id "+sid) {
		t.Fatalf("first run should start session %q: %s", sid, lines[0])
	}
	if !strings.Contains(lines[1], "--resume "+sid) || strings.Contains(lines[1], "Earlier in this conversation") {
		t.Fatalf("second run should resume without replay: %s", lines[1])
	}
}

func TestThreadBusyAndCancel(t *testing.T) {
	root := t.TempDir()
	h := writeHarness(t, "sleep 30")
	s := agentServer(t, root, h)
	_, m := agentPostJSON(t, s, "/api/threads/create", map[string]any{"message": "slow one"})
	id := m["id"].(string)
	if code, _ := agentPostJSON(t, s, "/api/threads/send", map[string]any{"id": id, "message": "impatient"}); code != 409 {
		t.Fatalf("send while running = %d, want 409", code)
	}
	if code, _ := agentPost(t, s, "/api/threads/cancel?id="+id); code != 200 {
		t.Fatalf("cancel = %d", code)
	}
	th := waitThreadIdle(t, s, id)
	if th.Turns[0].Error != "cancelled" {
		t.Fatalf("cancelled turn error = %q", th.Turns[0].Error)
	}
}

func TestThreadInterruptedTurnIsClosedOnLoad(t *testing.T) {
	isolateSettings(t)
	root := t.TempDir()
	m, _ := newAgentManager(root, "", nil)
	tm := newThreadManager(m, root)
	tm.mu.Lock()
	tm.threads["t1"] = &thread{ID: "t1", Title: "x", Turns: []*threadTurn{{ID: 1, Prompt: "p", Running: true}}}
	tm.saveLocked()
	tm.mu.Unlock()

	got := newThreadManager(m, root).Get("t1")
	if got == nil || got.Turns[0].Running || !strings.Contains(got.Turns[0].Error, "interrupted") {
		t.Fatalf("interrupted turn = %+v", got)
	}
}
