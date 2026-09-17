package pairing

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// StoreIOError reports a transient store I/O failure.
//
// It is the Go analogue of Python's OSError: the mutating operations propagate
// it so callers fail loudly instead of persisting an empty view of the store
// that would erase every approved sender, while the read-only checks treat it
// as "no data" and fail closed. BaseChannel._handle_message relies on being
// able to recognise exactly this class of failure (base.py:279-285).
type StoreIOError struct {
	// Op is the failed operation: "read", "write", "mkdir", "rename" or "random".
	Op string
	// Path is the file the operation was applied to, when known.
	Path string
	// Err is the underlying error.
	Err error
}

// Error renders the failure with the store path exactly once.
//
// The underlying error is usually an *fs.PathError, whose own message repeats
// the operation and path ("read /x/pairing.json: is a directory"); using only
// its cause keeps the operator-facing message free of that duplication.
func (e *StoreIOError) Error() string {
	detail := error(e.Err)
	var pathErr *fs.PathError
	if errors.As(e.Err, &pathErr) {
		detail = pathErr.Err
	}
	if e.Path == "" {
		return fmt.Sprintf("pairing: %s: %v", e.Op, detail)
	}
	return fmt.Sprintf("pairing: %s %s: %v", e.Op, e.Path, detail)
}

func (e *StoreIOError) Unwrap() error { return e.Err }

// IsStoreIOError reports whether err is a transient pairing-store I/O failure.
func IsStoreIOError(err error) bool {
	var target *StoreIOError
	return errors.As(err, &target)
}

// Store is a pairing store backed by one JSON file.
//
// The reference serialises every operation behind a single module-global
// threading.Lock (store.py:26) because the store is callable from both the sync
// CLI and async channel handlers. Go needs the same mutual exclusion for a
// different reason: read-modify-write of one file from concurrent goroutines
// would lose approvals. The lock is per Store rather than global, which is
// strictly safer — two stores never share a file by construction.
type Store struct {
	mu     sync.Mutex
	path   string
	now    func() float64
	logger *slog.Logger
}

// NewStore returns a store backed by path.
//
// An empty path selects DefaultPath() on every operation, mirroring the
// reference, which re-resolves get_data_dir()/pairing.json on each call
// (store.py:32-33) instead of caching it.
func NewStore(path string) *Store { return &Store{path: path} }

// DefaultPath returns the pairing store path:
// get_data_dir() / "pairing.json" (store.py:32-33), i.e. the parent directory
// of the configuration file.
func DefaultPath() string { return filepath.Join(config.DefaultDataDir(), "pairing.json") }

// Path returns the file the store operates on.
func (s *Store) Path() string { return s.filePath() }

// SetClock replaces the time source used for code timestamps, expiry checks and
// format_expiry. The reference reads time.time() directly; a caller that needs
// deterministic output (tests, fixtures) can supply its own source here.
func (s *Store) SetClock(now func() float64) { s.now = now }

// SetLogger replaces the diagnostic logger. The default is slog.Default().
func (s *Store) SetLogger(logger *slog.Logger) { s.logger = logger }

func (s *Store) filePath() string {
	if s.path != "" {
		return s.path
	}
	return DefaultPath()
}

// clock returns the current time as Unix seconds, the Go equivalent of
// time.time().
func (s *Store) clock() float64 {
	if s.now != nil {
		return s.now()
	}
	return float64(time.Now().UnixNano()) / 1e9
}

func (s *Store) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// ---------------------------------------------------------------------------
// In-memory view
// ---------------------------------------------------------------------------

// storeData is the loaded view of pairing.json.
//
// _load (store.py:36) returns the whole decoded dict, but _save (store.py:73)
// only ever writes "approved" and "pending", so unknown top-level keys are
// dropped on the first write — reproduced here by simply not keeping them.
//
// pending values are `any` rather than *jsonObject on purpose: _load does NOT
// filter malformed pending entries, and the operations that save without
// garbage-collecting first (revoke, revoke_channel) must preserve them.
type storeData struct {
	approvedOrder []string
	approved      map[string]map[string]struct{}

	pendingOrder []string
	pending      map[string]any
}

func newStoreData() *storeData {
	return &storeData{
		approved: map[string]map[string]struct{}{},
		pending:  map[string]any{},
	}
}

// setApproved adds sender to channel's set, creating the channel if needed.
// Python: data.setdefault("approved", {}).setdefault(channel, set()).add(sender)
func (d *storeData) setApproved(channel, sender string) {
	set, ok := d.approved[channel]
	if !ok {
		set = map[string]struct{}{}
		d.approved[channel] = set
		d.approvedOrder = append(d.approvedOrder, channel)
	}
	set[sender] = struct{}{}
}

// delApproved removes a channel entirely, matching `del approved[channel]`.
func (d *storeData) delApproved(channel string) {
	if _, ok := d.approved[channel]; !ok {
		return
	}
	delete(d.approved, channel)
	d.approvedOrder = removeString(d.approvedOrder, channel)
}

// setPending stores a pending entry. An existing code keeps its position in the
// file, matching dict assignment in Python: data.setdefault("pending", {})[code]
// = {...} (store.py:133). A code collision (possible but astronomically
// unlikely) must therefore overwrite the entry without duplicating the key.
func (d *storeData) setPending(code string, entry *jsonObject) {
	if _, ok := d.pending[code]; !ok {
		d.pendingOrder = append(d.pendingOrder, code)
	}
	d.pending[code] = entry
}

// delPending removes a pending code.
func (d *storeData) delPending(code string) {
	if _, ok := d.pending[code]; !ok {
		return
	}
	delete(d.pending, code)
	d.pendingOrder = removeString(d.pendingOrder, code)
}

func removeString(list []string, value string) []string {
	for i, item := range list {
		if item == value {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// ---------------------------------------------------------------------------
// Load / save
// ---------------------------------------------------------------------------

// load ports _load (store.py:36-70).
//
// A missing file is an empty store, malformed JSON is corruption that resets
// the in-memory view (the file itself is left untouched until the next save),
// and any other I/O failure is propagated so mutating callers fail loudly
// instead of persisting an empty view that would erase every approved sender.
func (s *Store) load() (*storeData, error) {
	path := s.filePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return newStoreData(), nil
		}
		s.log().Warn("Pairing store temporarily unreadable", "path", path)
		return nil, &StoreIOError{Op: "read", Path: path, Err: err}
	}

	doc, err := decodeJSONDocument(raw)
	if err != nil {
		s.log().Warn("Corrupted pairing store, resetting", "path", path)
		return newStoreData(), nil
	}
	root, ok := doc.(*jsonObject)
	if !ok {
		s.log().Warn("Corrupted pairing store, resetting", "path", path)
		return newStoreData(), nil
	}

	data := newStoreData()

	// JSON stores may contain null or malformed maps after partial edits;
	// treat like {} (store.py:56-63).
	if approved, ok := root.get("approved").(*jsonObject); ok {
		for _, channel := range approved.keys {
			users, _ := approved.vals[channel].([]any) // a non-list becomes []
			set := make(map[string]struct{}, len(users))
			for _, user := range users {
				set[pyStr(user)] = struct{}{}
			}
			data.approvedOrder = append(data.approvedOrder, channel)
			data.approved[channel] = set
		}
	}
	if pending, ok := root.get("pending").(*jsonObject); ok {
		for _, code := range pending.keys {
			data.pendingOrder = append(data.pendingOrder, code)
			data.pending[code] = pending.vals[code]
		}
	}
	return data, nil
}

// save ports _save (store.py:73-85).
func (s *Store) save(data *storeData) error {
	path := s.filePath()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return &StoreIOError{Op: "mkdir", Path: dir, Err: err}
		}
	}

	root := newJSONObject()

	// Convert sets back to sorted lists for JSON serialization.
	approved := newJSONObject()
	for _, channel := range data.approvedOrder {
		set := data.approved[channel]
		users := make([]string, 0, len(set))
		for user := range set {
			users = append(users, user)
		}
		sort.Strings(users)
		list := make([]any, len(users))
		for i, user := range users {
			list[i] = user
		}
		approved.set(channel, list)
	}
	root.set("approved", approved)

	pending := newJSONObject()
	for _, code := range data.pendingOrder {
		pending.set(code, data.pending[code])
	}
	root.set("pending", pending)

	return writeFileAtomic(path, []byte(encodeJSONDocument(root)))
}

// writeFileAtomic ports utils/helpers._write_text_atomic (helpers.py:556-577):
// write a sibling temp file, fsync it, rename over the target, then fsync the
// directory, preserving the target's existing permission bits.
//
// DIVERGENCE (documented, not silent): when the target does not yet exist,
// Python creates the temp file with the process umask (0644 typically) while
// os.CreateTemp uses 0600, so a freshly created pairing.json is more private
// here. Once the file exists its mode is preserved, so the two runtimes agree
// on every subsequent write. This matches the existing helper in
// internal/config/loader.go rather than inventing a second convention.
func writeFileAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	existing, statErr := os.Stat(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return &StoreIOError{Op: "write", Path: path, Err: err}
	}
	tmpName := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if statErr == nil {
		_ = tmp.Chmod(existing.Mode().Perm())
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return &StoreIOError{Op: "write", Path: tmpName, Err: err}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return &StoreIOError{Op: "write", Path: tmpName, Err: err}
	}
	if err := tmp.Close(); err != nil {
		return &StoreIOError{Op: "write", Path: tmpName, Err: err}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return &StoreIOError{Op: "rename", Path: path, Err: err}
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// gcPending ports _gc_pending (store.py:88-110): remove expired or malformed
// pending entries in place.
//
// It does NOT save; the reference only persists the removal as a side effect of
// the operation that happens to write next. A caller that lists pending
// requests therefore still sees the expired entry on disk afterwards.
func gcPending(data *storeData, now float64) {
	var expired []string
	for _, code := range data.pendingOrder {
		info, ok := data.pending[code].(*jsonObject)
		if !ok {
			expired = append(expired, code)
			continue
		}
		channel, isString := info.get("channel").(string)
		expiresAt, isNumber := numericValue(info.get("expires_at"))
		if !isString || channel == "" || info.get("sender_id") == nil || !isNumber || expiresAt < now {
			expired = append(expired, code)
		}
	}
	for _, code := range expired {
		data.delPending(code)
	}
}

// numericValue reports the numeric value of v.
//
// Python accepts int and float but explicitly rejects bool, which is a subclass
// of int. In this package bools decode to Go bool, so the type switch already
// excludes them; the exclusion is stated here so the intent survives a refactor
// that starts representing booleans as numbers.
func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		return 0, false
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

// Approval identifies a sender approved on a channel. Python returns the tuple
// (channel, sender_id).
type Approval struct {
	Channel  string
	SenderID string
}

// ClearResult is the return value of ClearChannel.
// Python returns {"approved": int, "pending": int}.
type ClearResult struct {
	Approved int
	Pending  int
}

// PendingRequest is one non-expired pending pairing request.
//
// It mirrors the dict built by list_pending (store.py:202-206), which merges
// the code into the stored entry. Channel, SenderID, CreatedAt and ExpiresAt
// keep the raw stored values: the reference does not coerce them, so a
// hand-edited entry can hold a number where a string is expected. Use PyStr to
// render one the way the Python command handler does.
type PendingRequest struct {
	Code      string
	Channel   any
	SenderID  any
	CreatedAt any
	ExpiresAt any
	// Extra holds any additional keys the stored entry carried, so a
	// round-trip through this API does not silently drop data.
	Extra map[string]any

	// present records which of the four standard keys the stored entry
	// actually had, because Python's dict merge copies only the keys that
	// exist: an entry without created_at produces a dict without the key, not
	// one holding null.
	present map[string]bool
}

// Map returns the request as the dict the reference returns, including only the
// keys the stored entry actually carried.
func (p PendingRequest) Map() map[string]any {
	out := make(map[string]any, 5+len(p.Extra))
	out["code"] = p.Code
	if p.present["channel"] {
		out["channel"] = p.Channel
	}
	if p.present["sender_id"] {
		out["sender_id"] = p.SenderID
	}
	if p.present["created_at"] {
		out["created_at"] = p.CreatedAt
	}
	if p.present["expires_at"] {
		out["expires_at"] = p.ExpiresAt
	}
	for k, v := range p.Extra {
		out[k] = v
	}
	return out
}

// GenerateCode returns an active pairing code for senderID on channel.
//
// An existing non-expired code for the same (channel, senderID) pair is
// returned unchanged, so repeated DMs do not mint new codes. Port of
// generate_code (store.py:113-141).
func (s *Store) GenerateCode(channel, senderID string, ttlSeconds int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return "", err
	}
	now := s.clock()
	gcPending(data, now)

	sender := senderID
	for _, code := range data.pendingOrder {
		info, ok := data.pending[code].(*jsonObject)
		if !ok {
			continue
		}
		storedChannel, _ := info.get("channel").(string)
		if storedChannel == channel && pyStr(info.get("sender_id")) == sender {
			return code, nil
		}
	}

	code, err := randomCode()
	if err != nil {
		return "", err
	}
	entry := newJSONObject()
	entry.set("channel", channel)
	entry.set("sender_id", sender)
	entry.set("created_at", json.Number(pyFloatRepr(now)))
	entry.set("expires_at", json.Number(pyFloatRepr(now+float64(ttlSeconds))))
	data.setPending(code, entry)

	if err := s.save(data); err != nil {
		return "", err
	}
	s.log().Info("Generated pairing code", "code", code, "sender_id", senderID, "channel", channel)
	return code, nil
}

// ApproveCode approves a pending pairing code and returns the sender it
// approved.
//
// The bool is false when the code does not exist or has expired; in that case
// nothing is written. Port of approve_code (store.py:144-162).
func (s *Store) ApproveCode(code string) (Approval, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return Approval{}, false, err
	}
	gcPending(data, s.clock())

	info, ok := data.pending[code].(*jsonObject)
	if !ok {
		// Absent, or a malformed entry that gc already dropped.
		return Approval{}, false, nil
	}
	data.delPending(code)

	channel, _ := info.get("channel").(string)
	senderID := pyStr(info.get("sender_id"))
	data.setApproved(channel, senderID)

	if err := s.save(data); err != nil {
		return Approval{}, false, err
	}
	s.log().Info("Approved pairing code", "code", code, "sender_id", senderID, "channel", channel)
	return Approval{Channel: channel, SenderID: senderID}, true, nil
}

// DenyCode rejects and discards a pending pairing code, reporting whether the
// code existed. Port of deny_code (store.py:165-179).
func (s *Store) DenyCode(code string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return false, err
	}
	gcPending(data, s.clock())

	if _, ok := data.pending[code]; !ok {
		return false, nil
	}
	data.delPending(code)
	if err := s.save(data); err != nil {
		return false, err
	}
	s.log().Info("Denied pairing code", "code", code)
	return true, nil
}

// IsApproved reports whether senderID has been approved on channel.
//
// An unreadable store fails closed: the check returns false and the store
// itself is left untouched (store.py:184-191). There is deliberately no error
// return, because the reference has none — a caller cannot distinguish "not
// approved" from "store unreadable", and inventing that distinction here would
// change the authorization decision the reference makes.
func (s *Store) IsApproved(channel, senderID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return false
	}
	_, ok := data.approved[channel][senderID]
	return ok
}

// ListPending returns all non-expired pending pairing requests.
//
// An unreadable store yields an empty list (store.py:196-200).
func (s *Store) ListPending() []PendingRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return []PendingRequest{}
	}
	gcPending(data, s.clock())

	out := make([]PendingRequest, 0, len(data.pendingOrder))
	for _, code := range data.pendingOrder {
		info, ok := data.pending[code].(*jsonObject)
		if !ok {
			// Unreachable after gcPending, which drops non-object entries.
			continue
		}
		req := PendingRequest{
			Code:      code,
			Channel:   info.get("channel"),
			SenderID:  info.get("sender_id"),
			CreatedAt: info.get("created_at"),
			ExpiresAt: info.get("expires_at"),
			present:   map[string]bool{},
		}
		for _, key := range info.keys {
			switch key {
			case "channel", "sender_id", "created_at", "expires_at":
				req.present[key] = true
			default:
				if req.Extra == nil {
					req.Extra = map[string]any{}
				}
				req.Extra[key] = info.vals[key]
			}
		}
		out = append(out, req)
	}
	return out
}

// Revoke removes an approved sender from channel, reporting whether the sender
// was present. Port of revoke (store.py:209-226).
func (s *Store) Revoke(channel, senderID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return false, err
	}
	users, ok := data.approved[channel]
	if !ok {
		return false, nil
	}
	if _, ok := users[senderID]; !ok {
		return false, nil
	}
	delete(users, senderID)
	if len(users) == 0 {
		data.delApproved(channel)
	}
	if err := s.save(data); err != nil {
		return false, err
	}
	s.log().Info("Revoked sender", "sender_id", senderID, "channel", channel)
	return true, nil
}

// RevokeChannel removes all approved sender IDs for channel and returns how
// many were removed.
//
// A channel that holds no approved senders is removed from the in-memory view
// but NOT persisted, matching the reference's early return (store.py:238-239):
// the empty entry survives on disk until some other operation writes.
func (s *Store) RevokeChannel(channel string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return 0, err
	}
	users, ok := data.approved[channel]
	if !ok || len(users) == 0 {
		data.delApproved(channel)
		return 0, nil
	}
	count := len(users)
	data.delApproved(channel)
	if err := s.save(data); err != nil {
		return 0, err
	}
	s.log().Info("Revoked approved senders", "count", count, "channel", channel)
	return count, nil
}

// ClearChannel removes approved senders and pending requests for channel.
// Port of clear_channel (store.py:245-272).
func (s *Store) ClearChannel(channel string) (ClearResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return ClearResult{}, err
	}
	gcPending(data, s.clock())

	approvedUsers := data.approved[channel]
	data.delApproved(channel)

	var pendingCodes []string
	for _, code := range data.pendingOrder {
		info, ok := data.pending[code].(*jsonObject)
		if !ok {
			continue
		}
		if pyStr(info.get("channel")) == channel {
			pendingCodes = append(pendingCodes, code)
		}
	}
	for _, code := range pendingCodes {
		data.delPending(code)
	}

	if len(approvedUsers) == 0 && len(pendingCodes) == 0 {
		return ClearResult{}, nil
	}
	if err := s.save(data); err != nil {
		return ClearResult{}, err
	}
	s.log().Info("Cleared channel", "approved", len(approvedUsers), "pending", len(pendingCodes), "channel", channel)
	return ClearResult{Approved: len(approvedUsers), Pending: len(pendingCodes)}, nil
}

// GetApproved returns all approved sender IDs for channel, sorted.
// An unreadable store yields an empty list (store.py:278-282).
func (s *Store) GetApproved(channel string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return []string{}
	}
	users := data.approved[channel]
	out := make([]string, 0, len(users))
	for user := range users {
		out = append(out, user)
	}
	sort.Strings(out)
	return out
}

// randomCode returns a fresh code such as "ABCD-EFGH".
//
// secrets.choice(_ALPHABET) draws uniformly from the 36-character alphabet via
// rejection sampling; 252 is the largest multiple of 36 below 256, so bytes at
// or above it are redrawn. A modulo without that rejection would bias the first
// four letters of the alphabet.
func randomCode() (string, error) {
	buf := make([]byte, CodeLength)
	var one [1]byte
	for i := 0; i < CodeLength; {
		if _, err := rand.Read(one[:]); err != nil {
			return "", &StoreIOError{Op: "random", Err: err}
		}
		if int(one[0]) >= 256-(256%len(CodeAlphabet)) {
			continue
		}
		buf[i] = CodeAlphabet[int(one[0])%len(CodeAlphabet)]
		i++
	}
	return string(buf[:4]) + "-" + string(buf[4:]), nil
}
