package journal

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestBlobStoreAppendsTurns(t *testing.T) {
	dir := t.TempDir()
	b := NewBlobStore(dir)

	ref, err := b.AppendTranscript("sess-1", TranscriptTurn{Time: time.Now(), Outcome: "completed", Prompt: "hi", Response: "hello"})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if ref != "sess-1.ndjson" {
		t.Fatalf("ref = %q, want sess-1.ndjson", ref)
	}
	if _, err := b.AppendTranscript("sess-1", TranscriptTurn{Time: time.Now(), Outcome: "completed", Prompt: "more", Response: "ok"}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	// Two appended turns → two NDJSON lines in the same file.
	f, err := os.Open(filepath.Join(dir, ref))
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines++
	}
	if lines != 2 {
		t.Fatalf("transcript has %d lines, want 2", lines)
	}
}

func TestBlobStoreNoopWithoutDir(t *testing.T) {
	ref, err := NewBlobStore("").AppendTranscript("s", TranscriptTurn{Outcome: "completed"})
	if err != nil || ref != "" {
		t.Fatalf("empty-dir store should no-op, got ref=%q err=%v", ref, err)
	}
}

func TestSafeBlobName(t *testing.T) {
	cases := map[string]string{
		"abc-123":   "abc-123",
		"a/b:c d":   "a_b_c_d",
		"":          "unknown",
		"..":        "unknown",
		".hidden":   "hidden",
		"sess.99_x": "sess.99_x",
	}
	for in, want := range cases {
		if got := safeBlobName(in); got != want {
			t.Errorf("safeBlobName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPruneRemovesOrphanedBlobs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "journal.db")
	blobDir := filepath.Join(dir, "blobs")
	retention := map[string]time.Duration{StreamACPSession: 24 * time.Hour}
	store, err := Open(dbPath, retention, WithBlobDir(blobDir))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	blobs := NewBlobStore(blobDir)
	refOld, _ := blobs.AppendTranscript("sess-old", TranscriptTurn{Time: time.Now(), Outcome: "completed", Prompt: "a", Response: "b"})
	refNew, _ := blobs.AppendTranscript("sess-new", TranscriptTurn{Time: time.Now(), Outcome: "completed", Prompt: "c", Response: "d"})
	refMixed, _ := blobs.AppendTranscript("sess-mixed", TranscriptTurn{Time: time.Now(), Outcome: "completed", Prompt: "e", Response: "f"})

	old := func(t time.Time) func() time.Time { return func() time.Time { return t } }
	past := time.Now().Add(-48 * time.Hour)

	// Old-only session, and the old turn of the mixed session: 48h ago.
	recOld := NewRecorder(store, map[string]bool{StreamACPSession: true}, nil, WithClock(old(past)))
	recOld.Record(context.Background(), Event{Stream: StreamACPSession, Kind: "session.turn", Level: LevelInfo, Summary: "old", Keys: Keys{SessionID: "sess-old"}, BlobRef: refOld})
	recOld.Record(context.Background(), Event{Stream: StreamACPSession, Kind: "session.turn", Level: LevelInfo, Summary: "mixed-old", Keys: Keys{SessionID: "sess-mixed"}, BlobRef: refMixed})
	drain(t, recOld)

	// Fresh session, and the fresh turn of the mixed session: now.
	recNew := NewRecorder(store, map[string]bool{StreamACPSession: true}, nil)
	recNew.Record(context.Background(), Event{Stream: StreamACPSession, Kind: "session.turn", Level: LevelInfo, Summary: "new", Keys: Keys{SessionID: "sess-new"}, BlobRef: refNew})
	recNew.Record(context.Background(), Event{Stream: StreamACPSession, Kind: "session.turn", Level: LevelInfo, Summary: "mixed-new", Keys: Keys{SessionID: "sess-mixed"}, BlobRef: refMixed})
	drain(t, recNew)

	res, err := store.Prune(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.Removed[StreamACPSession] != 2 { // sess-old + mixed-old
		t.Fatalf("removed = %v, want 2 acp_session rows", res.Removed)
	}

	// Orphan (sess-old) file gone; still-referenced files kept.
	if fileExists(t, filepath.Join(blobDir, refOld)) {
		t.Errorf("orphaned blob %s should have been removed", refOld)
	}
	if !fileExists(t, filepath.Join(blobDir, refNew)) {
		t.Errorf("live blob %s should have been kept", refNew)
	}
	if !fileExists(t, filepath.Join(blobDir, refMixed)) {
		t.Errorf("blob %s must be kept while its fresh turn survives", refMixed)
	}
}

func drain(t *testing.T, r *AsyncRecorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("recorder close: %v", err)
	}
}
