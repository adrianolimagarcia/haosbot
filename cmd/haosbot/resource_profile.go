package main

import (
	"os"
	"strconv"
	"strings"
)

const (
	projectionWorkersEnv = "NANOBOT_PROJECTION_WORKERS"
	memoryMaxPendingEnv  = "NANOBOT_MEMORY_MAX_PENDING"
	memoryCacheKBEnv     = "NANOBOT_MEMORY_CACHE_KB"
)

// resourceProfile keeps every new memory subsystem bounded by default. The
// defaults are intentionally conservative for SBCs, routers and small VPSs;
// operators can raise them without changing the config compatibility schema.
type resourceProfile struct {
	ProjectionWorkers int
	MemoryMaxPending  int
	MemoryCacheKB     int
}

func resolveResourceProfile() resourceProfile {
	p := resourceProfile{ProjectionWorkers: 1, MemoryMaxPending: 512, MemoryCacheKB: 512}
	p.ProjectionWorkers = boundedEnvInt(projectionWorkersEnv, p.ProjectionWorkers, 1, 4)
	p.MemoryMaxPending = boundedEnvInt(memoryMaxPendingEnv, p.MemoryMaxPending, 32, 4096)
	p.MemoryCacheKB = boundedEnvInt(memoryCacheKBEnv, p.MemoryCacheKB, 128, 4096)
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
