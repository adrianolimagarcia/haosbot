package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/textutil"
)

// ---------------------------------------------------------------------------
// Telegram webhook transport
//
// The reference (upstream/nanobot runtime.py:733-746) does not implement the
// receiver itself: it hands the job to python-telegram-bot's
// `Updater.start_webhook`, which runs a Tornado server. This port has no such
// dependency, so the receiver below is a net/http implementation of the same
// observable contract.
//
// The contract was read off PTB 22.8 (the version the reference's manifest
// pins) rather than guessed. Two probes were run against the installed library
// and their raw output is recorded in the commit message:
//
//   - `WebhookAppClass` registers the route with the regex `webhook_path/?$`,
//     answers only POST, and returns 403 for a missing or wrong secret token.
//   - `Updater._bootstrap` sends `set_webhook(url=webhook_url, ...)` with the
//     configured URL VERBATIM — url_path is not appended — and sends NO
//     `delete_webhook` in webhook mode (only the polling branch deletes).
//
// Where this port deliberately differs from the reference, the divergence is
// marked "DIVERGENCE" and the reason is given. All three divergences make the
// receiver stricter or more explicit than PTB; none of them weakens
// authentication.
// ---------------------------------------------------------------------------

// webhookSecretHeader is the header Telegram echoes the configured secret token
// in. PTB 22.8 reads the same header (webhookhandler.py:197).
const webhookSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

// webhookMaxBodyBytes bounds an inbound Update body.
//
// A Telegram Update is small: 4096 characters of text, 1024 of caption, 64
// bytes of callback data. 1 MiB is two orders of magnitude above any real
// update and still stops a hostile client from making the receiver allocate
// without limit. PTB/tornado's default buffer (100 MB) is far more permissive;
// the tighter bound is the simplest correct choice for a receiver that only
// ever reads one small JSON object.
const webhookMaxBodyBytes = 1 << 20

// webhookDeregisterTimeout bounds the deleteWebhook call made when the receiver
// stops, so a hung Bot API cannot stall channel shutdown.
const webhookDeregisterTimeout = 10 * time.Second

// errWebhookStartup marks a webhook startup failure that cannot heal by
// retrying: a port that cannot be bound, or Telegram rejecting setWebhook.
//
// The reference treats anything that is not a network timeout as terminal —
// `_is_transient_startup_error` (runtime.py:771-778) returns False for OSError,
// and the supervisor reacts with "Never heals on its own: fail instead of
// retrying forever" (runtime.py:628-633). Mirroring that is what turns a bind
// failure into a clear startup error instead of an endless retry loop, and it
// is why webhook mode can never silently degrade into long polling.
var errWebhookStartup = errors.New("telegram: webhook startup failed")

// webhookRoute is the local HTTP route the receiver serves.
//
// The reference passes `webhook_path.lstrip("/")` as PTB's `url_path`
// (runtime.py:738), and PTB's `_start_webhook` prepends "/" when it is missing
// (`if not url_path.startswith("/"): url_path = f"/{url_path}"`). Net effect:
// route == "/" + webhook_path.lstrip("/"). The config validator already
// guarantees a leading slash, so this is normally the identity; the lstrip is
// kept because a doubled leading slash would otherwise produce "//telegram".
func webhookRoute(path string) string {
	return "/" + strings.TrimLeft(path, "/")
}

// webhookReceiver serves one channel's Telegram webhook.
type webhookReceiver struct {
	ch     *Channel
	route  string
	secret string
}

// ServeHTTP implements the receiver's contract.
//
// Order matters and mirrors PTB: route, then method, then authentication, then
// body. Authentication runs before any parsing, so an unauthenticated request
// never reaches the JSON decoder or the dispatcher.
func (r *webhookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// DIVERGENCE (stricter): PTB serves `webhook_path/?$`, so the reference also
	// answers "/telegram/" with a trailing slash. This port serves exactly
	// webhook_path, per its specification: everything else is 404.
	if req.URL.Path != r.route {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// PTB declares SUPPORTED_METHODS = ("POST",), so a GET on the route is 405.
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !r.authenticated(req) {
		// DIVERGENCE (same outcome, different code): PTB raises
		// HTTPStatus.FORBIDDEN for a missing or wrong secret. This port answers
		// 401 Unauthorized, which is what the requirement specifies and what an
		// authentication failure means; 403 would claim the caller is
		// authenticated but not permitted.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Bound the body before reading a byte of it. MaxBytesReader also makes the
	// server close the connection instead of draining an oversized request.
	body := http.MaxBytesReader(w, req.Body, webhookMaxBodyBytes)

	update, err := decodeWebhookUpdate(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// DIVERGENCE (better): PTB lets json.JSONDecodeError escape, so the
		// reference answers a malformed body with 500. A 400 is the correct
		// rejection and keeps the receiver's own logs clean of self-inflicted
		// server errors. Either way the update is not dispatched.
		http.Error(w, "invalid update payload", http.StatusBadRequest)
		return
	}

	// Dispatch BEFORE acknowledging, through the very same function the polling
	// loop calls, so both transports share one downstream path and cannot
	// drift. dispatchUpdate only stages the update (it is non-blocking for
	// messages and commands), so this does not delay the acknowledgement.
	r.ch.dispatchUpdate(*update)

	// PTB sets this exact Content-Type before answering 200 with an empty body.
	w.Header().Set("Content-Type", `application/json; charset="utf-8"`)
	w.WriteHeader(http.StatusOK)
}

// authenticated reports whether the request carries the configured secret.
//
// The comparison is constant time. subtle.ConstantTimeCompare returns 0 for
// inputs of different length, so a wrong-length guess cannot be distinguished
// from a wrong-content one by timing.
func (r *webhookReceiver) authenticated(req *http.Request) bool {
	// Fail closed. Webhook mode cannot reach this with an empty secret (the
	// config validator requires one), but an empty expectation must never be
	// satisfiable by an empty header.
	if r.secret == "" {
		return false
	}
	presented := req.Header.Get(webhookSecretHeader)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(r.secret)) == 1
}

// decodeWebhookUpdate parses exactly one Update from body.
//
// A body is rejected when it is not valid JSON, when it carries trailing data
// after the object, or when it has no update_id. Telegram always sends a
// positive update_id, and PTB's own parser requires the field — a body without
// one is not an Update and must not be acknowledged as though it had been
// handled.
func decodeWebhookUpdate(body io.Reader) (*Update, error) {
	dec := json.NewDecoder(body)

	var update Update
	if err := dec.Decode(&update); err != nil {
		return nil, err
	}
	// `json.loads` (what PTB uses) rejects trailing data; Decode does not.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("telegram: trailing data after update")
	}
	if update.UpdateID == 0 {
		return nil, errors.New("telegram: update_id is missing")
	}
	return &update, nil
}

// startWebhook binds the configured listener, registers the webhook with
// Telegram, and serves until ctx is cancelled or Stop runs. It blocks, exactly
// as pollLoop does, so Start's lifecycle loop is unchanged between modes.
func (c *Channel) startWebhook(ctx context.Context) error {
	addr := net.JoinHostPort(c.cfg.WebhookListenHost, strconv.Itoa(c.cfg.WebhookListenPort))
	route := webhookRoute(c.cfg.WebhookPath)

	// Bind first. PTB binds after setWebhook, which would leave a webhook
	// registered at Telegram for a listener that never came up; binding first
	// makes a port conflict a self-contained failure with nothing to undo.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%w: cannot listen on %s: %v", errWebhookStartup, addr, err)
	}

	srv := &http.Server{
		Handler: &webhookReceiver{ch: c, route: route, secret: c.cfg.WebhookSecretToken},
		// A webhook peer is a remote server, not a trusted caller: bound the
		// header read so a slowloris client cannot hold connections open.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// net/http's own error logger would otherwise write to the process
		// default logger. Route it into the channel's logger at debug level.
		// net/http never logs request headers, so the secret token cannot reach
		// the log through this path — and the receiver logs no request itself.
		ErrorLog: slog.NewLogLogger(c.Logger().Handler(), slog.LevelDebug),
	}

	c.webhookMu.Lock()
	c.webhookServer = srv
	c.webhookMu.Unlock()

	// Shut the server down when the lifecycle context ends, so a manager
	// cancellation unblocks Serve the way pollLoop's ctx check does. Shutdown
	// (not Close) keeps the stop graceful; it is idempotent, so it races safely
	// with Stop's own shutdown.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhookDeregisterTimeout)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		case <-done:
		}
	}()

	// Register the public URL verbatim: the reference passes
	// `webhook_url.strip()` and PTB forwards it unchanged as set_webhook's
	// `url`. webhook_path is the LOCAL route only — a reverse proxy maps the
	// public URL onto it.
	if err := c.client.SetWebhook(ctx, SetWebhookParams{
		URL:            textutil.PyStrip(c.cfg.WebhookURL),
		MaxConnections: c.cfg.WebhookMaxConnections,
		AllowedUpdates: c.allowedUpdates(),
		SecretToken:    c.cfg.WebhookSecretToken,
	}); err != nil {
		c.webhookMu.Lock()
		c.webhookServer = nil
		c.webhookMu.Unlock()
		_ = srv.Close()
		return fmt.Errorf("%w: setWebhook: %v", errWebhookStartup, err)
	}

	c.Logger().Info("webhook receiver listening",
		"address", addr, "path", route, "url", textutil.PyStrip(c.cfg.WebhookURL))

	serveErr := srv.Serve(ln)

	// Whatever ended the server — Stop, a cancelled lifecycle context, or a
	// serve error — deregister, so Telegram stops POSTing at a listener that is
	// no longer there.
	//
	// DIVERGENCE: the reference does NOT do this. PTB's Updater.stop() only
	// stops its HTTP server (updater.py `_stop_httpd`), and the reference's
	// teardown never calls delete_webhook, so the frozen Python leaves the
	// webhook registered against a dead listener. Deregistering is the correct
	// cleanup, and the requirement asks for it.
	//
	// The lifecycle context is already cancelled by the time Serve returns, so
	// the deregistration runs on a context that keeps the caller's values but
	// not its cancellation — otherwise a shutdown would silently skip the call,
	// which is the same class of silent omission this transport exists to fix.
	deregCtx, cancelDereg := context.WithTimeout(context.WithoutCancel(ctx), webhookDeregisterTimeout)
	defer cancelDereg()
	if deregErr := c.client.DeleteWebhook(deregCtx, DeleteWebhookParams{DropPendingUpdates: false}); deregErr != nil {
		c.Logger().Warn("deleteWebhook on webhook shutdown failed", "error", deregErr)
	}

	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// stopWebhook stops the receiver if it is running. It is idempotent: after the
// first call the server is cleared, so a second call is a no-op.
func (c *Channel) stopWebhook(ctx context.Context) error {
	c.webhookMu.Lock()
	srv := c.webhookServer
	c.webhookServer = nil
	c.webhookMu.Unlock()

	if srv == nil {
		return nil
	}

	err := srv.Shutdown(ctx)
	if err != nil {
		// A cancelled or expired context must not leave the port bound: fall
		// back to an immediate close so the next start can rebind.
		_ = srv.Close()
	}
	return err
}
