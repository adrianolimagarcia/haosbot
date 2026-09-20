package marketplace

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func zipArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallIsDisabledByDefault(t *testing.T) {
	workspace := t.TempDir()
	svc := NewService(Options{Workspace: workspace})

	if svc.InstallSupported() {
		t.Fatal("InstallSupported()=true, want false by default")
	}
	if _, err := svc.Install(context.Background(), InstallRequest{Provider: "skillhub", SkillID: "demo"}); !errors.Is(err, ErrInstallDisabled) {
		t.Fatalf("Install err=%v, want ErrInstallDisabled", err)
	}
	if err := svc.Uninstall(context.Background(), "demo"); !errors.Is(err, ErrInstallDisabled) {
		t.Fatalf("Uninstall err=%v, want ErrInstallDisabled", err)
	}

	trending, err := svc.Trending(context.Background(), "cliapps", 1)
	if err != nil {
		t.Fatal(err)
	}
	if trending.InstallSupported {
		t.Fatal("Trending.InstallSupported=true, want false when installs are disabled")
	}
}

func TestSkillHubInstallAndUninstall(t *testing.T) {
	archive := zipArchive(t, map[string]string{
		"SKILL.md":         "---\nname: demo-skill\n---\n# Demo\n",
		"scripts/run.sh":   "#!/bin/sh\necho hi\n",
		"reference/doc.md": "# Doc\n",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/download") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	// The provider helpers take the client explicitly, so point them at the stub.
	workspace := t.TempDir()
	res, err := installSkillHubSkillFrom(context.Background(), srv.Client(), srv.URL, workspace, "demo-skill", "")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.Installed || res.Name != "demo-skill" {
		t.Fatalf("install response=%+v", res)
	}
	for _, rel := range []string{"SKILL.md", "scripts/run.sh", "reference/doc.md"} {
		if _, err := os.Stat(filepath.Join(workspace, "skills", "demo-skill", filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing installed file %s: %v", rel, err)
		}
	}

	svc := NewService(Options{Workspace: workspace, AllowRemoteInstall: true})
	installed := svc.installedSkills()
	if !installed["demo-skill"] {
		t.Fatalf("installed map=%v, want demo-skill", installed)
	}

	if err := svc.Uninstall(context.Background(), "demo-skill"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "skills", "demo-skill")); !os.IsNotExist(err) {
		t.Fatalf("skill directory still present: %v", err)
	}
}

func TestSkillHubInstallRejectsTraversalAndMissingManifest(t *testing.T) {
	cases := []struct {
		name    string
		entries map[string]string
	}{
		{name: "traversal", entries: map[string]string{"../evil.txt": "evil", "SKILL.md": "x"}},
		{name: "nested traversal", entries: map[string]string{"a/../../evil.txt": "evil", "SKILL.md": "x"}},
		{name: "absolute", entries: map[string]string{"/etc/evil.txt": "evil", "SKILL.md": "x"}},
		{name: "missing manifest", entries: map[string]string{"README.md": "no skill here"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := zipArchive(t, tc.entries)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(archive)
			}))
			defer srv.Close()

			workspace := t.TempDir()
			if _, err := installSkillHubSkillFrom(context.Background(), srv.Client(), srv.URL, workspace, "evil-skill", ""); err == nil {
				t.Fatal("install succeeded, want rejection")
			}
			if _, err := os.Stat(filepath.Join(workspace, "evil.txt")); !os.IsNotExist(err) {
				t.Fatal("traversal wrote outside the staging directory")
			}
			if _, err := os.Stat(filepath.Join(workspace, "skills", "evil-skill")); !os.IsNotExist(err) {
				t.Fatal("rejected skill was deployed anyway")
			}
		})
	}
}

func TestGitHubSkillInstallCopiesWholeDirectory(t *testing.T) {
	blobs := map[string]string{
		"sha-manifest":  "---\nname: gh-skill\n---\n# GH\n",
		"sha-helper":    "print('helper')\n",
		"sha-unrelated": "should not be installed",
	}
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/skills":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
		case strings.HasPrefix(r.URL.Path, "/repos/acme/skills/git/trees/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"truncated": false,
				"tree": []map[string]any{
					{"path": "README.md", "type": "blob", "size": 10, "url": api.URL + "/blobs/sha-unrelated"},
					{"path": "pack/skills/gh-skill/SKILL.md", "type": "blob", "size": 30, "url": api.URL + "/blobs/sha-manifest"},
					{"path": "pack/skills/gh-skill/helper.py", "type": "blob", "size": 16, "url": api.URL + "/blobs/sha-helper"},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/blobs/"):
			sha := strings.TrimPrefix(r.URL.Path, "/blobs/")
			body, ok := blobs[sha]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(body)),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	workspace := t.TempDir()
	res, err := installGitHubSkillFrom(context.Background(), api.Client(), api.URL, workspace, "acme/skills", "gh-skill")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.Installed {
		t.Fatalf("response=%+v", res)
	}
	if _, err := os.Stat(filepath.Join(workspace, "skills", "gh-skill", "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "skills", "gh-skill", "helper.py")); err != nil {
		t.Fatalf("helper.py missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "skills", "gh-skill", "README.md")); !os.IsNotExist(err) {
		t.Fatal("file outside the skill directory was installed")
	}
}

func TestResolveSkillRootPrefersShallowestCandidate(t *testing.T) {
	tree := githubTree{Tree: []githubTreeEntry{
		{Path: "pack/skills/demo/SKILL.md", Type: "blob"},
		{Path: "demo/SKILL.md", Type: "blob"},
		{Path: "demo/extra.md", Type: "blob"},
	}}
	root, err := resolveSkillRoot(tree, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if root != "demo" {
		t.Fatalf("root=%q, want demo", root)
	}
	if _, err := resolveSkillRoot(githubTree{}, "demo"); err == nil {
		t.Fatal("want error for a repository without the skill")
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	base := t.TempDir()
	for _, rel := range []string{"../evil", "a/../../evil", ""} {
		if _, err := safeJoin(base, rel); err == nil {
			t.Fatalf("safeJoin(%q) succeeded, want rejection", rel)
		}
	}
	got, err := safeJoin(base, "a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, base) {
		t.Fatalf("safeJoin returned %q outside %q", got, base)
	}
}

func TestUninstallRejectsUnsafeNames(t *testing.T) {
	svc := NewService(Options{Workspace: t.TempDir(), AllowRemoteInstall: true})
	for _, name := range []string{"", "../escape", "a/b", `a\b`} {
		if err := svc.Uninstall(context.Background(), name); err == nil {
			t.Fatalf("Uninstall(%q) succeeded, want rejection", name)
		}
	}
}

func TestReadOnlyProviderIsNotInstallable(t *testing.T) {
	svc := NewService(Options{Workspace: t.TempDir(), AllowRemoteInstall: true})
	_, err := svc.Install(context.Background(), InstallRequest{Provider: "cliapps", SkillID: "feishu"})
	if err == nil {
		t.Fatal("Install(cliapps) succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), "read-only catalogue") {
		t.Fatalf("err=%v, want the read-only catalogue explanation", err)
	}

	// The catalogue entry must not advertise an installer either.
	item := convertCliApp(cliAppEntry{Name: "feishu", Description: "chat"})
	if item.InstallSupported {
		t.Fatal("CLI Apps entry advertises install_supported=true")
	}
}
