// Package memoryfabric owns the transactional memory write path.
//
// A completed turn is one canonical record followed by durable projection
// jobs. The record and all projection jobs are committed in the same SQLite
// transaction. GraphRAG, Obsidian and future projections are consumers, never
// alternate sources of truth.
package memoryfabric

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	ProjectionGraph    = "graph"
	ProjectionObsidian = "obsidian"
	stateQueued        = "queued"
	stateRunning       = "running"
	stateSucceeded     = "succeeded"
	stateDead          = "dead"
	defaultLease       = 2 * time.Minute
	defaultMaxAttempts = 8
	defaultMaxPending  = 512
	defaultMaxPendingBytes int64 = 4 * 1024 * 1024
	defaultMaxContentBytes = 64 * 1024
	defaultMaxDiskBytes int64 = 200 * 1024 * 1024
)

type Config struct {
	Path        string
	BusyTimeout time.Duration
	CacheKB     int
	MaxPending  int
	MaxPendingBytes int64
	MaxContentBytes int
	MaxDiskBytes int64
	MaxAttempts int
	Lease       time.Duration
	Projections []string
}

type Record struct {
	ID         string
	SessionKey string
	Content    string
	CreatedAt  time.Time
}

type Job struct {
	ID         string
	Projection string
	RecordID   string
	SessionKey string
	Content    string
	Attempts   int
	CreatedAt  time.Time
}

type Stats struct {
	Pending       int64 `json:"pending"`
	PendingBytes  int64 `json:"pending_bytes"`
	Running       int64 `json:"running"`
	Succeeded     int64 `json:"succeeded"`
	Dead          int64 `json:"dead"`
	OldestAgeSecs int64 `json:"oldest_age_seconds"`
}

type Store struct {
	db          *sql.DB
	path        string
	maxPending  int
	maxPendingBytes int64
	maxContentBytes int
	maxDiskBytes int64
	maxAttempts int
	lease       time.Duration
	projections []string
	closeOnce   sync.Once
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("memoryfabric: path is empty")
	}
	if cfg.BusyTimeout <= 0 {
		cfg.BusyTimeout = 5 * time.Second
	}
	if cfg.CacheKB <= 0 {
		cfg.CacheKB = 512
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = defaultMaxPending
	}
	if cfg.MaxPendingBytes <= 0 {
		cfg.MaxPendingBytes = defaultMaxPendingBytes
	}
	if cfg.MaxContentBytes <= 0 {
		cfg.MaxContentBytes = defaultMaxContentBytes
	}
	if cfg.MaxDiskBytes <= 0 {
		cfg.MaxDiskBytes = defaultMaxDiskBytes
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.Lease <= 0 {
		cfg.Lease = defaultLease
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("memoryfabric: create data directory: %w", err)
	}
	db, err := sql.Open("sqlite3", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("memoryfabric: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	projections := append([]string(nil), cfg.Projections...)
	if len(projections) == 0 {
		projections = []string{ProjectionGraph, ProjectionObsidian}
	}
	s := &Store{db: db, path: cfg.Path, maxPending: cfg.MaxPending, maxPendingBytes: cfg.MaxPendingBytes, maxContentBytes: cfg.MaxContentBytes, maxDiskBytes: cfg.MaxDiskBytes, maxAttempts: cfg.MaxAttempts, lease: cfg.Lease, projections: projections}
	for _, pragma := range []string{
		"PRAGMA foreign_keys=ON",
		fmt.Sprintf("PRAGMA busy_timeout=%d", cfg.BusyTimeout.Milliseconds()),
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA temp_store=FILE",
		fmt.Sprintf("PRAGMA cache_size=-%d", cfg.CacheKB),
		"PRAGMA cache_spill=ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("memoryfabric: %s: %w", pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS memory_records (
  record_id TEXT PRIMARY KEY,
  session_key TEXT NOT NULL,
  content TEXT NOT NULL,
  content_hash BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_records_session ON memory_records(session_key,created_at);
CREATE TABLE IF NOT EXISTS memory_outbox (
  job_id TEXT NOT NULL,
  projection TEXT NOT NULL,
  record_id TEXT NOT NULL,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  lease_until INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(job_id,projection),
  FOREIGN KEY(record_id) REFERENCES memory_records(record_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_memory_outbox_claim ON memory_outbox(projection,state,next_attempt_at,lease_until,created_at);
CREATE TABLE IF NOT EXISTS memory_projection_receipts (
  projection TEXT NOT NULL,
  record_id TEXT NOT NULL,
  applied_at INTEGER NOT NULL,
  PRIMARY KEY(projection,record_id)
);
CREATE TABLE IF NOT EXISTS memory_queue_counters (
  singleton INTEGER PRIMARY KEY CHECK(singleton=1),
  pending_jobs INTEGER NOT NULL DEFAULT 0,
  pending_bytes INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO memory_queue_counters(singleton) VALUES(1);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("memoryfabric: create schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
UPDATE memory_queue_counters SET
  pending_jobs=(SELECT COUNT(*) FROM memory_outbox WHERE state IN (?,?)),
  pending_bytes=(SELECT COALESCE(SUM(LENGTH(content)),0) FROM memory_records WHERE record_id IN (SELECT DISTINCT record_id FROM memory_outbox WHERE state IN (?,?)))
WHERE singleton=1`, stateQueued, stateRunning, stateQueued, stateRunning); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("memoryfabric: rebuild queue counters: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.db.Close() })
	return err
}

// AppendTurn atomically writes the canonical memory record and all projection
// jobs. Repeating the same deterministic record is safe and repairs a missing
// projection row without duplicating the record.
func (s *Store) AppendTurn(ctx context.Context, recordID, sessionKey, content string) error {
	if strings.TrimSpace(recordID) == "" {
		recordID = DeterministicID(sessionKey, content)
	}
	if strings.TrimSpace(sessionKey) == "" {
		return errors.New("memoryfabric: session key is empty")
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("memoryfabric: content is empty")
	}
	if s.maxContentBytes > 0 && len(content) > s.maxContentBytes {
		return fmt.Errorf("memoryfabric: content exceeds limit (%d bytes)", s.maxContentBytes)
	}
	hash := sha256.Sum256([]byte(content))
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memoryfabric: begin append: %w", err)
	}
	defer tx.Rollback()
	var existingHash []byte
	var existingSession string
	err = tx.QueryRowContext(ctx, "SELECT session_key,content_hash FROM memory_records WHERE record_id=?", recordID).Scan(&existingSession, &existingHash)
	if err == nil {
		if existingSession != sessionKey || !sameBytes(existingHash, hash[:]) {
			return fmt.Errorf("memoryfabric: record %s already exists with different identity or content", recordID)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("memoryfabric: check record: %w", err)
	} else {
		if s.maxDiskBytes > 0 {
			used, err := diskUsage(s.path)
			if err != nil {
				return fmt.Errorf("memoryfabric: inspect disk usage: %w", err)
			}
			// This is a conservative admission check. SQLite page/index overhead is
			// intentionally not estimated precisely; a small margin keeps the
			// configured disk budget meaningful without a background full scan.
			if used+int64(len(content))+64*1024 > s.maxDiskBytes {
				return fmt.Errorf("memoryfabric: disk budget reached (%d bytes)", s.maxDiskBytes)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_records(record_id,session_key,content,content_hash,created_at) VALUES(?,?,?,?,?)`, recordID, sessionKey, content, hash[:], now); err != nil {
			return fmt.Errorf("memoryfabric: insert record: %w", err)
		}
	}
	var pending int64
	if err := tx.QueryRowContext(ctx, "SELECT pending_jobs FROM memory_queue_counters WHERE singleton=1").Scan(&pending); err != nil {
		return fmt.Errorf("memoryfabric: read pending jobs: %w", err)
	}
	var missingJobs int64
	for _, projection := range s.projections {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM memory_outbox WHERE job_id=? AND projection=?", recordID, projection).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			missingJobs++
		}
	}
	if pending+missingJobs > int64(s.maxPending) {
		return fmt.Errorf("memoryfabric: outbox capacity reached (%d jobs)", s.maxPending)
	}
	var pendingBytes int64
	if err := tx.QueryRowContext(ctx, "SELECT pending_bytes FROM memory_queue_counters WHERE singleton=1").Scan(&pendingBytes); err != nil {
		return fmt.Errorf("memoryfabric: read pending bytes: %w", err)
	}
	var recordPending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_outbox WHERE record_id=? AND state IN (?,?))`, recordID, stateQueued, stateRunning).Scan(&recordPending); err != nil {
		return fmt.Errorf("memoryfabric: check pending record: %w", err)
	}
	if missingJobs > 0 && !recordPending && pendingBytes+int64(len(content)) > s.maxPendingBytes {
		return fmt.Errorf("memoryfabric: outbox byte budget reached (%d bytes)", s.maxPendingBytes)
	}
	for _, projection := range s.projections {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_outbox(job_id,projection,record_id,state,created_at,updated_at) VALUES(?,?,?,?,?,?)`, recordID, projection, recordID, stateQueued, now, now); err != nil {
			return fmt.Errorf("memoryfabric: enqueue %s projection: %w", projection, err)
		}
	}
	if missingJobs > 0 {
		byteDelta := int64(0)
		if !recordPending {
			byteDelta = int64(len(content))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_queue_counters SET pending_jobs=pending_jobs+?,pending_bytes=pending_bytes+? WHERE singleton=1`, missingJobs, byteDelta); err != nil {
			return fmt.Errorf("memoryfabric: update queue counters: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memoryfabric: commit append: %w", err)
	}
	return nil
}

func (s *Store) Claim(ctx context.Context, projection string) (Job, bool, error) {
	if projection == "" {
		return Job{}, false, errors.New("memoryfabric: projection is empty")
	}
	now := time.Now().UnixMilli()
	leaseUntil := now + s.lease.Milliseconds()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	var jobID string
	err = tx.QueryRowContext(ctx, `SELECT job_id FROM memory_outbox WHERE projection=? AND ((state=? AND next_attempt_at<=?) OR (state=? AND lease_until<=?)) ORDER BY created_at,job_id LIMIT 1`, projection, stateQueued, now, stateRunning, now).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_outbox SET state=?,attempts=attempts+1,lease_until=?,updated_at=? WHERE job_id=? AND projection=? AND ((state=? AND next_attempt_at<=?) OR (state=? AND lease_until<=?))`, stateRunning, leaseUntil, now, jobID, projection, stateQueued, now, stateRunning, now)
	if err != nil {
		return Job{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return Job{}, false, err
	}
	var job Job
	var createdAt int64
	err = tx.QueryRowContext(ctx, `SELECT o.job_id,o.projection,o.record_id,r.session_key,r.content,o.attempts,o.created_at FROM memory_outbox o JOIN memory_records r ON r.record_id=o.record_id WHERE o.job_id=? AND o.projection=?`, jobID, projection).Scan(&job.ID, &job.Projection, &job.RecordID, &job.SessionKey, &job.Content, &job.Attempts, &createdAt)
	if err != nil {
		return Job{}, false, err
	}
	job.CreatedAt = time.UnixMilli(createdAt).UTC()
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (s *Store) Ack(ctx context.Context, projection, jobID string) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recordID string
	var contentBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT o.record_id,LENGTH(r.content) FROM memory_outbox o JOIN memory_records r ON r.record_id=o.record_id WHERE o.job_id=? AND o.projection=? AND o.state IN (?,?)`, jobID, projection, stateQueued, stateRunning).Scan(&recordID, &contentBytes); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_outbox SET state=?,lease_until=0,last_error='',updated_at=? WHERE job_id=? AND projection=? AND state IN (?,?)`, stateSucceeded, now, jobID, projection, stateQueued, stateRunning)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil
	}
	if err := s.decrementPendingCounter(ctx, tx, recordID, contentBytes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_projection_receipts(projection,record_id,applied_at) SELECT ?,record_id,? FROM memory_outbox WHERE job_id=? AND projection=?`, projection, now, jobID, projection); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Retry(ctx context.Context, projection, jobID string, cause error) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	var state, recordID string
	var contentBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT o.attempts,o.state,o.record_id,LENGTH(r.content) FROM memory_outbox o JOIN memory_records r ON r.record_id=o.record_id WHERE o.job_id=? AND o.projection=?`, jobID, projection).Scan(&attempts, &state, &recordID, &contentBytes); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if attempts >= s.maxAttempts {
		result, err := tx.ExecContext(ctx, `UPDATE memory_outbox SET state=?,lease_until=0,last_error=?,updated_at=? WHERE job_id=? AND projection=? AND state IN (?,?)`, stateDead, errorString(cause), now, jobID, projection, stateQueued, stateRunning)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 1 {
			if err := s.decrementPendingCounter(ctx, tx, recordID, contentBytes); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	delay := retryDelaySeconds(attempts)
	if _, err := tx.ExecContext(ctx, `UPDATE memory_outbox SET state=?,lease_until=0,next_attempt_at=?,last_error=?,updated_at=? WHERE job_id=? AND projection=? AND state IN (?,?)`, stateQueued, now+delay*1000, errorString(cause), now, jobID, projection, stateQueued, stateRunning); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) decrementPendingCounter(ctx context.Context, tx *sql.Tx, recordID string, contentBytes int64) error {
	var stillPending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_outbox WHERE record_id=? AND state IN (?,?))`, recordID, stateQueued, stateRunning).Scan(&stillPending); err != nil {
		return err
	}
	byteDelta := int64(0)
	if !stillPending {
		byteDelta = contentBytes
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_queue_counters SET pending_jobs=CASE WHEN pending_jobs>0 THEN pending_jobs-1 ELSE 0 END,pending_bytes=CASE WHEN pending_bytes>? THEN pending_bytes-? ELSE 0 END WHERE singleton=1`, byteDelta, byteDelta)
	return err
}

// RequeueProjection schedules every canonical record for projection again.
//
// It is used for derived-index migrations (for example per-session GraphRAG ->
// workspace GraphRAG). Canonical memory_records are untouched. The operation is
// idempotent at the data level and rebuilds queue counters transactionally.
func (s *Store) RequeueProjection(ctx context.Context, projection string) error {
	if strings.TrimSpace(projection) == "" {
		return errors.New("memoryfabric: projection is empty")
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { return err }
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO memory_outbox(job_id,projection,record_id,state,attempts,lease_until,next_attempt_at,last_error,created_at,updated_at)
SELECT record_id,?,record_id,?,0,0,0,'',created_at,?
FROM memory_records
`, projection, stateQueued, now); err != nil {
		return fmt.Errorf("memoryfabric: seed projection rebuild: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE memory_outbox
SET state=?,attempts=0,lease_until=0,next_attempt_at=0,last_error='',updated_at=?
WHERE projection=?
`, stateQueued, now, projection); err != nil {
		return fmt.Errorf("memoryfabric: requeue projection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_projection_receipts WHERE projection=?", projection); err != nil {
		return fmt.Errorf("memoryfabric: clear projection receipts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE memory_queue_counters SET
 pending_jobs=(SELECT COUNT(*) FROM memory_outbox WHERE state IN (?,?)),
 pending_bytes=(SELECT COALESCE(SUM(LENGTH(content)),0) FROM memory_records WHERE record_id IN
   (SELECT DISTINCT record_id FROM memory_outbox WHERE state IN (?,?)))
WHERE singleton=1
`, stateQueued, stateRunning, stateQueued, stateRunning); err != nil {
		return fmt.Errorf("memoryfabric: rebuild counters after requeue: %w", err)
	}
	return tx.Commit()
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	var pendingTotal int64
	if err := s.db.QueryRowContext(ctx, "SELECT pending_jobs,pending_bytes FROM memory_queue_counters WHERE singleton=1").Scan(&pendingTotal, &out.PendingBytes); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT state,COUNT(*) FROM memory_outbox WHERE state IN (?,?,?) GROUP BY state", stateRunning, stateSucceeded, stateDead)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return out, err
		}
		switch state {
		case stateQueued:
			out.Pending += count
		case stateRunning:
			out.Running += count
		case stateSucceeded:
			out.Succeeded += count
		case stateDead:
			out.Dead += count
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Pending = pendingTotal - out.Running
	if out.Pending < 0 {
		out.Pending = 0
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(created_at) FROM memory_outbox WHERE state IN (?,?)", stateQueued, stateRunning).Scan(&oldest); err != nil {
		return out, err
	}
	if oldest.Valid {
		out.OldestAgeSecs = (time.Now().UnixMilli() - oldest.Int64) / 1000
		if out.OldestAgeSecs < 0 {
			out.OldestAgeSecs = 0
		}
	}
	return out, nil
}

func DeterministicID(sessionKey, content string) string {
	h := sha256.Sum256([]byte(sessionKey + "\x00" + content))
	return "mem-" + hex.EncodeToString(h[:16])
}

func sameBytes(a, b []byte) bool { return string(a) == string(b) }
func errorString(err error) string { if err == nil { return "" }; return err.Error() }

func retryDelaySeconds(attempt int) int64 {
	switch {
	case attempt <= 1:
		return 1
	case attempt == 2:
		return 5
	case attempt == 3:
		return 30
	default:
		return 120
	}
}

func diskUsage(path string) (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}
