package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const (
	maxFrameBytes = 4 << 20
	cmdSubscribe = "aibot_subscribe"
	cmdHeartbeat = "ping"
	cmdRespond = "aibot_respond_msg"
	cmdRespondWelcome = "aibot_respond_welcome_msg"
	cmdSend = "aibot_send_msg"
	cmdCallback = "aibot_msg_callback"
	cmdEventCallback = "aibot_event_callback"
)

var errServerDisconnected = errors.New("wecom: server disconnected this connection")

type wsHeaders struct { ReqID string `json:"req_id"` }
type wsFrame struct {
	Cmd string `json:"cmd,omitempty"`
	Headers wsHeaders `json:"headers"`
	Body json.RawMessage `json:"body,omitempty"`
	ErrCode int `json:"errcode,omitempty"`
	ErrMsg string `json:"errmsg,omitempty"`
}
type inboundMessage struct {
	MsgID string `json:"msgid"`
	AIBotID string `json:"aibotid"`
	ChatID string `json:"chatid,omitempty"`
	ChatType string `json:"chattype"`
	From struct { UserID string `json:"userid"` } `json:"from"`
	CreateTime int64 `json:"create_time,omitempty"`
	MsgType string `json:"msgtype"`
	Text struct { Content string `json:"content"` } `json:"text"`
	Voice struct { Content string `json:"content"` } `json:"voice"`
	Mixed struct {
		Items []struct {
			MsgType string `json:"msgtype"`
			Text *struct { Content string `json:"content"` } `json:"text,omitempty"`
		} `json:"msg_item"`
	} `json:"mixed"`
}

type Channel struct {
	*channels.Base
	cfg Config

	mu sync.RWMutex
	writeMu sync.Mutex
	conn *websocket.Conn
	chatReq map[string]string
	pending map[string]chan wsFrame
	seen map[string]struct{}
	seenFIFO []string
	lastRx time.Time
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil { return nil, err }
	c := &Channel{
		cfg: cfg,
		chatReq: make(map[string]string),
		pending: make(map[string]chan wsFrame),
		seen: make(map[string]struct{}),
	}
	c.Base = channels.NewBase(c, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("WeCom"))
	return c, nil
}

func (c *Channel) ProgressTransportDefaults() (bool, bool, bool) { return false, false, true }

func (c *Channel) Start(ctx context.Context) error {
	if err := c.cfg.validateRuntime(); err != nil { return err }
	c.SetRunning(true)
	defer c.SetRunning(false)
	backoff := time.Second
	for ctx.Err() == nil && c.IsRunning() {
		authenticated, err := c.connectAndRun(ctx)
		if ctx.Err() != nil || !c.IsRunning() { return nil }
		if errors.Is(err, errServerDisconnected) {
			// WeCom uses this event when another connection supersedes the old
			// one. Reconnect once through the same lifecycle instead of spinning.
			backoff = 3 * time.Second
		} else if authenticated {
			backoff = time.Second
		}
		if err != nil { c.Logger().Warn("WeCom WebSocket disconnected", "error", err) }
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second { backoff = 30*time.Second }
		}
	}
	return nil
}

func (c *Channel) Stop(context.Context) error {
	c.SetRunning(false)
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil { return conn.Close(websocket.StatusNormalClosure, "channel stopping") }
	return nil
}

func (c *Channel) Send(ctx context.Context, msg core.OutboundMessage) error {
	content := strings.TrimSpace(msg.Content)
	if content == "" { return nil }
	c.mu.RLock()
	reqID := c.chatReq[msg.ChatID]
	c.mu.RUnlock()

	if reqID != "" {
		streamID := generateReqID("stream")
		body := map[string]any{
			"msgtype": "stream",
			"stream": map[string]any{"id": streamID, "finish": true, "content": content},
		}
		if _, err := c.request(ctx, wsFrame{
			Cmd: cmdRespond, Headers: wsHeaders{ReqID: reqID}, Body: mustJSON(body),
		}); err == nil {
			return nil
		} else {
			c.Logger().Warn("WeCom passive reply failed; trying proactive send", "chat_id", msg.ChatID, "error", err)
		}
	}
	if strings.TrimSpace(msg.ChatID) == "" { return errors.New("wecom: chat id is empty") }
	body := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{"content": content},
		"chatid": msg.ChatID,
	}
	_, err := c.request(ctx, wsFrame{
		Cmd: cmdSend, Headers: wsHeaders{ReqID: generateReqID(cmdSend)}, Body: mustJSON(body),
	})
	return err
}

func (c *Channel) connectAndRun(ctx context.Context) (bool, error) {
	conn, resp, err := websocket.Dial(ctx, c.cfg.WebSocketURL, nil)
	if resp != nil && resp.Body != nil { _ = resp.Body.Close() }
	if err != nil { return false, fmt.Errorf("wecom: websocket dial: %w", err) }
	conn.SetReadLimit(maxFrameBytes)
	defer conn.CloseNow()

	authReqID := generateReqID(cmdSubscribe)
	auth := wsFrame{
		Cmd: cmdSubscribe,
		Headers: wsHeaders{ReqID: authReqID},
		Body: mustJSON(map[string]string{"bot_id": c.cfg.BotID, "secret": c.cfg.Secret}),
	}
	if err := c.writeFrame(ctx, conn, auth); err != nil { return false, err }

	authCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	authenticated := false
	for !authenticated {
		_, raw, err := conn.Read(authCtx)
		if err != nil { return false, fmt.Errorf("wecom: auth read: %w", err) }
		var frame wsFrame
		if err := json.Unmarshal(raw, &frame); err != nil { continue }
		if frame.Headers.ReqID != authReqID { continue }
		if frame.ErrCode != 0 {
			return false, fmt.Errorf("wecom: authentication failed (%d): %s", frame.ErrCode, frame.ErrMsg)
		}
		authenticated = true
	}

	c.mu.Lock()
	if c.conn != nil && c.conn != conn { _ = c.conn.CloseNow() }
	c.conn = conn
	c.lastRx = time.Now()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == conn { c.conn = nil }
		for id, ch := range c.pending {
			select { case ch <- wsFrame{ErrCode: -1, ErrMsg: "connection closed"}: default: }
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()

	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go c.heartbeatLoop(hbCtx, conn)

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil { return true, err }
		c.mu.Lock()
		c.lastRx = time.Now()
		c.mu.Unlock()

		var frame wsFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			c.Logger().Warn("WeCom frame decode failed", "error", err)
			continue
		}
		if frame.Cmd == "" {
			c.resolvePending(frame)
			continue
		}
		switch frame.Cmd {
		case cmdCallback:
			if err := c.handleMessage(ctx, frame); err != nil {
				c.Logger().Warn("WeCom inbound dispatch failed", "error", err)
			}
		case cmdEventCallback:
			var event struct {
				MsgType string `json:"msgtype"`
				Event struct { Type string `json:"eventtype"` } `json:"event"`
				ChatID string `json:"chatid"`
				From struct { UserID string `json:"userid"` } `json:"from"`
			}
			if json.Unmarshal(frame.Body, &event) == nil {
				if event.Event.Type == "disconnected_event" { return true, errServerDisconnected }
				if event.Event.Type == "enter_chat" && c.cfg.WelcomeMessage != "" && event.ChatID != "" && c.IsAllowed(event.From.UserID) {
					_, _ = c.request(ctx, wsFrame{
						Cmd: cmdRespondWelcome,
						Headers: wsHeaders{ReqID: frame.Headers.ReqID},
						Body: mustJSON(map[string]any{"msgtype":"text","text":map[string]string{"content":c.cfg.WelcomeMessage}}),
					})
				}
			}
		}
	}
}

func (c *Channel) heartbeatLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(c.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.RLock()
			last := c.lastRx
			current := c.conn == conn
			c.mu.RUnlock()
			if !current { return }
			if !last.IsZero() && time.Since(last) > 3*c.cfg.HeartbeatInterval {
				_ = conn.Close(websocket.StatusGoingAway, "heartbeat timeout")
				return
			}
			hb := wsFrame{Cmd: cmdHeartbeat, Headers: wsHeaders{ReqID: generateReqID(cmdHeartbeat)}}
			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.writeFrame(writeCtx, conn, hb)
			cancel()
			if err != nil {
				_ = conn.CloseNow()
				return
			}
		}
	}
}

func (c *Channel) handleMessage(ctx context.Context, frame wsFrame) error {
	var msg inboundMessage
	if err := json.Unmarshal(frame.Body, &msg); err != nil { return err }
	sender := strings.TrimSpace(msg.From.UserID)
	if sender == "" || !c.IsAllowed(sender) { return nil }
	if msg.MsgID != "" && c.remember(msg.MsgID) { return nil }

	chatID := strings.TrimSpace(msg.ChatID)
	if chatID == "" { chatID = sender }
	content := ""
	switch msg.MsgType {
	case "text":
		content = strings.TrimSpace(msg.Text.Content)
	case "voice":
		if t := strings.TrimSpace(msg.Voice.Content); t != "" { content = "[voice] " + t } else { content = "[voice]" }
	case "mixed":
		var parts []string
		for _, item := range msg.Mixed.Items {
			if item.MsgType == "text" && item.Text != nil && strings.TrimSpace(item.Text.Content) != "" {
				parts = append(parts, strings.TrimSpace(item.Text.Content))
			}
		}
		content = strings.Join(parts, "\n")
	case "image":
		content = "[image]"
	case "file":
		content = "[file]"
	case "video":
		content = "[video]"
	default:
		return nil
	}
	if content == "" { return nil }

	c.mu.Lock()
	c.chatReq[chatID] = frame.Headers.ReqID
	c.mu.Unlock()
	return c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: sender,
		ChatID: chatID,
		Content: content,
		Metadata: map[string]any{"message_id": msg.MsgID, "chat_type": msg.ChatType, "aibot_id": msg.AIBotID},
		IsDM: msg.ChatType == "" || msg.ChatType == "single",
	})
}

func (c *Channel) request(ctx context.Context, frame wsFrame) (wsFrame, error) {
	reqID := frame.Headers.ReqID
	if reqID == "" { return wsFrame{}, errors.New("wecom: request id is empty") }
	ack := make(chan wsFrame, 1)
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return wsFrame{}, errors.New("wecom: WebSocket is not connected")
	}
	if _, exists := c.pending[reqID]; exists {
		c.mu.Unlock()
		return wsFrame{}, fmt.Errorf("wecom: request %s is already pending", reqID)
	}
	c.pending[reqID] = ack
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.pending[reqID] == ack { delete(c.pending, reqID) }
		c.mu.Unlock()
	}()

	if err := c.writeFrame(ctx, conn, frame); err != nil { return wsFrame{}, err }
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case response := <-ack:
		if response.ErrCode != 0 {
			return response, fmt.Errorf("wecom: ack error (%d): %s", response.ErrCode, response.ErrMsg)
		}
		return response, nil
	case <-timer.C:
		return wsFrame{}, fmt.Errorf("wecom: ack timeout for %s", reqID)
	case <-ctx.Done():
		return wsFrame{}, ctx.Err()
	}
}

func (c *Channel) resolvePending(frame wsFrame) {
	reqID := frame.Headers.ReqID
	if reqID == "" || strings.HasPrefix(reqID, cmdHeartbeat) || strings.HasPrefix(reqID, cmdSubscribe) { return }
	c.mu.RLock()
	ch := c.pending[reqID]
	c.mu.RUnlock()
	if ch != nil {
		select { case ch <- frame: default: }
	}
}

func (c *Channel) writeFrame(ctx context.Context, conn *websocket.Conn, frame wsFrame) error {
	raw, err := json.Marshal(frame)
	if err != nil { return err }
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.Write(ctx, websocket.MessageText, raw)
}

func (c *Channel) remember(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[id]; ok { return true }
	c.seen[id] = struct{}{}
	c.seenFIFO = append(c.seenFIFO, id)
	if len(c.seenFIFO) > 1000 {
		delete(c.seen, c.seenFIFO[0])
		c.seenFIFO = c.seenFIFO[1:]
	}
	return false
}

func mustJSON(v any) json.RawMessage { raw, _ := json.Marshal(v); return raw }

func generateReqID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }
	return prefix + "-" + hex.EncodeToString(b[:])
}
