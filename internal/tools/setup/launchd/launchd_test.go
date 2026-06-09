package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type recorder struct {
	calls [][]string
	err   error
}

func (r *recorder) Run(ctx context.Context, name string, args ...string) error {
	full := append([]string{name}, args...)
	r.calls = append(r.calls, full)
	return r.err
}

func newDeps(t *testing.T, home string) (*Tool, *recorder, *recorder) {
	t.Helper()
	plutil := &recorder{}
	launchctl := &recorder{}
	return New(Deps{
		Home:      func() (string, error) { return home, nil },
		GOOS:      "darwin",
		Plutil:    plutil.Run,
		Launchctl: launchctl.Run,
	}), plutil, launchctl
}

func TestTool_Metadata(t *testing.T) {
	tl, _, _ := newDeps(t, t.TempDir())
	if tl.Name() != "setup.launchd" {
		t.Fatalf("Name() = %q, want setup.launchd", tl.Name())
	}
	schema := tl.InputSchema()
	if schema == nil {
		t.Fatal("InputSchema must not be nil")
	}
	if got := schema.Required; len(got) != 1 || got[0] != "binary_path" {
		t.Fatalf("required = %v, want [binary_path]", got)
	}
}

func TestInvoke_NonDarwinReturnsClearError(t *testing.T) {
	tl := New(Deps{
		Home:      func() (string, error) { return t.TempDir(), nil },
		GOOS:      "linux",
		Plutil:    func(context.Context, string, ...string) error { return nil },
		Launchctl: func(context.Context, string, ...string) error { return nil },
	})
	_, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("error = %v, want one mentioning linux", err)
	}
}

func TestInvoke_WritesPlistAndLogsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not relevant on windows")
	}
	home := t.TempDir()
	tl, plutil, launchctl := newDeps(t, home)

	res, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	wantPath := filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.plist")
	if r.Path != wantPath {
		t.Fatalf("Path = %q, want %q", r.Path, wantPath)
	}
	body, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("plist missing: %v", err)
	}
	for _, want := range []string{"dev.murtaugh", "/usr/local/bin/murtaugh", "<string>slack</string>"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("plist missing %q in:\n%s", want, body)
		}
	}
	logsDir := filepath.Join(home, "Library", "Logs", "murtaugh")
	if _, err := os.Stat(logsDir); err != nil {
		t.Fatalf("logs dir missing: %v", err)
	}
	if len(plutil.calls) != 1 {
		t.Fatalf("plutil called %d times, want 1", len(plutil.calls))
	}
	if len(launchctl.calls) != 0 {
		t.Fatalf("launchctl should not be called when load=false; got %v", launchctl.calls)
	}
	if r.Loaded {
		t.Fatal("Loaded must be false when load is not requested")
	}
}

func TestInvoke_LoadInvokesBootoutBootstrapThenKickstart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not relevant on windows")
	}
	home := t.TempDir()
	tl, _, launchctl := newDeps(t, home)

	if _, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
		"load":        true,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(launchctl.calls) != 3 {
		t.Fatalf("launchctl calls = %v, want bootout+bootstrap+kickstart", launchctl.calls)
	}
	if launchctl.calls[0][1] != "bootout" || launchctl.calls[1][1] != "bootstrap" || launchctl.calls[2][1] != "kickstart" {
		t.Fatalf("calls order wrong: %v", launchctl.calls)
	}
	// kickstart must target the labeled service, not the bare domain, or
	// launchctl errors and the agent never spawns.
	last := launchctl.calls[2]
	if last[len(last)-1] != "gui/"+strconv.Itoa(os.Getuid())+"/dev.murtaugh" {
		t.Fatalf("kickstart target = %v, want gui/<uid>/dev.murtaugh", last)
	}
}

func TestInvoke_BootoutFailureIsToleratedBootstrapFailureSurfaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not relevant on windows")
	}
	home := t.TempDir()
	plutil := &recorder{}
	launchctl := &recorder{}
	calls := 0
	launchctlFn := func(ctx context.Context, name string, args ...string) error {
		calls++
		full := append([]string{name}, args...)
		launchctl.calls = append(launchctl.calls, full)
		if calls == 1 {
			return errors.New("nothing to boot out")
		}
		return errors.New("bootstrap failed")
	}
	tl := New(Deps{
		Home:      func() (string, error) { return home, nil },
		GOOS:      "darwin",
		Plutil:    plutil.Run,
		Launchctl: launchctlFn,
	})
	_, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
		"load":        true,
	})
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("error = %v, want bootstrap failure surfaced", err)
	}
}
