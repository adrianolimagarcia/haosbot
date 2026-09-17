package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

const graphEmbedderModeEnv = "NANOBOT_GRAPH_EMBEDDER"

const maxAutoEmbedderBytes int64 = 8 * 1024 * 1024

// resolveGraphEmbedder never downloads a model in auto mode. Operators can
// choose "potion" to allow the dependency's normal model resolution, or
// "off" to force lexical+graph retrieval. Low-resource mode passes
// autoAllowed=false, making vector retrieval an explicit opt-in there.
func resolveGraphEmbedder(ctx context.Context, autoAllowed bool) (micrographrag.Embedder, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(graphEmbedderModeEnv)))
	if mode == "off" || mode == "disabled" {
		return nil, nil
	}
	if mode == "" || mode == "auto" {
		if !autoAllowed {
			return nil, nil
		}
		home := strings.TrimSpace(os.Getenv("GO_POTION_HOME"))
		if home == "" {
			return nil, nil
		}
		model := filepath.Join(home, "BASE2M", "model.safetensors")
		tokenizer := filepath.Join(home, "BASE2M", "tokenizer.json")
		modelInfo, err := os.Stat(model)
		if err != nil {
			return nil, nil
		}
		tokenizerInfo, err := os.Stat(tokenizer)
		if err != nil {
			return nil, nil
		}
		// Keep two megabytes of the incremental RAM budget for SQLite/vector
		// buffers and request handling. File size is only an admission guard;
		// RSS still needs to be measured on the target device.
		if modelInfo.Size()+tokenizerInfo.Size() > maxAutoEmbedderBytes {
			return nil, nil
		}
	}
	return micrographrag.NewPotionEmbedder(ctx)
}
