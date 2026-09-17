package main

import (
	"testing"
	"time"
)

func TestNextProjectionBackoffCapsAtThirtySeconds(t *testing.T) {
	if got := nextProjectionBackoff(500 * time.Millisecond); got != time.Second {
		t.Fatalf("first backoff=%s", got)
	}
	if got := nextProjectionBackoff(16 * time.Second); got != maxProjectionIdleBackoff {
		t.Fatalf("capped backoff=%s", got)
	}
	if got := nextProjectionBackoff(maxProjectionIdleBackoff); got != maxProjectionIdleBackoff {
		t.Fatalf("backoff exceeded cap=%s", got)
	}
}
