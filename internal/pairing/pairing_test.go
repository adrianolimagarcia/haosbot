package pairing

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore returns a store on a path inside the test's own directory. The
// file does not exist yet.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "pairing.json"))
	store.SetLogger(discardLogger())
	return store
}

// frozenStore returns a store whose clock is pinned, so expiry behaviour is
// tested against an exact number rather than wall-clock luck.
func frozenStore(t *testing.T, now float64) *Store {
	t.Helper()
	store := newTestStore(t)
	store.SetClock(func() float64 { return now })
	return store
}

func mustGenerate(t *testing.T, store *Store, channel, sender string, ttl int) string {
	t.Helper()
	code, err := store.GenerateCode(channel, sender, ttl)
	if err != nil {
		t.Fatalf("GenerateCode(%q, %q): %v", channel, sender, err)
	}
	return code
}

func readStore(t *testing.T, store *Store) string {
	t.Helper()
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Codes
// ---------------------------------------------------------------------------

func TestGeneratedCodeShapeAndAlphabet(t *testing.T) {
	store := newTestStore(t)

	// Draw enough codes that every alphabet character has an overwhelming
	// chance of appearing: 500 codes is 4000 draws over 36 symbols, so the
	// expected count per symbol is ~111.
	//
	// Each draw uses a DISTINCT sender: generate_code is idempotent per
	// (channel, sender), so reusing one sender would return a single code and
	// the alphabet coverage would be meaningless.
	seen := map[rune]int{}
	for i := 0; i < 500; i++ {
		code := mustGenerate(t, store, "telegram", "sender-"+strconv.Itoa(i), DefaultTTLSeconds)
		if len(code) != 9 {
			t.Fatalf("code %q has length %d, want 9", code, len(code))
		}
		if code[4] != '-' {
			t.Fatalf("code %q has no dash at index 4", code)
		}
		for i, r := range code {
			if i == 4 {
				continue
			}
			if !strings.ContainsRune(CodeAlphabet, r) {
				t.Fatalf("code %q contains %q, which is outside CodeAlphabet", code, r)
			}
			seen[r]++
		}
	}
	for _, r := range CodeAlphabet {
		if seen[r] == 0 {
			t.Errorf("alphabet character %q never appeared in 500 codes", r)
		}
	}
}

func TestGenerateCodeIsIdempotentPerSender(t *testing.T) {
	store := newTestStore(t)

	first := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)
	second := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)
	if first != second {
		t.Errorf("a second request from the same sender minted a new code: %q then %q", first, second)
	}

	// A different channel or a different sender is a different request.
	other := mustGenerate(t, store, "telegram", "bob", DefaultTTLSeconds)
	if other == first {
		t.Error("a different sender reused the first code")
	}
	otherChannel := mustGenerate(t, store, "discord", "alice", DefaultTTLSeconds)
	if otherChannel == first {
		t.Error("a different channel reused the first code")
	}
	if got := len(store.ListPending()); got != 3 {
		t.Errorf("pending = %d, want 3", got)
	}
}

func TestGenerateCodeReplacesExpiredRequest(t *testing.T) {
	store := frozenStore(t, 1000)

	first := mustGenerate(t, store, "telegram", "alice", 10)
	store.SetClock(func() float64 { return 1011 }) // past the 10s TTL
	second := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	if first == second {
		t.Error("an expired request was reused instead of replaced")
	}
	pending := store.ListPending()
	if len(pending) != 1 || pending[0].Code != second {
		t.Fatalf("pending = %+v, want only %q", pending, second)
	}
}

// ---------------------------------------------------------------------------
// Approval lifecycle
// ---------------------------------------------------------------------------

func TestApproveDenyRevokeLifecycle(t *testing.T) {
	store := newTestStore(t)
	code := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	if store.IsApproved("telegram", "alice") {
		t.Fatal("sender approved before the code was approved")
	}

	approval, ok, err := store.ApproveCode(code)
	if err != nil || !ok {
		t.Fatalf("ApproveCode = (%v, %v, %v), want ok", approval, ok, err)
	}
	if approval.Channel != "telegram" || approval.SenderID != "alice" {
		t.Errorf("approval = %+v, want telegram/alice", approval)
	}
	if !store.IsApproved("telegram", "alice") {
		t.Error("sender not approved after ApproveCode")
	}
	if store.IsApproved("discord", "alice") {
		t.Error("approval leaked to another channel")
	}
	if got := store.GetApproved("telegram"); len(got) != 1 || got[0] != "alice" {
		t.Errorf("GetApproved = %v, want [alice]", got)
	}

	// The code is consumed: approving again must not report success.
	if _, ok, err := store.ApproveCode(code); err != nil || ok {
		t.Errorf("second ApproveCode = (_, %v, %v), want false/nil", ok, err)
	}
	// And it is no longer pending.
	if got := len(store.ListPending()); got != 0 {
		t.Errorf("pending after approval = %d, want 0", got)
	}

	revoked, err := store.Revoke("telegram", "alice")
	if err != nil || !revoked {
		t.Fatalf("Revoke = (%v, %v), want true/nil", revoked, err)
	}
	if store.IsApproved("telegram", "alice") {
		t.Error("sender still approved after Revoke")
	}
	if revoked, err := store.Revoke("telegram", "alice"); err != nil || revoked {
		t.Errorf("second Revoke = (%v, %v), want false/nil", revoked, err)
	}
	if got := store.GetApproved("telegram"); len(got) != 0 {
		t.Errorf("GetApproved after revoke = %v, want empty", got)
	}
}

func TestApproveCodeRejectsUnknownAndMalformedCodes(t *testing.T) {
	store := newTestStore(t)
	mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	for _, code := range []string{"", "ZZZZZZZZ", "ABCD-EFGH", "not a code"} {
		approval, ok, err := store.ApproveCode(code)
		if err != nil {
			t.Errorf("ApproveCode(%q) returned error %v, want nil", code, err)
		}
		if ok {
			t.Errorf("ApproveCode(%q) reported success as %+v", code, approval)
		}
	}
	if got := len(store.ListPending()); got != 1 {
		t.Errorf("a failed approval consumed the pending request: %d left", got)
	}
}

func TestDenyCode(t *testing.T) {
	store := newTestStore(t)
	code := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	if denied, err := store.DenyCode("ZZZZZZZZ"); err != nil || denied {
		t.Errorf("DenyCode(unknown) = (%v, %v), want false/nil", denied, err)
	}
	if denied, err := store.DenyCode(code); err != nil || !denied {
		t.Errorf("DenyCode(code) = (%v, %v), want true/nil", denied, err)
	}
	if denied, err := store.DenyCode(code); err != nil || denied {
		t.Errorf("second DenyCode = (%v, %v), want false/nil", denied, err)
	}
	if got := len(store.ListPending()); got != 0 {
		t.Errorf("pending after deny = %d, want 0", got)
	}
}

func TestRevokeChannelAndClearChannel(t *testing.T) {
	store := newTestStore(t)
	for _, sender := range []string{"alice", "bob"} {
		code := mustGenerate(t, store, "telegram", sender, DefaultTTLSeconds)
		if _, _, err := store.ApproveCode(code); err != nil {
			t.Fatalf("approve %s: %v", sender, err)
		}
	}
	mustGenerate(t, store, "discord", "carol", DefaultTTLSeconds)

	if got, err := store.RevokeChannel("telegram"); err != nil || got != 2 {
		t.Errorf("RevokeChannel(telegram) = (%d, %v), want (2, nil)", got, err)
	}
	if got, err := store.RevokeChannel("telegram"); err != nil || got != 0 {
		t.Errorf("RevokeChannel again = (%d, %v), want (0, nil)", got, err)
	}
	if got := store.GetApproved("telegram"); len(got) != 0 {
		t.Errorf("telegram still approved: %v", got)
	}

	// discord holds two pending requests (carol from above, dave here) and no
	// approvals; telegram's approvals were already revoked.
	code := mustGenerate(t, store, "discord", "dave", DefaultTTLSeconds)
	cleared, err := store.ClearChannel("discord")
	if err != nil {
		t.Fatalf("ClearChannel: %v", err)
	}
	if cleared.Approved != 0 || cleared.Pending != 2 {
		t.Errorf("ClearChannel = %+v, want {Approved:0 Pending:2}", cleared)
	}
	if _, ok, _ := store.ApproveCode(code); ok {
		t.Error("ClearChannel left the pending request in place")
	}
	if cleared, err := store.ClearChannel("discord"); err != nil || cleared != (ClearResult{}) {
		t.Errorf("ClearChannel on an empty channel = (%+v, %v), want zero", cleared, err)
	}
}

func TestGetApprovedIsSortedAndDeduplicated(t *testing.T) {
	store := newTestStore(t)
	writeRaw(t, store, `{"approved": {"telegram": ["zoe", "alice", "bob", "alice"]}, "pending": {}}`)

	got := store.GetApproved("telegram")
	want := []string{"alice", "bob", "zoe"}
	if len(got) != len(want) {
		t.Fatalf("GetApproved = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetApproved = %v, want %v", got, want)
		}
	}
	if got := store.GetApproved("unknown"); len(got) != 0 {
		t.Errorf("GetApproved(unknown) = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Expiry
// ---------------------------------------------------------------------------

func TestExpiredRequestsAreNotApproved(t *testing.T) {
	// _gc_pending drops an entry only when expires_at < now, so the instant of
	// expiry itself is still valid. That boundary is easy to get wrong by one
	// comparison, so both sides of it are pinned.
	store := frozenStore(t, 1000)
	code := mustGenerate(t, store, "telegram", "alice", 10) // expires_at = 1010

	store.SetClock(func() float64 { return 1010 }) // exactly at the boundary
	if got := len(store.ListPending()); got != 1 {
		t.Errorf("pending exactly at the boundary = %d, want 1", got)
	}
	if _, ok, err := store.ApproveCode(code); err != nil || !ok {
		t.Errorf("ApproveCode at the expiry boundary = (_, %v, %v), want true/nil", ok, err)
	}
}

func TestRequestIsRejectedOncePastExpiry(t *testing.T) {
	store := frozenStore(t, 1000)
	code := mustGenerate(t, store, "telegram", "alice", 10)

	store.SetClock(func() float64 { return 1010.001 })
	if got := len(store.ListPending()); got != 0 {
		t.Errorf("expired request still listed: %d", got)
	}
	if _, ok, err := store.ApproveCode(code); err != nil || ok {
		t.Errorf("ApproveCode past expiry = (_, %v, %v), want false/nil", ok, err)
	}
	if store.IsApproved("telegram", "alice") {
		t.Error("an expired code approved the sender")
	}
}

func TestListPendingDropsMalformedEntriesWithoutSaving(t *testing.T) {
	store := frozenStore(t, 1000)
	raw := `{
	  "approved": {},
	  "pending": {
	    "GOODCODE": {"channel": "telegram", "sender_id": "alice", "expires_at": 9000000000.0},
	    "BADTYPE": "garbage",
	    "NOCHANNEL": {"sender_id": "x", "expires_at": 9000000000.0},
	    "EMPTYCHANNEL": {"channel": "", "sender_id": "x", "expires_at": 9000000000.0},
	    "NOSENDER": {"channel": "telegram", "expires_at": 9000000000.0},
	    "BOOLEXPIRY": {"channel": "telegram", "sender_id": "x", "expires_at": true},
	    "EXPIRED": {"channel": "telegram", "sender_id": "x", "expires_at": 1.0}
	  }
	}`
	writeRaw(t, store, raw)

	pending := store.ListPending()
	if len(pending) != 1 || pending[0].Code != "GOODCODE" {
		t.Fatalf("ListPending = %+v, want only GOODCODE", pending)
	}

	// The reference garbage-collects in memory only: the file must be
	// byte-identical afterwards, or a listing would silently rewrite the store.
	if got := readStore(t, store); got != raw {
		t.Errorf("ListPending rewrote the file:\n got %s\nwant %s", got, raw)
	}

	// A revoke does not garbage-collect either, but it does save — so the
	// malformed entries must survive it verbatim.
	if _, err := store.Revoke("telegram", "nobody"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	after := readStore(t, store)
	for _, code := range []string{"BADTYPE", "NOCHANNEL", "BOOLEXPIRY", "EXPIRED"} {
		if !strings.Contains(after, code) {
			t.Errorf("revoke dropped the malformed entry %s: %s", code, after)
		}
	}
}

func TestGenerateCodeGarbageCollectsExpiredEntries(t *testing.T) {
	store := frozenStore(t, 1000)
	writeRaw(t, store, `{
	  "approved": {},
	  "pending": {"EXPIRED": {"channel": "telegram", "sender_id": "x", "expires_at": 1.0}}
	}`)

	mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	if got := readStore(t, store); strings.Contains(got, "EXPIRED") {
		t.Errorf("generate did not garbage-collect the expired entry: %s", got)
	}
}

func TestFormatExpiry(t *testing.T) {
	store := frozenStore(t, 1000)

	for _, tc := range []struct {
		expiresAt float64
		want      string
	}{
		{1120, "120s"},
		{1001, "1s"},
		{1000, "expired"},
		{999, "expired"},
		{1000.5, "expired"}, // int() truncates toward zero
	} {
		if got := store.FormatExpiry(tc.expiresAt); got != tc.want {
			t.Errorf("FormatExpiry(%v) = %q, want %q", tc.expiresAt, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// File format and durability
// ---------------------------------------------------------------------------

func TestSaveWritesSortedApprovedAndVerbatimPending(t *testing.T) {
	store := frozenStore(t, 1755000000.1234567)
	code := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)
	if _, _, err := store.ApproveCode(code); err != nil {
		t.Fatalf("approve: %v", err)
	}
	for _, sender := range []string{"zoe", "bob"} {
		c := mustGenerate(t, store, "telegram", sender, DefaultTTLSeconds)
		if _, _, err := store.ApproveCode(c); err != nil {
			t.Fatalf("approve %s: %v", sender, err)
		}
	}
	carolCode := mustGenerate(t, store, "telegram", "carol", DefaultTTLSeconds)

	got := readStore(t, store)
	want := `{
  "approved": {
    "telegram": [
      "alice",
      "bob",
      "zoe"
    ]
  },
  "pending": {
    "` + "CODE" + `": {
      "channel": "telegram",
      "sender_id": "carol",
      "created_at": 1755000000.1234567,
      "expires_at": 1755000600.1234567
    }
  }
}`
	want = strings.Replace(want, "CODE", carolCode, 1)
	if got != want {
		t.Errorf("store file differs\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSaveDropsUnknownTopLevelKeys(t *testing.T) {
	store := newTestStore(t)
	raw := `{"version": 2, "approved": {"telegram": ["alice"]}, "pending": {}, "extra": {"x": 1}}`
	writeRaw(t, store, raw)

	// Revoking a sender who is NOT in the list returns early without saving, so
	// the file must still be byte-identical. Only a real change writes.
	if revoked, err := store.Revoke("telegram", "nobody"); err != nil || revoked {
		t.Fatalf("Revoke(absent) = (%v, %v), want false/nil", revoked, err)
	}
	if got := readStore(t, store); got != raw {
		t.Errorf("a no-op revoke rewrote the file:\n got %s\nwant %s", got, raw)
	}

	if revoked, err := store.Revoke("telegram", "alice"); err != nil || !revoked {
		t.Fatalf("Revoke(alice) = (%v, %v), want true/nil", revoked, err)
	}
	got := readStore(t, store)
	if strings.Contains(got, "version") || strings.Contains(got, "extra") {
		t.Errorf("unknown top-level keys survived a save: %s", got)
	}
}

func TestCorruptFileResetsInMemoryButLeavesFileUntouched(t *testing.T) {
	store := newTestStore(t)
	writeRaw(t, store, "not json")

	if store.IsApproved("telegram", "alice") {
		t.Error("a corrupt store approved a sender")
	}
	if got := len(store.ListPending()); got != 0 {
		t.Errorf("a corrupt store listed %d pending requests", got)
	}
	if got := readStore(t, store); got != "not json" {
		t.Errorf("reading a corrupt store rewrote it: %q", got)
	}
}

func TestNonDictTopLevelResetsToEmpty(t *testing.T) {
	store := newTestStore(t)
	writeRaw(t, store, `[1, 2, 3]`)

	if got := store.GetApproved("telegram"); len(got) != 0 {
		t.Errorf("GetApproved on a non-dict store = %v, want empty", got)
	}
}

func TestApprovedValuesAreCoercedWithPythonStr(t *testing.T) {
	store := newTestStore(t)
	writeRaw(t, store, `{"approved": {"telegram": [5, 5.0, null, true, [1, 2], "a"]}, "pending": {}}`)

	got := store.GetApproved("telegram")
	want := []string{"5", "5.0", "None", "True", "[1, 2]", "a"}
	if len(got) != len(want) {
		t.Fatalf("GetApproved = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetApproved = %v, want %v", got, want)
		}
	}
	// Each raw value was coerced with str(), so the numeric entries are
	// reachable under their rendered form and NOT under any other.
	if !store.IsApproved("telegram", "5") {
		t.Error(`the numeric entry 5 was not stored as the string "5"`)
	}
	if !store.IsApproved("telegram", "5.0") {
		t.Error(`the float entry 5.0 was not stored as the string "5.0"`)
	}
	if store.IsApproved("telegram", "05") {
		t.Error("an unrelated sender was approved")
	}
}

func TestNonListApprovedValueBecomesEmpty(t *testing.T) {
	store := newTestStore(t)
	writeRaw(t, store, `{"approved": {"telegram": 7}, "pending": {}}`)

	if got := store.GetApproved("telegram"); len(got) != 0 {
		t.Errorf("a scalar approved value produced %v, want empty", got)
	}
}

func TestPendingRequestMapOmitsAbsentKeys(t *testing.T) {
	store := frozenStore(t, 1000)
	writeRaw(t, store, `{
	  "approved": {},
	  "pending": {"FULLCODE": {"channel": "telegram", "sender_id": "alice", "expires_at": 9000000000.0, "note": "hi"},
	              "PARTCODE": {"channel": "telegram", "sender_id": "alice", "expires_at": 9000000000.0}}
	}`)

	pending := store.ListPending()
	if len(pending) != 2 {
		t.Fatalf("ListPending = %+v, want 2 entries", pending)
	}
	full := pending[0].Map()
	if _, ok := full["created_at"]; ok {
		t.Errorf("created_at was invented for an entry that has none: %v", full)
	}
	if full["note"] != "hi" {
		t.Errorf("extra key dropped: %v", full)
	}
	if full["code"] != "FULLCODE" {
		t.Errorf("code not merged into the entry: %v", full)
	}
}

func TestAtomicWriteLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "nested", "pairing.json"))
	store.SetLogger(discardLogger())
	mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	entries, err := os.ReadDir(filepath.Join(dir, "nested"))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "pairing.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only pairing.json", names)
	}
}

func TestSavePreservesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	if err := os.WriteFile(path, []byte(`{"approved": {}, "pending": {}}`), 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	store := NewStore(path)
	store.SetLogger(discardLogger())

	mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode after save = %o, want 640", got)
	}
}

// ---------------------------------------------------------------------------
// Failure modes
// ---------------------------------------------------------------------------

func TestUnreadableStoreIsAnIOErrorAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pairing.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := NewStore(path)
	store.SetLogger(discardLogger())

	if store.IsApproved("telegram", "alice") {
		t.Error("an unreadable store approved a sender")
	}
	if got := len(store.ListPending()); got != 0 {
		t.Errorf("an unreadable store listed %d requests", got)
	}

	_, err := store.GenerateCode("telegram", "alice", DefaultTTLSeconds)
	if err == nil {
		t.Fatal("GenerateCode against an unreadable store returned no error")
	}
	if !IsStoreIOError(err) {
		t.Errorf("GenerateCode error = %v, want a StoreIOError", err)
	}
	var ioErr *StoreIOError
	if !errors.As(err, &ioErr) {
		t.Fatalf("error %v does not unwrap to *StoreIOError", err)
	}
	if ioErr.Op != "read" {
		t.Errorf("StoreIOError.Op = %q, want read", ioErr.Op)
	}
	if !strings.Contains(err.Error(), "pairing") || !strings.Contains(err.Error(), path) {
		t.Errorf("error message %q does not name the store path", err.Error())
	}
	if strings.Count(err.Error(), path) != 1 {
		t.Errorf("error message %q repeats the path", err.Error())
	}

	// Read-only operations must not have created anything.
	if _, _, err := store.ApproveCode("ZZZZZZZZ"); err == nil {
		t.Error("ApproveCode against an unreadable store returned no error")
	}
	if _, err := store.DenyCode("ZZZZZZZZ"); err == nil {
		t.Error("DenyCode against an unreadable store returned no error")
	}
	if _, err := store.Revoke("telegram", "alice"); err == nil {
		t.Error("Revoke against an unreadable store returned no error")
	}
	if _, err := store.RevokeChannel("telegram"); err == nil {
		t.Error("RevokeChannel against an unreadable store returned no error")
	}
	if _, err := store.ClearChannel("telegram"); err == nil {
		t.Error("ClearChannel against an unreadable store returned no error")
	}
	// Reference behaviour, confirmed by running it against a directory in place
	// of the store file: "list" reports an empty list (list_pending swallows
	// the I/O error) while every command that would write reports the
	// unavailable message, and an unknown subcommand never touches the store.
	for _, tc := range []struct {
		text string
		want string
	}{
		{"list", "No pending pairing requests."},
		{"", "No pending pairing requests."},
		{"approve NOPECODE", "The pairing store is temporarily unavailable. Please try again."},
		{"deny NOPECODE", "The pairing store is temporarily unavailable. Please try again."},
		{"revoke alice", "The pairing store is temporarily unavailable. Please try again."},
		{"frobnicate", "Unknown pairing command.\nUsage: `/pairing [list|approve <code>|deny <code>|revoke <user_id>|revoke <channel> <user_id>]`"},
	} {
		if got := store.HandlePairingCommand("telegram", tc.text); got != tc.want {
			t.Errorf("HandlePairingCommand(%q) =\n%q\nwant\n%q", tc.text, got, tc.want)
		}
	}
}

func TestWriteFailureIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	// The parent of the store path is a FILE, so MkdirAll cannot succeed.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	store := NewStore(filepath.Join(blocker, "pairing.json"))
	store.SetLogger(discardLogger())

	_, err := store.GenerateCode("telegram", "alice", DefaultTTLSeconds)
	if err == nil {
		t.Fatal("GenerateCode into an unusable path returned no error")
	}
	if !IsStoreIOError(err) {
		t.Errorf("error = %v, want a StoreIOError", err)
	}
}

func TestMissingFileIsAnEmptyStore(t *testing.T) {
	store := newTestStore(t)

	if got := store.GetApproved("telegram"); len(got) != 0 {
		t.Errorf("GetApproved = %v, want empty", got)
	}
	if revoked, err := store.Revoke("telegram", "alice"); err != nil || revoked {
		t.Errorf("Revoke on a missing store = (%v, %v), want false/nil", revoked, err)
	}
	// A no-op does not save, so nothing has been created yet.
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a no-op created the store file: %v", err)
	}
	// A real mutation creates it.
	mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)
	if _, err := os.Stat(store.Path()); err != nil {
		t.Errorf("the store file was not created: %v", err)
	}
}

func TestListPendingDoesNotExposeInternalState(t *testing.T) {
	store := frozenStore(t, 1000)
	code := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	pending := store.ListPending()
	pending[0].Channel = "mutated"
	if pending[0].Extra == nil {
		pending[0].Extra = map[string]any{}
	}
	pending[0].Extra["injected"] = true
	pending[0].Map()["injected-via-map"] = true

	// Re-reading must reflect the file, not the caller's mutation.
	fresh := store.ListPending()
	if fresh[0].Code != code {
		t.Errorf("code = %q, want %q", fresh[0].Code, code)
	}
	if got := pyStr(fresh[0].Channel); got != "telegram" {
		t.Errorf("channel = %q, want telegram", got)
	}
	if _, ok := fresh[0].Map()["injected"]; ok {
		t.Error("a caller mutation leaked into a later read")
	}
	if _, ok := fresh[0].Map()["injected-via-map"]; ok {
		t.Error("a mutation through Map() leaked into a later read")
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestConcurrentAccessIsRaceFree exercises the store from many goroutines at
// once. Run with -race: without the store's mutex the load-modify-save
// sequences interleave and the file loses approvals.
func TestConcurrentAccessIsRaceFree(t *testing.T) {
	store := frozenStore(t, 1000)

	const workers = 16
	const perWorker = 8

	var wg sync.WaitGroup
	codes := make([]string, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				code, err := store.GenerateCode("telegram", senderName(w, i), DefaultTTLSeconds)
				if err != nil {
					t.Errorf("GenerateCode: %v", err)
					return
				}
				codes[w*perWorker+i] = code
			}
		}(w)
	}
	wg.Wait()

	// Every distinct sender must have its own live code.
	if got := len(store.ListPending()); got != workers*perWorker {
		t.Fatalf("pending = %d, want %d", got, workers*perWorker)
	}

	for i, code := range codes {
		if code == "" {
			t.Fatalf("code %d was never minted", i)
		}
	}

	// Approving concurrently must not lose approvals.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if _, ok, err := store.ApproveCode(codes[w*perWorker+i]); err != nil || !ok {
					t.Errorf("ApproveCode(%q) = (_, %v, %v)", codes[w*perWorker+i], ok, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	approved := store.GetApproved("telegram")
	if len(approved) != workers*perWorker {
		t.Fatalf("approved = %d, want %d", len(approved), workers*perWorker)
	}

	// The file on disk must parse and agree with the in-memory view.
	var doc map[string]any
	if err := json.Unmarshal([]byte(readStore(t, store)), &doc); err != nil {
		t.Fatalf("store file is not valid JSON after concurrent writes: %v", err)
	}
	onDisk, _ := doc["approved"].(map[string]any)["telegram"].([]any)
	if len(onDisk) != workers*perWorker {
		t.Errorf("file holds %d approvals, want %d", len(onDisk), workers*perWorker)
	}
}

func senderName(worker, i int) string {
	return "sender-" + string(rune('a'+worker%26)) + "-" + string(rune('0'+i%10))
}

// ---------------------------------------------------------------------------
// Package-level helpers
// ---------------------------------------------------------------------------

func TestDefaultPathMatchesDataDir(t *testing.T) {
	want := filepath.Join(config.DefaultDataDir(), "pairing.json")
	if got := DefaultPath(); got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultReturnsOneStore(t *testing.T) {
	first, second := Default(), Default()
	if first != second {
		t.Error("Default() returned two different stores")
	}
	if Default().Path() != DefaultPath() {
		t.Errorf("Default().Path() = %q, want %q", Default().Path(), DefaultPath())
	}
}

func TestHandlePairingCommandSubcommands(t *testing.T) {
	store := frozenStore(t, 1000)
	code := mustGenerate(t, store, "telegram", "alice", DefaultTTLSeconds)

	for _, tc := range []struct {
		text string
		want string
	}{
		{"", "Pending pairing requests:\n- `" + code + "` | telegram | alice | 600s"},
		{"list", "Pending pairing requests:\n- `" + code + "` | telegram | alice | 600s"},
		{"  list  ", "Pending pairing requests:\n- `" + code + "` | telegram | alice | 600s"},
		{"\x1clist\x1c", "Pending pairing requests:\n- `" + code + "` | telegram | alice | 600s"},
		{"approve", "Usage: `/pairing approve <code>`"},
		{"deny", "Usage: `/pairing deny <code>`"},
		{"revoke", "Usage: `/pairing revoke <user_id>` or `/pairing revoke <channel> <user_id>`"},
		{"frobnicate", "Unknown pairing command.\nUsage: `/pairing [list|approve <code>|deny <code>|revoke <user_id>|revoke <channel> <user_id>]`"},
	} {
		if got := store.HandlePairingCommand("telegram", tc.text); got != tc.want {
			t.Errorf("HandlePairingCommand(%q) =\n%q\nwant\n%q", tc.text, got, tc.want)
		}
	}

	if got := store.HandlePairingCommand("telegram", "approve "+code); got != "Approved pairing code `"+code+"` — alice can now access telegram" {
		t.Errorf("approve = %q", got)
	}
	if got := store.HandlePairingCommand("telegram", "approve NOPECODE"); got != "Invalid or expired pairing code: `NOPECODE`" {
		t.Errorf("approve unknown = %q", got)
	}
	if got := store.HandlePairingCommand("telegram", "revoke alice"); got != "Revoked alice from telegram" {
		t.Errorf("revoke one = %q", got)
	}
	if got := store.HandlePairingCommand("telegram", "revoke bob"); got != "bob was not in the approved list for telegram" {
		t.Errorf("revoke missing = %q", got)
	}
	if got := store.HandlePairingCommand("telegram", "revoke discord bob"); got != "bob was not in the approved list for discord" {
		t.Errorf("revoke two = %q", got)
	}
	if got := store.HandlePairingCommand("telegram", "list"); got != "No pending pairing requests." {
		t.Errorf("empty list = %q", got)
	}
}

func TestPySplitUsesPythonWhitespace(t *testing.T) {
	// U+001C..U+001F are whitespace for Python's str.split() and are NOT
	// whitespace for Go's unicode.IsSpace, so a naive port would keep
	// "revoke\x1calice" as a single token.
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"  a  b  ", []string{"a", "b"}},
		{"a\tb\nc\rd", []string{"a", "b", "c", "d"}},
		{"a\x1cb", []string{"a", "b"}},
		{"a\x1db\x1ec\x1fd", []string{"a", "b", "c", "d"}},
		// NBSP and the ideographic space ARE Python whitespace
		// ('\xa0'.isspace() is True); the zero-width space is not.
		{"\u00a0a", []string{"a"}},
		{"a\u00a0b", []string{"a", "b"}},
		{"\u3000a", []string{"a"}},
		{"\u200ba", []string{"\u200ba"}},
		{"a\u200bb", []string{"a\u200bb"}},
	} {
		got := pySplit(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("pySplit(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("pySplit(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeRaw(t *testing.T, store *Store, content string) {
	t.Helper()
	if err := os.WriteFile(store.Path(), []byte(content), 0o600); err != nil {
		t.Fatalf("write store fixture: %v", err)
	}
}
