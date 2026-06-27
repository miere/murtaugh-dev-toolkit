package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type stubServer struct {
	releaseJSON []byte
	assetBody   []byte
	calls       []string
}

func (s *stubServer) get(_ context.Context, url string) ([]byte, error) {
	s.calls = append(s.calls, url)
	if strings.Contains(url, "/releases/") {
		return s.releaseJSON, nil
	}
	return s.assetBody, nil
}

func releaseFor(t *testing.T, tag string) []byte {
	t.Helper()
	doc := map[string]any{
		"tag_name": tag,
		"assets": []map[string]any{{
			"name":                 "murtaugh-" + tag + "-darwin-arm64",
			"browser_download_url": "https://example/" + tag,
		}},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newTool(t *testing.T, current string, srv *stubServer) (*Tool, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "murtaugh")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	tl := New(Deps{
		CurrentVersion: func() string { return current },
		CurrentBinary:  func() (string, error) { return bin, nil },
		GOOS:           "darwin",
		GOARCH:         "arm64",
		HTTPGet:        srv.get,
		VerifyBinary:   func(string) error { return nil },
		Owner:          "miere",
		Repo:           "murtaugh",
	})
	return tl, bin
}

func TestTool_Metadata(t *testing.T) {
	tl, _ := newTool(t, "v1.0.0", &stubServer{})
	if tl.Name() != "setup.update" {
		t.Fatalf("Name() = %q", tl.Name())
	}
	if tl.InputSchema() == nil {
		t.Fatal("InputSchema must not be nil")
	}
}

func TestInvoke_DevWithoutForceIsRefused(t *testing.T) {
	tl, _ := newTool(t, "dev", &stubServer{releaseJSON: releaseFor(t, "v2.0.0"), assetBody: []byte("new")})
	_, err := tl.Invoke(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("err = %v, want refusal mentioning dev", err)
	}
}

func TestInvoke_DevWithForceProceeds(t *testing.T) {
	srv := &stubServer{releaseJSON: releaseFor(t, "v2.0.0"), assetBody: []byte("new")}
	tl, bin := newTool(t, "dev", srv)
	res, err := tl.Invoke(context.Background(), map[string]any{"force": true})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Skipped {
		t.Fatal("Skipped must be false when force is set")
	}
	got, _ := os.ReadFile(bin)
	if string(got) != "new" {
		t.Fatalf("binary content = %q, want new", got)
	}
	if r.BackupPath == "" {
		t.Fatal("BackupPath must be set when replacing")
	}
}

func TestInvoke_SameVersionSkips(t *testing.T) {
	srv := &stubServer{releaseJSON: releaseFor(t, "v1.0.0"), assetBody: []byte("new")}
	tl, bin := newTool(t, "v1.0.0", srv)
	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Skipped {
		t.Fatal("Skipped must be true when already at target")
	}
	got, _ := os.ReadFile(bin)
	if string(got) != "old" {
		t.Fatalf("binary clobbered: %q", got)
	}
}

func TestInvoke_NewerReleaseInstallsAfterVerify(t *testing.T) {
	srv := &stubServer{releaseJSON: releaseFor(t, "v2.0.0"), assetBody: []byte("new")}
	tl, bin := newTool(t, "v1.0.0", srv)
	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Skipped || r.TargetVersion != "v2.0.0" {
		t.Fatalf("unexpected result: %+v", r)
	}
	got, _ := os.ReadFile(bin)
	if string(got) != "new" {
		t.Fatalf("binary content = %q", got)
	}
}

func TestInvoke_VerifyFailureLeavesOriginalIntact(t *testing.T) {
	srv := &stubServer{releaseJSON: releaseFor(t, "v2.0.0"), assetBody: []byte("new")}
	tl, bin := newTool(t, "v1.0.0", srv)
	tl.deps.VerifyBinary = func(string) error { return errors.New("smoke failed") }
	_, err := tl.Invoke(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error from verify failure")
	}
	got, _ := os.ReadFile(bin)
	if string(got) != "old" {
		t.Fatalf("binary changed despite verify failure: %q", got)
	}
}

func TestInvoke_AssetNotFoundForArchSurfacesError(t *testing.T) {
	doc := map[string]any{
		"tag_name": "v2.0.0",
		"assets": []map[string]any{{
			"name":                 "murtaugh-v2.0.0-linux-amd64",
			"browser_download_url": "https://example/x",
		}},
	}
	body, _ := json.Marshal(doc)
	srv := &stubServer{releaseJSON: body, assetBody: []byte("x")}
	tl, _ := newTool(t, "v1.0.0", srv)
	_, err := tl.Invoke(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "darwin-arm64") {
		t.Fatalf("err = %v, want missing-asset message", err)
	}
}
