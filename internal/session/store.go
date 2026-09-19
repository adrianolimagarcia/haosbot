package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Layout constants, mirroring nanobot/session/manager.py:71-76.
const (
	workspaceStateDir      = ".nanobot"
	workspaceIDFile        = "workspace-id"
	sessionFilesLockName   = ".session-files.lock"
	workspaceMigrationLock = ".workspace-migration.lock"
	workspaceMarkerName    = ".workspace"
	sessionSuffix          = ".jsonl"
	journalSuffix          = ".journal.jsonl"
	checkpointSuffix       = ".checkpoint.json"
	migrationLockTimeout   = 30 * time.Second
	sessionCacheMaxEntries = 4
	sessionCacheMaxBytes   = 256 << 10
)

var workspaceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Store persists sessions as one JSONL file per session key.
//
// Files live under <sessionsRoot>/<workspaceID>/, matching the reference
// layout (manager.py:551-585, :579). The workspace id is the value of
// <workspace>/.nanobot/workspace-id (32 lowercase hex characters,
// manager.py:71-73), so a Go process and a Python process pointed at the same
// workspace and sessions root read and write the same files.
//
// Store is safe for concurrent use by multiple goroutines. It is also safe
// alongside a live Python process on POSIX: every operation takes the same
// .session-files.lock advisory lock that filelock takes.
type Store struct {
	root      string
	workspace string
	dir       string
	initErr   error
	cacheMu   sync.Mutex
	cache     map[string]cachedSession
	cacheSeq  uint64
}

type cachedSession struct {
	sess          *Session
	exists        bool
	size          int64
	modTime       time.Time
	journalExists bool
	journalSize   int64
	journalModTime time.Time
	used          uint64
}

// NewStore returns a store for workspace, persisting under sessionsRoot.
//
//   - sessionsRoot == "" selects the reference default, $HOME/.nanobot/sessions
//     (config/paths.py:15-22, loader.py:35-39).
//   - workspace == "" disables the workspace namespace, so Dir() is
//     sessionsRoot itself. The reference always has a workspace; this form
//     exists for Go-only deployments and tests.
//
// Like the reference, NewStore creates directories and the workspace identity
// marker as a side effect. The reference raises RuntimeError when the sessions
// root is inside the workspace; because this constructor cannot return an
// error, the failure is recorded and returned by every method that touches
// storage, so the store fails closed rather than writing into the workspace.
func NewStore(workspace, sessionsRoot string) *Store {
	s := &Store{cache: make(map[string]cachedSession)}
	ws, err := canonicalPath(workspace)
	if err != nil {
		s.initErr = err
		return s
	}
	s.workspace = ws

	root := sessionsRoot
	if strings.TrimSpace(root) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			s.initErr = fmt.Errorf("session: cannot resolve default sessions root: %w", err)
			return s
		}
		root = filepath.Join(home, ".nanobot", "sessions")
	}
	root, err = canonicalPath(root)
	if err != nil {
		s.initErr = err
		return s
	}
	s.root = root

	if s.workspace != "" && pathWithin(root, s.workspace) {
		s.initErr = fmt.Errorf(
			"session: session storage must be outside the agent workspace; "+
				"move --config outside --workspace or choose a nested workspace directory "+
				"(root %q is inside workspace %q)", root, s.workspace)
		s.dir = root
		return s
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		s.initErr = fmt.Errorf("session: create sessions root: %w", err)
		s.dir = root
		return s
	}
	// Best-effort, matching manager.py:565-566.
	_ = os.Chmod(root, 0o700)

	if s.workspace == "" {
		s.dir = root
		return s
	}

	// Workspace-id allocation is guarded by the migration lock, exactly like
	// the reference (manager.py:568-585).
	lock, err := acquireFileLock(filepath.Join(root, workspaceMigrationLock), migrationLockTimeout)
	if err != nil {
		s.initErr = fmt.Errorf("session: lock workspace namespace: %w", err)
		s.dir = root
		return s
	}
	defer lock.release()

	id, err := resolveWorkspaceID(root, s.workspace)
	if err != nil {
		s.initErr = err
		s.dir = root
		return s
	}
	id, err = claimWorkspaceNamespace(root, s.workspace, id)
	if err != nil {
		s.initErr = err
		s.dir = root
		return s
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.initErr = fmt.Errorf("session: create session namespace: %w", err)
		s.dir = root
		return s
	}
	s.dir = dir
	if err := ensureWorkspaceMarker(dir, s.workspace); err != nil {
		s.initErr = err
	}
	return s
}

// Dir returns the directory holding this store's session files.
func (s *Store) Dir() string { return s.dir }

// Path returns the canonical file path for a session key.
//
// Mirrors JsonlSessionStore.get_session_path (manager.py:1026-1027).
func (s *Store) Path(key string) string {
	return filepath.Join(s.dir, StorageKey(key)+sessionSuffix)
}

// checkpointPath returns the runtime-checkpoint sidecar path for a key.
//
// Mirrors get_runtime_checkpoint_path (manager.py:1029-1030).
func (s *Store) checkpointPath(key string) string {
	return filepath.Join(s.dir, StorageKey(key)+checkpointSuffix)
}

// journalPath is the append-only fast-path sidecar. Normal interactive turns
// append only the new records here; a background full Save compacts the journal
// back into the canonical JSONL. The sidecar is intentionally private to the Go
// runtime and is replayed before a session is returned.
func (s *Store) journalPath(key string) string {
	return filepath.Join(s.dir, StorageKey(key)+journalSuffix)
}

// legacyLossyPath returns the retired in-directory path for a key. It is only
// ever unlinked (manager.py:1032-1033, :1410-1416); it is never read or
// written.
func (s *Store) legacyLossyPath(key string) string {
	return filepath.Join(s.dir, safeKey(key)+sessionSuffix)
}

// Open returns the session for key.
//
// A missing file yields a new empty session and no error, matching
// SessionManager.get_or_create. A file that cannot be read at all (permissions,
// I/O) returns the error: the reference's _SESSION_DATA_ERRORS does not include
// OSError, so it propagates there too.
//
// A file whose lines are not all valid JSON objects is read tolerantly: the
// unusable lines are skipped and the file is left untouched, which is what the
// reference's repair path does (manager.py:1120-1199 — repair never writes).
// A line that is a valid JSON object but not a shape core.Message can model is
// preserved verbatim as an opaque record rather than dropped, because the
// reference's strict load appends any JSON object to the message list without
// validating it (manager.py:1089-1090).
func (s *Store) Open(key string) (*Session, error) {
	if err := s.initErr; err != nil {
		return nil, err
	}
	var sess *Session
	err := s.withLock(func() error {
		path := s.Path(key)
		info, statErr := os.Stat(path)
		exists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("session: stat %s: %w", path, statErr)
		}
		_, checkpointErr := os.Stat(s.checkpointPath(key))
		journalInfo, journalErr := os.Stat(s.journalPath(key))
		journalExists := journalErr == nil
		if journalErr != nil && !os.IsNotExist(journalErr) {
			return fmt.Errorf("session: stat journal: %w", journalErr)
		}
		// Cache validation includes BOTH the canonical file and journal. That
		// keeps a warm session O(1) while still detecting another process that
		// appended to the journal.
		if checkpointErr != nil && os.IsNotExist(checkpointErr) {
			if cached, ok := s.cachedSession(key, exists, info, journalExists, journalInfo); ok {
				sess = cached
				return nil
			}
		}
		loaded, err := s.loadLocked(key)
		if err != nil {
			return err
		}
		sess = loaded
		s.rememberSession(key, loaded, exists, info, journalExists, journalInfo)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// List returns the session keys stored in this directory.
//
// Mirrors _list_sessions_unlocked (manager.py:1543-1641): only files whose stem
// is canonical base64url-nopad are considered, hidden files are skipped (as
// Python's glob does), the first line must be a metadata record, and the stored
// key wins over the filename when it is a non-empty string. The result is
// ordered by updated_at descending, like the reference, with the key as a
// deterministic tie-break.
func (s *Store) List() ([]string, error) {
	if err := s.initErr; err != nil {
		return nil, err
	}
	var keys []string
	err := s.withLock(func() error {
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return err
		}
		type row struct {
			key     string
			updated string
		}
		byKey := make(map[string]row, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || entry.IsDir() {
				continue
			}

			// Canonical session file.
			if strings.HasSuffix(name, sessionSuffix) && !strings.HasSuffix(name, journalSuffix) {
				stem := strings.TrimSuffix(name, sessionSuffix)
				key, ok := SessionKeyFromStem(stem)
				if !ok {
					continue
				}
				updated, ok := s.firstRecordUpdatedAt(filepath.Join(s.dir, name), key)
				if !ok {
					continue
				}
				byKey[key] = row{key: key, updated: updated}
				continue
			}

			// Journal-only sessions must remain discoverable before the
			// background compactor has produced the canonical JSONL.
			if strings.HasSuffix(name, journalSuffix) {
				stem := strings.TrimSuffix(name, journalSuffix)
				key, ok := SessionKeyFromStem(stem)
				if !ok {
					continue
				}
				info, statErr := entry.Info()
				if statErr != nil {
					continue
				}
				updated := formatNaive(info.ModTime())
				if prev, exists := byKey[key]; !exists || updated > prev.updated {
					byKey[key] = row{key: key, updated: updated}
				}
			}
		}
		rows := make([]row, 0, len(byKey))
		for _, r := range byKey {
			// A journal newer than the base determines the effective update
			// time even when both files exist.
			if info, err := os.Stat(s.journalPath(r.key)); err == nil {
				if journalUpdated := formatNaive(info.ModTime()); journalUpdated > r.updated {
					r.updated = journalUpdated
				}
			}
			rows = append(rows, r)
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].updated != rows[j].updated {
				return rows[i].updated > rows[j].updated
			}
			return rows[i].key < rows[j].key
		})
		keys = make([]string, 0, len(rows))
		for _, r := range rows {
			keys = append(keys, r.key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// Delete removes the session for key.
//
// It unlinks the canonical file, the runtime-checkpoint sidecar and the retired
// lossy path in the same directory, mirroring _delete_unlocked
// (manager.py:1410-1426). A key that has no files is not an error.
//
// DELIBERATE DIVERGENCE: the reference additionally unlinks
// ~/.nanobot/sessions/<safe_key>.jsonl (the pre-relocation global path,
// manager.py:1035-1036, :1415). This implementation does not touch files
// outside the store's own directory. Nothing reads that path — the reference
// never loads from it (spec §1.4) and its list_sessions never scans it — so
// leaving it behind only leaves a stale file on disk.
func (s *Store) Delete(key string) error {
	if err := s.initErr; err != nil {
		return err
	}
	return s.withLock(func() error {
		defer s.forgetSession(key)
		var firstErr error
		for _, path := range []string{s.Path(key), s.journalPath(key), s.checkpointPath(key), s.legacyLossyPath(key)} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				if firstErr == nil {
					firstErr = fmt.Errorf("session: delete %s: %w", path, err)
				}
			}
		}
		return firstErr
	})
}

func (s *Store) cachedSession(key string, exists bool, info os.FileInfo, journalExists bool, journalInfo os.FileInfo) (*Session, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	cached, ok := s.cache[key]
	if !ok || cached.exists != exists || cached.journalExists != journalExists {
		return nil, false
	}
	if exists && (info == nil || cached.size != info.Size() || !cached.modTime.Equal(info.ModTime())) {
		delete(s.cache, key)
		return nil, false
	}
	if journalExists && (journalInfo == nil || cached.journalSize != journalInfo.Size() || !cached.journalModTime.Equal(journalInfo.ModTime())) {
		delete(s.cache, key)
		return nil, false
	}
	s.cacheSeq++
	cached.used = s.cacheSeq
	s.cache[key] = cached
	return cached.sess, true
}

func (s *Store) rememberSession(key string, sess *Session, exists bool, info os.FileInfo, journalExists bool, journalInfo os.FileInfo) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	// The in-memory session is the hot copy. Bound cache entries by count; do
	// not evict a live long conversation merely because its canonical JSONL is
	// larger than the old 256 KiB threshold, otherwise every turn has to parse
	// the entire transcript again.
	s.cacheSeq++
	entry := cachedSession{sess: sess, exists: exists, journalExists: journalExists, used: s.cacheSeq}
	if info != nil {
		entry.size, entry.modTime = info.Size(), info.ModTime()
	}
	if journalInfo != nil {
		entry.journalSize, entry.journalModTime = journalInfo.Size(), journalInfo.ModTime()
	}
	if len(s.cache) >= sessionCacheMaxEntries {
		oldestKey := ""
		var oldest uint64
		for candidate, cached := range s.cache {
			if oldestKey == "" || cached.used < oldest {
				oldestKey, oldest = candidate, cached.used
			}
		}
		if oldestKey != "" && oldestKey != key {
			delete(s.cache, oldestKey)
		}
	}
	s.cache[key] = entry
}

func (s *Store) rememberCurrent(sess *Session) {
	path := s.Path(sess.Key())
	info, err := os.Stat(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		s.forgetSession(sess.Key())
		return
	}
	journalInfo, journalErr := os.Stat(s.journalPath(sess.Key()))
	journalExists := journalErr == nil
	if journalErr != nil && !os.IsNotExist(journalErr) {
		s.forgetSession(sess.Key())
		return
	}
	s.rememberSession(sess.Key(), sess, exists, info, journalExists, journalInfo)
}

func (s *Store) rememberSaved(sess *Session) {
	s.rememberCurrent(sess)
}

func (s *Store) forgetSession(key string) {
	s.cacheMu.Lock()
	delete(s.cache, key)
	s.cacheMu.Unlock()
}

// withLock runs fn while holding the session-files lock.
func (s *Store) withLock(fn func() error) error {
	if s.dir == "" {
		return errors.New("session: store has no session directory")
	}
	lock, err := acquireFileLock(filepath.Join(s.dir, sessionFilesLockName), 0)
	if err != nil {
		return err
	}
	defer lock.release()
	return fn()
}

// loadLocked reads a session file. The caller must hold the session-files lock.
func (s *Store) loadLocked(key string) (*Session, error) {
	path := s.Path(key)
	sess := s.newSession(key)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("session: read %s: %w", path, err)
		}
	} else if err := sess.consumeRecords(data); err != nil {
		return nil, err
	}

	// Replay the append-only journal after the canonical file. This is the
	// crash-recovery path for the latency fast path: if the process dies before
	// background compaction, every acknowledged message is still present.
	if journal, err := os.ReadFile(s.journalPath(key)); err == nil {
		if err := sess.consumeRecords(journal); err != nil {
			return nil, err
		}
		sess.journalSize = int64(len(journal))
		if info, statErr := os.Stat(s.journalPath(key)); statErr == nil {
			sess.updatedAt = info.ModTime()
			sess.updatedStr = formatNaive(info.ModTime())
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("session: read journal: %w", err)
	}

	// Session.__post_init__ resets an out-of-range archive offset to 0,
	// because a corrupt offset would hide the whole transcript
	// (manager.py:295-301).
	if sess.lastArchived < 0 || sess.lastArchived > len(sess.messages) {
		sess.lastArchived = 0
	}
	if _, err := os.Stat(path); err == nil {
		s.overlayCheckpointLocked(sess, path)
	}
	return sess, nil
}

// firstRecordUpdatedAt reads only the first record of a file, the way
// _list_sessions_unlocked does (manager.py:1552-1562). It reports false when
// the file must be skipped: a blank first line, or a first record that is not
// a metadata record.
func (s *Store) firstRecordUpdatedAt(path, fallbackKey string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", false
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	fields, err := objectFields([]byte(line))
	if err != nil {
		return "", false
	}
	recordType, _ := stringField(fields, "_type")
	if recordType != "metadata" {
		return "", false
	}
	updated, ok := stringField(fields, "updated_at")
	if !ok || updated == "" {
		// Fall back to the file mtime, like manager.py:1597.
		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", false
		}
		updated = formatNaive(info.ModTime())
	}
	return updated, true
}

// writeAtomic writes data to path via an exclusive temp file and a rename,
// mirroring _save_unlocked (manager.py:1316-1344).
//
// Like the reference's default (fsync=False), no fsync is issued: the rename is
// atomic with respect to readers, but a crash immediately afterwards can lose
// the file.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+randomHex(8)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		f = nil
		return err
	}
	f = nil
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// randomHex returns n random bytes as 2n lowercase hex characters, matching
// secrets.token_hex(n).
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read never fails on supported platforms; fall back to a
		// time-derived value rather than panicking.
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// ---------------------------------------------------------------------------
// Workspace namespace (manager.py:551-760)
// ---------------------------------------------------------------------------

// canonicalPath mirrors Path(p).expanduser().resolve(strict=False): it expands
// a leading ~, makes the path absolute, and resolves symlinks in the longest
// existing prefix (leaving a non-existent suffix intact).
func canonicalPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	expanded, err := expandUser(p)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	rest := ""
	cur := abs
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(abs), nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// expandUser expands a leading "~" or "~/" to the user's home directory.
func expandUser(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("session: expand %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

// pathWithin reports whether child is parent or lives under it.
func pathWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveWorkspaceID loads, recovers or mints the workspace identity, mirroring
// _load_or_create_workspace_id (manager.py:676-690).
func resolveWorkspaceID(root, workspace string) (string, error) {
	marker := filepath.Join(workspace, workspaceStateDir, workspaceIDFile)
	if info, err := os.Lstat(marker); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("session: workspace identity marker must not be a symlink: %s", marker)
		}
		return readWorkspaceID(marker)
	}

	if recovered, ok := findWorkspaceNamespace(root, workspace); ok {
		if err := writeTextAtomic(marker, recovered+"\n", 0o600); err != nil {
			return "", err
		}
		return recovered, nil
	}

	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return "", fmt.Errorf("session: create workspace state dir: %w", err)
	}
	id := randomHex(16)
	// O_EXCL so a concurrent creator wins; O_NOFOLLOW is not portable and the
	// Lstat above already rejects a symlinked marker.
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return readWorkspaceID(marker)
		}
		return "", fmt.Errorf("session: create workspace identity marker: %w", err)
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(marker)
		return "", fmt.Errorf("session: write workspace identity marker: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(marker)
		return "", fmt.Errorf("session: write workspace identity marker: %w", err)
	}
	return id, nil
}

// readWorkspaceID validates a workspace-id marker (manager.py:620-629).
func readWorkspaceID(marker string) (string, error) {
	data, err := os.ReadFile(marker)
	if err != nil {
		return "", fmt.Errorf("session: read workspace identity marker: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if !workspaceIDRe.MatchString(value) {
		return "", fmt.Errorf(
			"session: workspace identity marker is invalid: %s; restore its original "+
				"32-character identifier before starting", marker)
	}
	return value, nil
}

// findWorkspaceNamespace recovers an identity marker that was removed, by
// looking for a namespace whose .workspace marker names this workspace
// (manager.py:639-668).
func findWorkspaceNamespace(root, workspace string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if !workspaceIDRe.MatchString(entry.Name()) || !entry.IsDir() {
			continue
		}
		marker := filepath.Join(root, entry.Name(), workspaceMarkerName)
		info, err := os.Lstat(marker)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		recorded, err := os.ReadFile(marker)
		if err != nil {
			continue
		}
		recordedPath, err := canonicalPath(strings.TrimSpace(string(recorded)))
		if err != nil || recordedPath == "" {
			continue
		}
		if recordedPath == workspace || sameFile(recordedPath, workspace) {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) > 1 {
		// The reference raises here; the constructor cannot, so the first
		// match is used and the ambiguity is not silently resolved by minting
		// a fresh identity (which would orphan the existing sessions).
		return matches[0], true
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}

// sameFile reports whether two paths refer to the same file.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// claimWorkspaceNamespace binds a workspace id to a namespace directory,
// rotating the id when a copied workspace claims one that belongs to a
// different, still-existing path (manager.py:712-760).
func claimWorkspaceNamespace(root, workspace, id string) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		dir := filepath.Join(root, id)
		marker := filepath.Join(dir, workspaceMarkerName)

		if info, err := os.Lstat(dir); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("session: session namespace must not be a symlink: %s", dir)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("session: session namespace is not a directory: %s", dir)
			}
			markerInfo, err := os.Lstat(marker)
			if err == nil {
				if markerInfo.Mode()&os.ModeSymlink != 0 {
					return "", fmt.Errorf("session: session workspace marker must not be a symlink: %s", marker)
				}
				recorded, readErr := os.ReadFile(marker)
				if readErr != nil {
					return "", fmt.Errorf("session: read session workspace marker: %w", readErr)
				}
				text := strings.TrimSpace(string(recorded))
				if text == "" {
					return "", fmt.Errorf("session: session workspace marker is empty: %s", marker)
				}
				recordedPath, err := canonicalPath(text)
				if err == nil {
					switch {
					case recordedPath == workspace:
						return id, nil
					case sameFile(recordedPath, workspace):
						if err := writeTextAtomic(marker, workspace+"\n", 0o600); err != nil {
							return "", err
						}
						return id, nil
					case !pathExists(recordedPath):
						// The identity marker travelled with a renamed or moved
						// workspace: re-adopt it.
						if err := writeTextAtomic(marker, workspace+"\n", 0o600); err != nil {
							return "", err
						}
						return id, nil
					}
				}
				// Both paths exist and differ: this is a copy, not a move.
				id = randomHex(16)
				if err := writeTextAtomic(
					filepath.Join(workspace, workspaceStateDir, workspaceIDFile),
					id+"\n", 0o600); err != nil {
					return "", err
				}
				continue
			}
			// No marker: an empty namespace is adopted, a populated one is
			// refused (manager.py:733-740).
			empty, err := dirIsEmpty(dir)
			if err != nil {
				return "", err
			}
			if !empty {
				return "", fmt.Errorf("session: session namespace has data but no workspace marker: %s", dir)
			}
			if err := ensureWorkspaceMarker(dir, workspace); err != nil {
				return "", err
			}
			return id, nil
		}

		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("session: create session namespace: %w", err)
		}
		if err := ensureWorkspaceMarker(dir, workspace); err != nil {
			return "", err
		}
		return id, nil
	}
	return "", fmt.Errorf("session: could not allocate an isolated session namespace for %s", workspace)
}

// ensureWorkspaceMarker writes <dir>/.workspace when it is missing.
func ensureWorkspaceMarker(dir, workspace string) error {
	marker := filepath.Join(dir, workspaceMarkerName)
	if _, err := os.Lstat(marker); err == nil {
		return nil
	}
	return writeTextAtomic(marker, workspace+"\n", 0o600)
}

// dirIsEmpty reports whether dir has no entries.
func dirIsEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// pathExists reports whether a path exists.
func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// writeTextAtomic mirrors _write_text_atomic (manager.py:604-617): a temp file
// with mode 0600, fsynced, then renamed.
func writeTextAtomic(path, content string, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+randomHex(8)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		f = nil
		return err
	}
	f = nil
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// ---------------------------------------------------------------------------
// Record helpers
// ---------------------------------------------------------------------------

// rawField is one member of a JSON object, in file order.
type rawField struct {
	key string
	val json.RawMessage
}

// objectFields decodes a JSON object while preserving member order.
func objectFields(data []byte) ([]rawField, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("session: session records must be JSON objects")
	}
	var fields []rawField
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("session: object key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		fields = append(fields, rawField{key: key, val: raw})
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return nil, err
	}
	return fields, nil
}

// fieldValue returns the last value for key, matching Python's dict semantics
// where a duplicate JSON key keeps the final value.
func fieldValue(fields []rawField, key string) (json.RawMessage, bool) {
	var val json.RawMessage
	found := false
	for _, f := range fields {
		if f.key == key {
			val = f.val
			found = true
		}
	}
	return val, found
}

// stringField returns a string member.
func stringField(fields []rawField, key string) (string, bool) {
	raw, ok := fieldValue(fields, key)
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// intField returns an integer member. Python's _archive_offset requires a real
// int and explicitly rejects bool, so "3.0" and "true" both fail here.
func intField(fields []rawField, key string) (int, bool) {
	raw, ok := fieldValue(fields, key)
	if !ok {
		return 0, false
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, false
	}
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// decodeOrderedObject decodes a JSON object into a map plus its key order.
// Numbers stay json.Number so they round-trip byte for byte.
func decodeOrderedObject(raw json.RawMessage) (map[string]any, []string, bool) {
	fields, err := objectFields(raw)
	if err != nil {
		return nil, nil, false
	}
	out := make(map[string]any, len(fields))
	order := make([]string, 0, len(fields))
	for _, f := range fields {
		dec := json.NewDecoder(bytes.NewReader(f.val))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false
		}
		if _, exists := out[f.key]; !exists {
			order = append(order, f.key)
		}
		out[f.key] = v
	}
	return out, order, true
}

// splitLines splits file content the way Python's universal-newline text mode
// does: "\n", "\r" and "\r\n" all end a line.
func splitLines(data []byte) []string {
	lines := make([]string, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			lines = append(lines, string(data[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(data[start:i]))
			if i+1 < len(data) && data[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}
