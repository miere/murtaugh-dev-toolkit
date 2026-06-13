package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGo)
)

// maxPayloadBytes caps the serialized payload stored on a row. Oversized
// payloads are replaced with a small marker so rows stay bounded; the full body
// belongs in a blob referenced by Event.BlobRef.
const maxPayloadBytes = 16 << 10 // 16 KiB

const schema = `
CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,
    stream     TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    level      TEXT    NOT NULL,
    corr_id    TEXT,
    team_id    TEXT,
    channel_id TEXT,
    thread_ts  TEXT,
    user_id    TEXT,
    session_id TEXT,
    job_name   TEXT,
    rule_id    TEXT,
    summary    TEXT    NOT NULL,
    payload    TEXT,
    blob_ref   TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_stream_ts     ON events(stream, ts);
CREATE INDEX IF NOT EXISTS idx_events_stream_lvl_ts ON events(stream, level, ts);
CREATE INDEX IF NOT EXISTS idx_events_channel_ts    ON events(stream, channel_id, ts);
CREATE INDEX IF NOT EXISTS idx_events_corr          ON events(corr_id);
CREATE INDEX IF NOT EXISTS idx_events_session       ON events(session_id);
`

// Store owns the SQLite database backing the journal. It serves both the write
// path (appendBatch, used by the async recorder) and the read/maintenance path
// (Query, Stats, Prune). A process opens its own Store: the gateway daemon for
// writing and sweeping, the CLI/MCP frontends for reading. WAL mode lets those
// separate processes read while the daemon writes.
type Store struct {
	db        *sql.DB
	retention map[string]time.Duration
	// blobDir, when set, is where transcript blobs live. Prune uses it to
	// remove files orphaned by row deletion; empty disables blob cleanup.
	blobDir string
}

// OpenOption customises a Store at Open time.
type OpenOption func(*Store)

// WithBlobDir tells the store where transcript blobs live so Prune can delete
// files orphaned when their last referencing row is removed. Without it, Prune
// trims rows only and leaves blob files alone.
func WithBlobDir(dir string) OpenOption {
	return func(s *Store) { s.blobDir = strings.TrimSpace(dir) }
}

// Open opens (creating if absent) the journal database at path and ensures the
// schema exists. retention maps a stream to its max age and is consulted by
// Prune; streams absent from the map are never pruned. A path of ":memory:" or
// a "file:" DSN is passed through; any other path has its parent directory
// created.
//
// The connection is configured for WAL so concurrent reader processes do not
// block the single writer, with a busy_timeout so a brief write lock is waited
// out rather than erroring. MaxOpenConns is pinned to 1: within a process the
// journal is single-writer by design, and serializing also keeps WAL and the
// pragmas deterministic.
func Open(path string, retention map[string]time.Duration, opts ...OpenOption) (*Store, error) {
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create journal dir %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open journal %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init journal schema: %w", err)
	}

	if retention == nil {
		retention = map[string]time.Duration{}
	}
	store := &Store{db: db, retention: retention}
	for _, opt := range opts {
		opt(store)
	}
	return store, nil
}

// dsn builds the modernc.org/sqlite connection string, attaching the pragmas
// that every connection must carry. Pragmas ride on the DSN (rather than a
// post-open Exec) so they apply to each pooled connection. A ":memory:" path is
// used verbatim (with pragmas) by tests; real paths are turned into file URIs.
func dsn(path string) string {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", "busy_timeout(5000)")
	pragmas.Add("_pragma", "synchronous(NORMAL)")
	pragmas.Add("_pragma", "auto_vacuum(INCREMENTAL)")
	pragmas.Add("_pragma", "foreign_keys(0)")
	if strings.HasPrefix(path, "file:") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return path + sep + pragmas.Encode()
	}
	return path + "?" + pragmas.Encode()
}

// Close closes the underlying database. It does not drain the recorder; the
// composition root closes the recorder (which drains) before the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// appendBatch inserts a batch of events in a single transaction. It is called
// only by the async recorder's writer goroutine, so the writes are serialized.
// A nil/oversized payload is normalised before insert.
func (s *Store) appendBatch(ctx context.Context, batch []recordedEvent) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin journal tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO events
        (ts, stream, kind, level, corr_id, team_id, channel_id, thread_ts, user_id, session_id, job_name, rule_id, summary, payload, blob_ref)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare journal insert: %w", err)
	}
	defer stmt.Close()

	for _, re := range batch {
		e := re.event
		level := e.Level
		if level == "" {
			level = LevelInfo
		}
		if _, err := stmt.ExecContext(ctx,
			re.at.UnixMilli(), e.Stream, e.Kind, string(level), nullStr(e.CorrID),
			nullStr(e.Keys.TeamID), nullStr(e.Keys.ChannelID), nullStr(e.Keys.ThreadTS),
			nullStr(e.Keys.UserID), nullStr(e.Keys.SessionID), nullStr(e.Keys.JobName),
			nullStr(e.Keys.RuleID), e.Summary, encodePayload(e.Payload), nullStr(e.BlobRef),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert journal event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit journal tx: %w", err)
	}
	return nil
}

// Query returns events matching q, most recent first, bounded by q.Limit
// (defaulting to defaultQueryLimit and capped at maxQueryLimit).
func (s *Store) Query(ctx context.Context, q Query) ([]Record, error) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, val any) {
		where = append(where, clause)
		args = append(args, val)
	}
	if q.Stream != "" {
		add("stream = ?", q.Stream)
	}
	if q.Kind != "" {
		add("kind = ?", q.Kind)
	}
	if q.Level != "" {
		// "at least this severity" via the same ranking rank() uses, expressed
		// inline so the comparison happens in SQL.
		add("(CASE level WHEN 'debug' THEN 0 WHEN 'warn' THEN 2 WHEN 'error' THEN 3 ELSE 1 END) >= ?", q.Level.rank())
	}
	if q.ChannelID != "" {
		add("channel_id = ?", q.ChannelID)
	}
	if q.UserID != "" {
		add("user_id = ?", q.UserID)
	}
	if q.SessionID != "" {
		add("session_id = ?", q.SessionID)
	}
	if q.CorrID != "" {
		add("corr_id = ?", q.CorrID)
	}
	if q.RuleID != "" {
		add("rule_id = ?", q.RuleID)
	}
	if !q.Since.IsZero() {
		add("ts >= ?", q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		add("ts <= ?", q.Until.UnixMilli())
	}

	sb := strings.Builder{}
	sb.WriteString(`SELECT id, ts, stream, kind, level, corr_id, team_id, channel_id, thread_ts, user_id, session_id, job_name, rule_id, summary, payload, blob_ref FROM events`)
	if len(where) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(where, " AND "))
	}
	sb.WriteString(" ORDER BY ts DESC, id DESC LIMIT ?")
	args = append(args, effectiveLimit(q.Limit))

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query journal: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var (
			rec     Record
			ts      int64
			corr    sql.NullString
			team    sql.NullString
			channel sql.NullString
			thread  sql.NullString
			user    sql.NullString
			session sql.NullString
			job     sql.NullString
			rule    sql.NullString
			payload sql.NullString
			blob    sql.NullString
			level   string
		)
		if err := rows.Scan(&rec.ID, &ts, &rec.Stream, &rec.Kind, &level, &corr,
			&team, &channel, &thread, &user, &session, &job, &rule, &rec.Summary, &payload, &blob); err != nil {
			return nil, fmt.Errorf("scan journal row: %w", err)
		}
		rec.Time = time.UnixMilli(ts).UTC()
		rec.Level = Level(level)
		rec.CorrID = corr.String
		rec.Keys = Keys{
			TeamID:    team.String,
			ChannelID: channel.String,
			ThreadTS:  thread.String,
			UserID:    user.String,
			SessionID: session.String,
			JobName:   job.String,
			RuleID:    rule.String,
		}
		if payload.Valid && payload.String != "" {
			rec.Payload = json.RawMessage(payload.String)
		}
		rec.BlobRef = blob.String
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Stats returns a per-stream summary for every known stream, including streams
// with no rows (reported as zero) so callers see the full picture.
func (s *Store) Stats(ctx context.Context) ([]StreamStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stream, COUNT(*), MIN(ts), MAX(ts) FROM events GROUP BY stream`)
	if err != nil {
		return nil, fmt.Errorf("stat journal: %w", err)
	}
	defer rows.Close()

	seen := map[string]StreamStat{}
	for rows.Next() {
		var (
			stream       string
			count        int64
			minTS, maxTS sql.NullInt64
		)
		if err := rows.Scan(&stream, &count, &minTS, &maxTS); err != nil {
			return nil, fmt.Errorf("scan journal stat: %w", err)
		}
		st := StreamStat{Stream: stream, Count: count}
		if minTS.Valid {
			t := time.UnixMilli(minTS.Int64).UTC()
			st.Oldest = &t
		}
		if maxTS.Valid {
			t := time.UnixMilli(maxTS.Int64).UTC()
			st.Newest = &t
		}
		seen[stream] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]StreamStat, 0, len(Streams))
	for _, stream := range Streams {
		if st, ok := seen[stream]; ok {
			out = append(out, st)
		} else {
			out = append(out, StreamStat{Stream: stream})
		}
	}
	return out, nil
}

// Prune deletes events older than each stream's configured retention, relative
// to now. Streams with no retention (or a non-positive one) are left untouched.
// It is the body of both the daemon's internal sweeper and the journal.prune
// tool.
func (s *Store) Prune(ctx context.Context, now time.Time) (PruneResult, error) {
	res := PruneResult{Removed: map[string]int64{}}
	for stream, d := range s.retention {
		if d <= 0 {
			continue
		}
		cutoff := now.Add(-d).UnixMilli()
		// Collect the blob refs about to lose rows before deleting, so we can
		// remove any that end up unreferenced.
		candidates, err := s.expiringBlobRefs(ctx, stream, cutoff)
		if err != nil {
			return res, err
		}
		r, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE stream = ? AND ts < ?`, stream, cutoff)
		if err != nil {
			return res, fmt.Errorf("prune journal stream %q: %w", stream, err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return res, fmt.Errorf("prune journal stream %q rows: %w", stream, err)
		}
		if n > 0 {
			res.Removed[stream] = n
			res.Total += n
		}
		s.removeOrphanBlobs(ctx, candidates)
	}
	return res, nil
}

// expiringBlobRefs lists the distinct blob refs on rows about to be pruned from
// a stream. Returns nil when no blob dir is configured (cleanup disabled).
func (s *Store) expiringBlobRefs(ctx context.Context, stream string, cutoff int64) ([]string, error) {
	if s.blobDir == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT blob_ref FROM events WHERE stream = ? AND ts < ? AND blob_ref IS NOT NULL AND blob_ref <> ''`,
		stream, cutoff)
	if err != nil {
		return nil, fmt.Errorf("collect expiring blob refs for %q: %w", stream, err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// removeOrphanBlobs deletes each candidate blob file that no surviving row still
// references. A session's transcript file is kept while any of its turns remain
// and removed once the whole session has aged out. Deletion is best-effort.
func (s *Store) removeOrphanBlobs(ctx context.Context, refs []string) {
	if s.blobDir == "" {
		return
	}
	for _, ref := range refs {
		var one int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE blob_ref = ? LIMIT 1`, ref).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			_ = os.Remove(filepath.Join(s.blobDir, ref))
		}
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// encodePayload marshals v to a JSON string for storage. A nil payload becomes
// SQL NULL; a payload larger than maxPayloadBytes is replaced with a marker so
// rows stay bounded (the full body belongs in a blob). Marshal failures are
// recorded as a marker rather than dropping the whole event.
func encodePayload(v any) any {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return `{"_error":"payload not serialisable"}`
	}
	if len(data) > maxPayloadBytes {
		return fmt.Sprintf(`{"_truncated":true,"_bytes":%d}`, len(data))
	}
	return string(data)
}
