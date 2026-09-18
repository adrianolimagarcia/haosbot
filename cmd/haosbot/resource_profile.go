package main

import (
	"os"
	"strconv"
	"strings"
)

const (
	resourceProfileEnv          = "NANOBOT_RESOURCE_PROFILE"
	projectionWorkersEnv        = "NANOBOT_PROJECTION_WORKERS"
	projectionPollMsEnv         = "NANOBOT_PROJECTION_POLL_MS"
	memoryMaxPendingEnv         = "NANOBOT_MEMORY_MAX_PENDING"
	memoryMaxPendingBytesEnv = "NANOBOT_MEMORY_MAX_PENDING_BYTES"
	memoryMaxContentBytesEnv = "NANOBOT_MEMORY_MAX_CONTENT_BYTES"
	memoryCacheKBEnv           = "NANOBOT_MEMORY_CACHE_KB"
	obsidianProjectionEnv      = "NANOBOT_OBSIDIAN_PROJECTION"
	memoryRetrievalEnv         = "NANOBOT_MEMORY_RETRIEVAL"
)

const (
	// Future resources have an incremental budget. These are guardrails for
	// optional components, not a claim about the process's total RSS.
	defaultFutureRAMBytes  int64 = 10 * 1024 * 1024
	defaultFutureDiskBytes int64 = 200 * 1024 * 1024
)

// resourceProfile keeps every new memory subsystem bounded by default. The
// defaults are intentionally conservative for SBCs, routers and small VPSs;
// operators can raise them without changing the config compatibility schema.
type resourceProfile struct {
	Name              string
	ProjectionWorkers int
	ProjectionPollMs  int
	MemoryMaxPending  int
	MemoryMaxPendingBytes int64
	MemoryMaxContentBytes int
	MemoryCacheKB     int
	ObsidianEnabled   bool
	MemoryRetrievalEnabled bool
	FutureRAMBytes    int64
	FutureDiskBytes   int64
}

func resolveResourceProfile() resourceProfile {
	p := resourceProfile{
		Name: "balanced", ProjectionWorkers: 1, ProjectionPollMs: 2000,
		MemoryMaxPending: 512, MemoryMaxPendingBytes: 4 * 1024 * 1024,
		MemoryMaxContentBytes: 64 * 1024, MemoryCacheKB: 512,
		ObsidianEnabled: true, MemoryRetrievalEnabled: true, FutureRAMBytes: defaultFutureRAMBytes,
		FutureDiskBytes: defaultFutureDiskBytes,
	}
	name := strings.ToLower(strings.TrimSpace(os.Getenv(resourceProfileEnv)))
	if name == "low" {
		p.Name = name
		p.ProjectionPollMs = 5000
		p.MemoryMaxPending = 256
		p.MemoryMaxPendingBytes = 2 * 1024 * 1024
		p.MemoryMaxContentBytes = 32 * 1024
		p.MemoryCacheKB = 256
		p.ObsidianEnabled = false
		// Retrieval remains enabled by default even in low-resource mode. The
		// profile still disables the embedder and keeps the SQLite/outbox bounds
		// conservative; operators can explicitly opt out with
		// NANOBOT_MEMORY_RETRIEVAL=0.
		p.MemoryRetrievalEnabled = true
	}
	p.ProjectionWorkers = boundedEnvInt(projectionWorkersEnv, p.ProjectionWorkers, 1, 4)
	p.ProjectionPollMs = boundedEnvInt(projectionPollMsEnv, p.ProjectionPollMs, 1000, 10000)
	p.MemoryMaxPending = boundedEnvInt(memoryMaxPendingEnv, p.MemoryMaxPending, 32, 4096)
	p.MemoryMaxPendingBytes = int64(boundedEnvInt(memoryMaxPendingBytesEnv, int(p.MemoryMaxPendingBytes), 64*1024, 8*1024*1024))
	p.MemoryMaxContentBytes = boundedEnvInt(memoryMaxContentBytesEnv, p.MemoryMaxContentBytes, 4*1024, 256*1024)
	p.MemoryCacheKB = boundedEnvInt(memoryCacheKBEnv, p.MemoryCacheKB, 128, 4096)
	p.ObsidianEnabled = boundedEnvBool(obsidianProjectionEnv, p.ObsidianEnabled)
	p.MemoryRetrievalEnabled = boundedEnvBool(memoryRetrievalEnv, p.MemoryRetrievalEnabled)
	return p
}

func boundedEnvInt(name string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min {
		return fallback
	}
	if v > max {
		return max
	}
	return v
}

func boundedEnvBool(name string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
