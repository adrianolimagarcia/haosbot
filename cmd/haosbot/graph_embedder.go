package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	micrographrag "github.com/adrianolimagarcia/micrographrag-go"
)

const graphEmbedderModeEnv = "NANOBOT_GRAPH_EMBEDDER"

// resolveGraphEmbedder never downloads a model in auto mode. Operators can
// choose "potion" to allow the dependency's normal model resolution, or
// "off" to force lexical+graph retrieval.
func resolveGraphEmbedder(ctx context.Context) (micrographrag.Embedder, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(graphEmbedderModeEnv)))
	if mode == "off" || mode == "disabled" {
		return nil, nil
	}
	if mode == "" || mode == "auto" {
		home := strings.TrimSpace(os.Getenv("GO_POTION_HOME"))
		if home == "" {
			return nil, nil
		}
		model := filepath.Join(home, "BASE2M", "model.safetensors")
		tokenizer := filepath.Join(home, "BASE2M", "tokenizer.json")
		if _, err := os.Stat(model); err != nil {
			return nil, nil
		}
		if _, err := os.Stat(tokenizer); err != nil {
			return nil, nil
		}
	}
	return micrographrag.NewPotionEmbedder(ctx)
}
