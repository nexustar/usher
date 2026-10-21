package terminal

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeCall struct {
	in   string
	args []string
}

type fakeRunner struct {
	session bool
	windows []string
	calls   []fakeCall
}

func (f *fakeRunner) run(args ...string) (string, error) {
	return f.runStdin("", args...)
}

func (f *fakeRunner) runStdin(in string, args ...string) (string, error) {
	f.calls = append(f.calls, fakeCall{in: in, args: slices.Clone(args)})
	switch args[0] {
	case "has-session":
		if !f.session {
			return "", errors.New("no server")
		}
	case "list-windows":
		if !f.session {
			return "", errors.New("no server")
		}
		return strings.Join(f.windows, "\n") + "\n", nil
	case "new-session", "new-window":
		f.session = true
		for i, arg := range args[:len(args)-1] {
			if arg == "-n" {
				f.windows = append(f.windows, args[i+1])
				break
			}
		}
	case "kill-window":
		id := strings.TrimPrefix(args[len(args)-1], sessionName+":")
		for i, name := range f.windows {
			if name == id {
				f.windows = append(f.windows[:i], f.windows[i+1:]...)
				break
			}
		}
	case "capture-pane":
		id := strings.TrimPrefix(args[len(args)-1], sessionName+":")
		if !slices.Contains(f.windows, id) {
			return "", errors.New("can't find window " + id)
		}
		return "prompt$ ", nil
	}
	return "", nil
}

func testManager(f *fakeRunner) *Manager {
	return &Manager{runner: f, shell: "/bin/bash", available: true}
}

func TestManagerOpenBindsOneWindowPerSession(t *testing.T) {
	f := &fakeRunner{}
	m := testManager(f)
	if err := m.Open("session-a", "/work/a", 91, 27); err != nil {
		t.Fatal(err)
	}
	if !m.Has("session-a") {
		t.Fatal("opened terminal is not live")
	}
	if err := m.Open("session-a", "/different", 100, 30); err != nil {
		t.Fatal(err)
	}
	if got := len(f.windows); got != 1 {
		t.Fatalf("windows = %d, want one reused window", got)
	}

	var spawns int
	for _, call := range f.calls {
		if call.args[0] == "new-session" || call.args[0] == "new-window" {
			spawns++
			joined := strings.Join(call.args, " ")
			if !strings.Contains(joined, "-n session-a -c /work/a") || call.args[len(call.args)-1] != "'/bin/bash'" {
				t.Errorf("spawn args = %q", call.args)
			}
		}
	}
	if spawns != 1 {
		t.Errorf("spawn calls = %d, want 1", spawns)
	}
}

func TestManagerSubmitCaptureKeysAndClose(t *testing.T) {
	f := &fakeRunner{}
	m := testManager(f)
	if err := m.Open("session-a", "/work/a", 80, 24); err != nil {
		t.Fatal(err)
	}
	input := "printf '%s\\n' '你好'\necho done"
	if err := m.Submit("session-a", "request-1", input); err != nil {
		t.Fatal(err)
	}
	if err := m.Submit("session-a", "request-1", input); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Capture("session-a"); err != nil || got != "prompt$ " {
		t.Fatalf("Capture = (%q, %v)", got, err)
	}
	if err := m.SendControl("session-a", "C-c"); err != nil {
		t.Fatal(err)
	}

	var loaded bool
	var loads int
	var pasted, entered bool
	for _, call := range f.calls {
		if call.args[0] == "load-buffer" {
			loaded = true
			loads++
			if call.in != input {
				t.Errorf("load-buffer stdin = %q, want exact input", call.in)
			}
		}
		if call.args[0] == "paste-buffer" && slices.Contains(call.args, "-p") {
			pasted = true
		}
		if slices.Contains(call.args, "send-keys") && call.args[len(call.args)-1] == "Enter" {
			entered = true
		}
	}
	if !loaded {
		t.Error("Submit did not use load-buffer")
	}
	if loads != 1 {
		t.Errorf("duplicate request loaded %d times, want once", loads)
	}
	if !pasted || !entered {
		t.Errorf("Submit sequence: bracketed paste=%v enter=%v", pasted, entered)
	}
	if err := m.Close("session-a"); err != nil {
		t.Fatal(err)
	}
	if m.Has("session-a") {
		t.Error("terminal still live after Close")
	}
	if _, err := m.Capture("session-a"); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("Capture after close error = %v, want ErrNotOpen", err)
	}
}

func TestUnavailableManager(t *testing.T) {
	m := &Manager{}
	if m.Available() {
		t.Fatal("zero manager should be unavailable")
	}
	if err := m.Open("id", "/work", 80, 24); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Open error = %v, want ErrUnavailable", err)
	}
}
