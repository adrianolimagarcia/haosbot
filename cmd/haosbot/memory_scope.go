package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/adrianolimagarcia/nanobot-go/internal/memoryfabric"
	"github.com/adrianolimagarcia/nanobot-go/internal/multiagent"
)

const scopedGraphStorePrefix = "__haosbot_scope_v1__"

func scopedGraphRoot(dataDir string) string {
	return filepath.Join(dataDir, "graph-memory-scoped")
}

func projectMemoryNamespace(workspace string) memoryfabric.Namespace {
	return memoryfabric.Namespace{Scope: memoryfabric.ScopeProject, Owner: workspaceGraphNamespace(workspace)}
}

func globalMemoryNamespace() memoryfabric.Namespace {
	return memoryfabric.Namespace{Scope: memoryfabric.ScopeGlobal, Owner: memoryfabric.ScopeGlobal}
}

func agentMemoryNamespace(profile multiagent.Profile, workspace string) (memoryfabric.Namespace, error) {
	scope := strings.ToLower(strings.TrimSpace(profile.MemoryScope))
	if scope == "" {
		scope = memoryfabric.ScopeProject
	}
	project := workspaceGraphNamespace(workspace)
	switch scope {
	case memoryfabric.ScopePrivate:
		return memoryfabric.Namespace{Scope: scope, Owner: project + ":agent:" + profile.ID}, nil
	case memoryfabric.ScopeTeam:
		team := strings.TrimSpace(profile.MemoryOwner)
		if team == "" {
			team = "default"
		}
		return memoryfabric.Namespace{Scope: scope, Owner: project + ":team:" + team}, nil
	case memoryfabric.ScopeProject:
		return memoryfabric.Namespace{Scope: scope, Owner: project}, nil
	case memoryfabric.ScopeGlobal:
		// Global is deliberately singular. memoryOwner partitions team scope,
		// but allowing it here would create a nominally-global island that the
		// commander and other global agents could not see.
		return globalMemoryNamespace(), nil
	default:
		return memoryfabric.Namespace{}, fmt.Errorf("unsupported memory scope %q for agent %s", scope, profile.ID)
	}
}

func graphStoreKey(namespace memoryfabric.Namespace) (string, error) {
	ns, err := memoryfabric.NormalizeNamespace(namespace)
	if err != nil {
		return "", err
	}
	return scopedGraphStorePrefix + ":" + ns.Key(), nil
}
