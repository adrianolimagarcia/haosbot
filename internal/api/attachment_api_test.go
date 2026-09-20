package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

func TestValidateWebUIMediaScopesFilesToSession(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	root := t.TempDir(); s.SetDataDir(root)
	session := "session_1234567890"
	dir := filepath.Join(s.webUIMediaRoot(), session)
	if err := os.MkdirAll(dir, 0o700); err != nil { t.Fatal(err) }
	inside := filepath.Join(dir, "0123456789abcdef01234567.txt")
	if err := os.WriteFile(inside, []byte("hello"), 0o600); err != nil { t.Fatal(err) }
	got, err := s.validateWebUIMedia(session, []string{inside, inside})
	if err != nil { t.Fatalf("validate inside: %v", err) }
	if len(got) != 1 || got[0] != inside { t.Fatalf("dedupe got %#v", got) }
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("no"), 0o600); err != nil { t.Fatal(err) }
	if _, err := s.validateWebUIMedia(session, []string{outside}); err == nil { t.Fatal("expected cross-session/path rejection") }
}

func TestAttachmentListResolveAndCleanup(t *testing.T) {
	s := NewServer(config.DefaultConfig(), nil, nil)
	root := t.TempDir(); s.SetDataDir(root)
	session := "session_1234567890"
	dir := filepath.Join(s.webUIMediaRoot(), session)
	if err := os.MkdirAll(dir, 0o700); err != nil { t.Fatal(err) }
	id := "0123456789abcdef01234567"
	path := filepath.Join(dir, id+".txt")
	if err := os.WriteFile(path, []byte("preview me"), 0o600); err != nil { t.Fatal(err) }
	items, err := s.listWebUIAttachments(session)
	if err != nil { t.Fatal(err) }
	if len(items) != 1 || items[0].ID != id || items[0].Binary || items[0].Preview != "preview me" { t.Fatalf("unexpected item %#v", items) }
	resolved, err := s.resolveWebUIAttachment(session, id)
	if err != nil || resolved != path { t.Fatalf("resolve %q %v", resolved, err) }
	if err := cleanupWebUIMedia(s.webUIMediaRoot(), session); err != nil { t.Fatal(err) }
	if _, err := os.Stat(dir); !os.IsNotExist(err) { t.Fatalf("media directory survived cleanup: %v", err) }
}

func TestAttachmentHelpersRejectUnsafeValues(t *testing.T) {
	if validAttachmentID("../escape") { t.Fatal("unsafe id accepted") }
	if got := safeAttachmentExtension("../../evil.EXE;sh", "text/plain"); got != ".bin" { t.Fatalf("unsafe extension = %q", got) }
	if webUIAttachmentMIMEAllowed("application/x-executable") { t.Fatal("executable MIME accepted") }
}
