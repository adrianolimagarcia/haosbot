package websocket

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const outboundQueueSize = 64

// Channel accepts authenticated WebSocket clients and routes messages through
// the shared channel manager and message bus.
type Channel struct {
	*channels.Base
	cfg Config

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	conns    map[string]*clientConn
	reservations int
	stopping bool
}

type clientConn struct {
	chatID string
	clientID string
	conn   *websocket.Conn
	send   chan []byte
	done   chan struct{}
	closed sync.Once
}

// New builds a WebSocket channel from channels.websocket configuration.
func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil { return nil, err }
	c := &Channel{cfg: cfg, conns: make(map[string]*clientConn)}
	c.Base = channels.NewBase(c, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("WebSocket"))
	return c, nil
}

func (c *Channel) Start(ctx context.Context) error {
	if strings.TrimSpace(c.cfg.Token) == "" { return errors.New("websocket: token is required") }
	listener, err := net.Listen("tcp", net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.Port)))
	if err != nil { return fmt.Errorf("websocket: listen: %w", err) }
	c.mu.Lock()
	c.listener = listener
	c.server = &http.Server{Handler: http.HandlerFunc(c.serveHTTP), ReadHeaderTimeout: 5 * time.Second}
	c.stopping = false
	server := c.server
	c.mu.Unlock()
	c.SetRunning(true)
	defer c.SetRunning(false)

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.Stop(shutdownCtx)
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) { return nil }
		return fmt.Errorf("websocket: serve: %w", err)
	}
}

func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
	for {
		c.mu.Lock()
		reservations := c.reservations
		c.mu.Unlock()
		if reservations == 0 { break }
		select {
		case <-ctx.Done(): return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	c.mu.Lock()
	server := c.server
	conns := make([]*clientConn, 0, len(c.conns))
	for _, conn := range c.conns { conns = append(conns, conn) }
	c.mu.Unlock()
	for _, conn := range conns {
		closeCtx, cancel := context.WithTimeout(ctx, time.Second)
		_ = conn.conn.Close(closeCtx, websocket.StatusGoingAway, "server stopping")
		cancel()
		conn.finish()
	}
	if server == nil { return nil }
	if err := server.Shutdown(ctx); err != nil { return err }
	return nil
}

func (c *Channel) Send(ctx context.Context, msg core.OutboundMessage) error {
	payload := map[string]any{"event": "message", "chat_id": msg.ChatID, "text": msg.Content}
	if len(msg.Media) != 0 { payload["media"] = msg.Media }
	if msg.ReplyTo != nil { payload["reply_to"] = *msg.ReplyTo }
	return c.enqueue(ctx, msg.ChatID, payload)
}

// SendDelta exposes the shared manager's streaming capability to WebSocket
// clients. Each frame is independently valid JSON so clients can recover after
// reconnecting without parsing concatenated text fragments.
func (c *Channel) SendDelta(ctx context.Context, chatID, delta string, metadata map[string]any, opts channels.DeltaOptions) error {
	payload := map[string]any{"event": "delta", "chat_id": chatID, "text": delta, "stream_end": opts.StreamEnd}
	if opts.StreamID != nil { payload["stream_id"] = *opts.StreamID }
	return c.enqueue(ctx, chatID, payload)
}

func (c *Channel) enqueue(ctx context.Context, chatID string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil { return err }
	c.mu.Lock()
	client := c.conns[chatID]
	c.mu.Unlock()
	if client == nil { return fmt.Errorf("websocket: client %q is not connected", chatID) }
	select {
	case client.send <- data: return nil
	case <-client.done: return errors.New("websocket: client disconnected")
	case <-ctx.Done(): return ctx.Err()
	default: return errors.New("websocket: client outbound queue is full")
	}
}

func (c *Channel) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != c.cfg.Path { http.NotFound(w, r); return }
	if r.Method != http.MethodGet { w.Header().Set("Allow", http.MethodGet); http.Error(w, "method not allowed", http.StatusMethodNotAllowed); return }
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(c.cfg.Token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" { clientID = "anon-" + randomHex(12) }
	if len(clientID) > 128 || strings.ContainsAny(clientID, "\r\n\x00") {
		http.Error(w, "invalid client_id", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		http.Error(w, "server stopping", http.StatusServiceUnavailable)
		return
	}
	if len(c.conns)+c.reservations >= c.cfg.MaxConnections {
		c.mu.Unlock()
		http.Error(w, "too many clients", http.StatusTooManyRequests)
		return
	}
	c.reservations++
	c.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			c.mu.Lock()
			c.reservations--
			c.mu.Unlock()
		}
	}()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil { return }
	conn.SetReadLimit(c.cfg.MaxMessageBytes)
	client := &clientConn{chatID: randomUUID(), clientID: clientID, conn: conn, send: make(chan []byte, outboundQueueSize), done: make(chan struct{})}
	ready, _ := json.Marshal(map[string]any{"event": "ready", "chat_id": client.chatID, "client_id": client.clientID})
	writeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err = conn.Write(writeCtx, websocket.MessageText, ready)
	cancel()
	if err != nil { _ = conn.CloseNow(); return }
	c.mu.Lock()
	c.conns[client.chatID] = client
	c.reservations--
	reserved = false
	c.mu.Unlock()
	go c.writeLoop(client)
	defer func() {
		client.finish()
		c.mu.Lock()
		if c.conns[client.chatID] == client { delete(c.conns, client.chatID) }
		c.mu.Unlock()
		_ = conn.CloseNow()
	}()

	for {
		_, data, err := conn.Read(context.Background())
		if err != nil { return }
		content, err := inboundText(data)
		if err != nil { _ = conn.Close(websocket.StatusInvalidFramePayloadData, err.Error()); return }
		if content == "" { continue }
		err = c.HandleMessage(context.Background(), channels.InboundRequest{
			SenderID: client.clientID, ChatID: client.chatID, Content: content,
			Metadata: map[string]any{"client_id": client.clientID}, IsDM: false,
		})
		if err != nil { c.Logger().Warn("WebSocket inbound dispatch failed", "client_id", client.clientID, "error", err) }
	}
}

func inboundText(data []byte) (string, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" { return "", nil }
	if !json.Valid(data) || !strings.HasPrefix(trimmed, "{") { return trimmed, nil }
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil { return "", err }
	for _, key := range []string{"content", "text", "message"} {
		if text, ok := value[key].(string); ok { return strings.TrimSpace(text), nil }
	}
	return "", nil
}

func (c *Channel) writeLoop(client *clientConn) {
	for {
		select {
		case <-client.done: return
		case data := <-client.send:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := client.conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil { _ = client.conn.CloseNow(); client.finish(); return }
		}
	}
}

func (client *clientConn) finish() { client.closed.Do(func() { close(client.done) }) }

func randomHex(n int) string {
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil { return strconv.FormatInt(time.Now().UnixNano(), 16) }
	return hex.EncodeToString(data)
}

func randomUUID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil { return randomHex(16) }
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(data)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
