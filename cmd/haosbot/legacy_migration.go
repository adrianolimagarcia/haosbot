package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
)

type legacyGraphOutboxRecord struct {
	Op          string `json:"op"`
	JobID       string `json:"job_id"`
	SessionKey  string `json:"session_key"`
	Content     string `json:"content"`
}

// migrateLegacyGraphOutbox imports pending records from the pre-Memory-Fabric
// JSONL outbox. It is deliberately read-only: leaving the source file in place
// makes migration restart-safe, while deterministic IDs make repeated imports
// harmless.
func migrateLegacyGraphOutbox(path string, fabric *memoryfabric.Store) error {
	if fabric == nil { return errors.New("legacy migration: memory fabric is nil") }
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) { return nil }
	if err != nil { return fmt.Errorf("open legacy GraphRAG outbox: %w", err) }
	defer f.Close()
	pending := map[string]legacyGraphOutboxRecord{}
	acked := map[string]struct{}{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" { continue }
		var record legacyGraphOutboxRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			// A final partial JSONL record is a known crash state. Valid records
			// before it remain importable and the new SQLite outbox is durable.
			if errors.Is(err, io.ErrUnexpectedEOF) || !strings.HasSuffix(line, "}") { break }
			return fmt.Errorf("decode legacy GraphRAG outbox: %w", err)
		}
		switch record.Op {
		case "enqueue", "retry":
			if _, ok := acked[record.JobID]; !ok && record.JobID != "" { pending[record.JobID] = record }
		case "ack":
			delete(pending, record.JobID)
			acked[record.JobID] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil { return fmt.Errorf("read legacy GraphRAG outbox: %w", err) }
	for _, record := range pending {
		if strings.TrimSpace(record.SessionKey) == "" || strings.TrimSpace(record.Content) == "" { continue }
		if err := fabric.AppendTurn(context.Background(), record.JobID, record.SessionKey, record.Content); err != nil {
			return fmt.Errorf("import legacy GraphRAG job %s: %w", record.JobID, err)
		}
	}
	return nil
}
