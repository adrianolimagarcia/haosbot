package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	graphOutboxVersion       = 1
	graphOutboxMaxRecordSize = 8 << 20
	graphOutboxMaxRetry      = 30 * time.Second
)

type graphOutboxRecord struct {
	Version   int       `json:"version"`
	Op        string    `json:"op"`
	JobID     string    `json:"job_id"`
	SessionKey string   `json:"session_key,omitempty"`
	Content   string    `json:"content,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
	NotBefore time.Time `json:"not_before,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type graphOutbox struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	pending map[string]graphIndexJob
	acked   map[string]struct{}
	closed  bool
}

func openGraphOutbox(path string) (*graphOutbox, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("graph outbox path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create graph outbox directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open graph outbox: %w", err)
	}
	o := &graphOutbox{
		path:    path,
		file:    file,
		pending: make(map[string]graphIndexJob),
		acked:   make(map[string]struct{}),
	}
	if err := o.replay(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return o, nil
}

func (o *graphOutbox) replay() error {
	if _, err := o.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind graph outbox: %w", err)
	}
	reader := bufio.NewReaderSize(o.file, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if len(trimmed) > graphOutboxMaxRecordSize {
				return fmt.Errorf("graph outbox record exceeds %d bytes", graphOutboxMaxRecordSize)
			}
			var record graphOutboxRecord
			if decodeErr := json.Unmarshal([]byte(trimmed), &record); decodeErr != nil {
				// A crash can leave only the final JSON record partially written.
				// A malformed complete record is corruption and must stop startup;
				// an unterminated final record is safely discarded.
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("decode graph outbox: %w", decodeErr)
			}
			if record.Version != graphOutboxVersion {
				return fmt.Errorf("unsupported graph outbox version %d", record.Version)
			}
			o.apply(record)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read graph outbox: %w", err)
		}
	}
	_, err := o.file.Seek(0, io.SeekEnd)
	return err
}

func (o *graphOutbox) apply(record graphOutboxRecord) {
	if record.JobID == "" {
		return
	}
	switch record.Op {
	case "enqueue":
		if _, acked := o.acked[record.JobID]; acked {
			return
		}
		o.pending[record.JobID] = graphIndexJob{
			ID:         record.JobID,
			sessionKey: record.SessionKey,
			content:    record.Content,
			Attempts:   record.Attempts,
			NotBefore:  record.NotBefore,
		}
	case "retry":
		if _, acked := o.acked[record.JobID]; acked {
			return
		}
		job, ok := o.pending[record.JobID]
		if !ok {
			return
		}
		job.Attempts = record.Attempts
		job.NotBefore = record.NotBefore
		o.pending[record.JobID] = job
	case "ack":
		delete(o.pending, record.JobID)
		o.acked[record.JobID] = struct{}{}
	}
}

func (o *graphOutbox) appendLocked(record graphOutboxRecord) error {
	if o.closed || o.file == nil {
		return errors.New("graph outbox is closed")
	}
	record.Version = graphOutboxVersion
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode graph outbox: %w", err)
	}
	if len(payload) > graphOutboxMaxRecordSize {
		return fmt.Errorf("graph outbox record exceeds %d bytes", graphOutboxMaxRecordSize)
	}
	payload = append(payload, '\n')
	for len(payload) > 0 {
		n, writeErr := o.file.Write(payload)
		if writeErr != nil {
			return fmt.Errorf("append graph outbox: %w", writeErr)
		}
		payload = payload[n:]
		if n == 0 {
			return errors.New("append graph outbox: zero-byte write")
		}
	}
	if err := o.file.Sync(); err != nil {
		return fmt.Errorf("sync graph outbox: %w", err)
	}
	return nil
}

func (o *graphOutbox) Enqueue(job graphIndexJob) (bool, error) {
	if job.ID == "" {
		job.ID = graphMemoryJobID(job.sessionKey, "", job.content)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false, errors.New("graph outbox is closed")
	}
	if _, ok := o.acked[job.ID]; ok {
		return true, nil
	}
	if _, ok := o.pending[job.ID]; ok {
		return true, nil
	}
	if err := o.appendLocked(graphOutboxRecord{
			Op:         "enqueue",
			JobID:      job.ID,
			SessionKey: job.sessionKey,
			Content:    job.content,
			Attempts:   job.Attempts,
			NotBefore:  job.NotBefore,
			CreatedAt:  time.Now().UTC(),
		}); err != nil {
		return false, err
	}
	o.pending[job.ID] = job
	return true, nil
}

func (o *graphOutbox) Retry(job graphIndexJob, notBefore time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return errors.New("graph outbox is closed")
	}
	if _, ok := o.pending[job.ID]; !ok {
		return nil
	}
	if err := o.appendLocked(graphOutboxRecord{
			Op:        "retry",
			JobID:     job.ID,
			Attempts:  job.Attempts,
			NotBefore: notBefore.UTC(),
		}); err != nil {
		return err
	}
	job.NotBefore = notBefore
	o.pending[job.ID] = job
	return nil
}

func (o *graphOutbox) Ack(jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("graph outbox ACK requires job id")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return errors.New("graph outbox is closed")
	}
	if _, ok := o.acked[jobID]; ok {
		return nil
	}
	if err := o.appendLocked(graphOutboxRecord{
			Op:        "ack",
			JobID:     jobID,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
		return err
	}
	delete(o.pending, jobID)
	o.acked[jobID] = struct{}{}
	return nil
}

func (o *graphOutbox) Pending() []graphIndexJob {
	o.mu.Lock()
	defer o.mu.Unlock()
	jobs := make([]graphIndexJob, 0, len(o.pending))
	for _, job := range o.pending {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].ID < jobs[j].ID
	})
	return jobs
}

func (o *graphOutbox) Compact() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil {
		return nil
	}
	tmpPath := o.path + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create graph outbox compaction file: %w", err)
	}
	jobs := make([]graphIndexJob, 0, len(o.pending))
	for _, job := range o.pending {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	for _, job := range jobs {
		record, marshalErr := json.Marshal(graphOutboxRecord{
			Version:    graphOutboxVersion,
			Op:         "enqueue",
			JobID:       job.ID,
			SessionKey:  job.sessionKey,
			Content:     job.content,
			Attempts:    job.Attempts,
			NotBefore:   job.NotBefore,
			CreatedAt:   time.Now().UTC(),
		})
		if marshalErr != nil {
			return marshalErr
		}
		record = append(record, '\n')
		if _, writeErr := tmp.Write(record); writeErr != nil {
			return fmt.Errorf("write graph outbox compaction: %w", writeErr)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync graph outbox compaction: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close graph outbox compaction: %w", err)
	}
	if err := o.file.Close(); err != nil {
		return fmt.Errorf("close graph outbox before compaction: %w", err)
	}
	if err := os.Rename(tmpPath, o.path); err != nil {
		return fmt.Errorf("replace graph outbox: %w", err)
	}
	file, err := os.OpenFile(o.path, os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("reopen graph outbox: %w", err)
	}
	o.file = file
	o.acked = make(map[string]struct{})
	ok = true
	return nil
}

func (o *graphOutbox) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	file := o.file
	o.file = nil
	o.mu.Unlock()
	if file == nil {
		return nil
	}
	return file.Close()
}

func graphMemoryJobID(sessionKey, turnID, content string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, sessionKey)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, turnID)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, content)
	return "gm-" + hex.EncodeToString(h.Sum(nil)[:16])
}
