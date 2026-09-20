package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/session"
)

// End-to-end cover for the search handler: a match that sits far enough into an
// accented message that both window edges land inside a multi-byte rune used to
// produce a snippet full of replacement characters.
func TestWebUISearchReturnsRuneAlignedSnippets(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(workspace, filepath.Join(dir, "sessions"))
	if _, err := store.List(); err != nil {
		t.Fatalf("session store: %v", err)
	}
	key := "webui:" + strings.Repeat("a", 32)
	sess, err := store.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	sess.AddMessage(core.Message{
		Role:    core.RoleUser,
		Content: core.TextContent(strings.Repeat("á", 200) + " alvo " + strings.Repeat("é", 200)),
	})
	if err := sess.Save(); err != nil {
		t.Fatal(err)
	}

	s := NewServer(&config.Config{}, nil, nil)
	s.SetDataDir(dir)
	s.SetSessionStore(store)

	rec := httptest.NewRecorder()
	s.handleWebUISearch(rec, httptest.NewRequest(http.MethodGet, "/api/webui/search?q=alvo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Results []struct {
			Key     string `json:"key"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) == 0 {
		t.Fatalf("no results: %s", rec.Body.String())
	}
	snippet := payload.Results[0].Snippet
	if !utf8.ValidString(snippet) {
		t.Fatalf("snippet is not valid UTF-8: %q", snippet)
	}
	if strings.ContainsRune(snippet, utf8.RuneError) {
		t.Fatalf("snippet contains a replacement character: %q", snippet)
	}
	if !strings.Contains(snippet, "alvo") {
		t.Fatalf("snippet lost the match: %q", snippet)
	}
	if strings.HasPrefix(snippet, "alvo") {
		t.Fatalf("snippet was not windowed around the match: %q", snippet)
	}
}
