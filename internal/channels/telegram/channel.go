package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
	"github.com/adrianolimagarcia/nanobot-go/internal/events"
)

// Channel implements the Telegram bot channel in pure Go using BotClient.
type Channel struct {
	*channels.Base
	cfg          Config
	senderPolicy SenderPolicy
	client       *BotClient

	botUserID   int64
	botUsername string

	// thread context cache: (chatID:messageID) -> messageThreadID
	threadsMu      sync.Mutex
	messageThreads map[string]int64 // key: chatID + ":" + messageID

	// in-flight compaction notices: (chatID:compactionID) -> messageID
	compactionMu      sync.Mutex
	compactionNotices map[string]int64 // key: chatID + ":" + compactionID

	// per-session ordered inbound staging
	inboundMu      sync.Mutex
	inboundBuffers map[string][]*queuedUpdate
	inboundWorkers map[string]context.CancelFunc

	// stop coordination
	stopCtx    context.Context
	cancelStop context.CancelFunc
}

// queuedUpdate represents a staged inbound item before drain.
type queuedUpdate struct {
	kind      string // "command" or "message"
	update    Update
	messageID int64
	updateID  int64
}

// New creates a new Telegram channel instance.
func New(section channels.Section, bus channels.InboundPublisher, opts ...channels.Option) (*Channel, error) {
	rawMap, _ := section.Map()
	parsedCfg, err := ParseConfig(rawMap)
	if err != nil {
		return nil, fmt.Errorf("telegram config parse: %w", err)
	}

	c := &Channel{
		cfg:               parsedCfg,
		senderPolicy:      NewSenderPolicy(section, nil),
		messageThreads:    make(map[string]int64),
		compactionNotices: make(map[string]int64),
		inboundBuffers:    make(map[string][]*queuedUpdate),
		inboundWorkers:    make(map[string]context.CancelFunc),
	}

	baseOpts := append([]channels.Option{
		channels.WithName(ChannelName),
		channels.WithDisplayName("Telegram"),
	}, opts...)
	c.Base = channels.NewBase(c, section, bus, baseOpts...)

	// Pass the configured pairing store to senderPolicy
	c.senderPolicy = NewSenderPolicy(section, c.PairingStore())

	// Build BotClient
	proxyStr := ""
	if c.cfg.Proxy != nil {
		proxyStr = *c.cfg.Proxy
	}
	c.client = NewBotClient(c.cfg.Token, proxyStr)

	return c, nil
}

// SetClient overrides the internal BotClient (useful for testing).
func (c *Channel) SetClient(client *BotClient) {
	c.client = client
}

// Start runs the channel lifecycle: starts polling updates.
func (c *Channel) Start(ctx context.Context) error {
	if c.cfg.Token == "" {
		c.Logger().Error("bot token not configured")
		return errors.New("telegram: bot token not configured")
	}

	c.stopCtx, c.cancelStop = context.WithCancel(ctx)
	defer c.cancelStop()

	c.SetRunning(true)
	defer c.SetRunning(false)

	backoff := RestartBackoffInitialSeconds
	for c.IsRunning() && c.stopCtx.Err() == nil {
		err := c.runLifecycle(c.stopCtx)
		if err == nil || errors.Is(err, context.Canceled) {
			break
		}

		if errors.Is(err, ErrTokenInvalid) {
			c.Logger().Error("bot token rejected by Telegram")
			return errors.New("telegram: bot token was rejected by the server")
		}

		c.Logger().Error("telegram lifecycle failed; retrying", "error", err, "backoff_sec", backoff)
		select {
		case <-c.stopCtx.Done():
			return nil
		case <-time.After(time.Duration(backoff * float64(time.Second))):
		}
		backoff = min(backoff*2, RestartBackoffMaxSeconds)
	}

	return nil
}

func (c *Channel) runLifecycle(ctx context.Context) error {
	// Initialize bot info
	user, err := c.client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	c.botUserID = user.ID
	if user.Username != nil {
		c.botUsername = *user.Username
	}
	c.Logger().Info("bot connected", "username", c.botUsername, "id", c.botUserID)

	// Register commands
	if err := c.client.SetMyCommands(ctx, SetMyCommandsParams{Commands: DefaultBotCommands()}); err != nil {
		c.Logger().Warn("failed to register bot commands", "error", err)
	}

	// Delete webhook if needed to switch to polling
	if err := c.client.DeleteWebhook(ctx, DeleteWebhookParams{DropPendingUpdates: false}); err != nil {
		c.Logger().Debug("deleteWebhook before polling", "error", err)
	}

	return c.pollLoop(ctx)
}

// pollLoop performs long polling with getUpdates.
func (c *Channel) pollLoop(ctx context.Context) error {
	var offset int64 = 0
	allowedUpdates := []string{"message"}
	if c.cfg.InlineKeyboards {
		allowedUpdates = append(allowedUpdates, "callback_query")
	}

	c.Logger().Info("starting bot in polling mode")

	pollTimeout := 10 // seconds for long polling
	for c.IsRunning() && ctx.Err() == nil {
		params := GetUpdatesParams{
			Offset:         offset,
			Limit:          100,
			Timeout:        pollTimeout,
			AllowedUpdates: allowedUpdates,
		}

		// Request getUpdates
		updates, err := c.client.GetUpdates(ctx, params)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				if apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
					// Cancellable wait: the reference awaits asyncio.sleep, which
					// shutdown interrupts. time.Sleep would delay Stop by the
					// whole RetryAfter window.
					if !sleepCtx(ctx, time.Duration(apiErr.Parameters.RetryAfter)*time.Second) {
						return nil
					}
					continue
				}
			}
			c.Logger().Warn("getUpdates failed", "error", err)
			if !sleepCtx(ctx, time.Second) {
				return nil
			}
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			c.dispatchUpdate(u)
		}
	}

	return nil
}

// sleepCtx waits for d, returning false if ctx was cancelled first. The
// reference uses asyncio.sleep, which a shutdown interrupts; time.Sleep is not
// interruptible and would stall Stop for the full delay.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// dispatchUpdate routes an incoming update.
func (c *Channel) dispatchUpdate(u Update) {
	if u.Message != nil {
		msg := u.Message
		text := msg.Text

		// Check if it's a slash command
		if strings.HasPrefix(text, "/") {
			// Strip @bot_username if present
			cmdPart := text
			if idx := strings.Index(cmdPart, " "); idx != -1 {
				cmdPart = cmdPart[:idx]
			}
			if atIdx := strings.Index(cmdPart, "@"); atIdx != -1 {
				targetBot := cmdPart[atIdx+1:]
				if c.botUsername != "" && !strings.EqualFold(targetBot, c.botUsername) {
					// Addressed to another bot
					return
				}
			}

			if isSlashCommand(text) {
				c.enqueueOrderedUpdate("command", u)
				return
			}
		}

		// Plain message
		c.enqueueOrderedUpdate("message", u)
		return
	}

	if u.CallbackQuery != nil && c.cfg.InlineKeyboards {
		c.handleCallbackQuery(u.CallbackQuery)
		return
	}
}

func isSlashCommand(text string) bool {
	if text == "/start" || strings.HasPrefix(text, "/start@") || strings.HasPrefix(text, "/start ") {
		return true
	}
	if text == "/help" || strings.HasPrefix(text, "/help@") || strings.HasPrefix(text, "/help ") {
		return true
	}
	if BusSlashCommandRe.MatchString(text) {
		return true
	}
	for _, alias := range CommandAliases {
		if text == alias.Canonical || strings.HasPrefix(text, alias.Canonical+" ") ||
			strings.HasPrefix(text, alias.Canonical+"@") {
			return true
		}
	}
	return false
}

// handleCallbackQuery processes an inline keyboard button press.
func (c *Channel) handleCallbackQuery(cb *CallbackQuery) {
	if cb == nil {
		return
	}
	// Best-effort answer callback query
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.client.AnswerCallbackQuery(ctx, AnswerCallbackQueryParams{
			CallbackQueryID: cb.ID,
		})
	}()

	if cb.Message == nil || cb.From == nil {
		return
	}

	senderID := c.senderID(cb.From)
	if !c.senderPolicy.IsAllowed(senderID) {
		return
	}

	chatID := strconv.FormatInt(cb.Message.Chat.ID, 10)
	c.rememberThreadContext(cb.Message)

	meta := c.buildMessageMetadata(cb.Message, cb.From)
	meta["callback_query_id"] = cb.ID

	var sessionKey *string
	if sk := c.deriveTopicSessionKey(cb.Message); sk != "" {
		sessionKey = &sk
	}

	_ = c.HandleMessage(context.Background(), channels.InboundRequest{
		SenderID:   senderID,
		ChatID:     chatID,
		Content:    cb.Data,
		Metadata:   meta,
		SessionKey: sessionKey,
		IsDM:       cb.Message.Chat.Type == "private",
	})
}

// Stop shuts down the channel and releases resources.
func (c *Channel) Stop(ctx context.Context) error {
	c.SetRunning(false)
	if c.cancelStop != nil {
		c.cancelStop()
	}

	// Cancel all inbound workers
	c.inboundMu.Lock()
	for _, cancel := range c.inboundWorkers {
		cancel()
	}
	c.inboundWorkers = make(map[string]context.CancelFunc)
	c.inboundBuffers = make(map[string][]*queuedUpdate)
	c.inboundMu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Inbound ordering & dispatch
// ---------------------------------------------------------------------------

func (c *Channel) enqueueOrderedUpdate(kind string, u Update) {
	if u.Message == nil {
		return
	}
	key := c.queueKeyForMessage(u.Message)

	c.inboundMu.Lock()
	defer c.inboundMu.Unlock()

	item := &queuedUpdate{
		kind:      kind,
		update:    u,
		messageID: u.Message.MessageID,
		updateID:  u.UpdateID,
	}

	c.inboundBuffers[key] = append(c.inboundBuffers[key], item)
	if _, exists := c.inboundWorkers[key]; !exists {
		workerCtx, cancel := context.WithCancel(context.Background())
		c.inboundWorkers[key] = cancel
		go c.drainOrderedUpdates(workerCtx, key)
	}
}

func (c *Channel) drainOrderedUpdates(ctx context.Context, key string) {
	defer func() {
		c.inboundMu.Lock()
		defer c.inboundMu.Unlock()
		if len(c.inboundBuffers[key]) == 0 {
			delete(c.inboundBuffers, key)
			delete(c.inboundWorkers, key)
			return
		}
		// The reference cannot reach this branch: Python's event loop does not
		// yield between the empty-batch break and the worker cleanup, so no
		// coroutine can enqueue in that window. Go goroutines are truly
		// concurrent, so an update may arrive after our final drain. Hand the
		// buffer to a fresh worker instead of stranding it forever.
		workerCtx, cancel := context.WithCancel(context.Background())
		c.inboundWorkers[key] = cancel
		go c.drainOrderedUpdates(workerCtx, key)
	}()

	for c.IsRunning() && ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}

		c.inboundMu.Lock()
		batch := c.inboundBuffers[key]
		c.inboundBuffers[key] = nil
		c.inboundMu.Unlock()

		if len(batch) == 0 {
			break
		}

		// Sort batch by (messageID, updateID)
		sortQueuedUpdates(batch)

		for _, item := range batch {
			if ctx.Err() != nil {
				return
			}
			if item.kind == "command" {
				c.processForwardCommand(ctx, item.update)
			} else {
				c.processMessageUpdate(ctx, item.update)
			}
		}
	}
}

func sortQueuedUpdates(batch []*queuedUpdate) {
	for i := 1; i < len(batch); i++ {
		for j := i; j > 0; j-- {
			if batch[j].messageID < batch[j-1].messageID ||
				(batch[j].messageID == batch[j-1].messageID && batch[j].updateID < batch[j-1].updateID) {
				batch[j], batch[j-1] = batch[j-1], batch[j]
			} else {
				break
			}
		}
	}
}

func (c *Channel) queueKeyForMessage(msg *Message) string {
	if sk := c.deriveTopicSessionKey(msg); sk != "" {
		return sk
	}
	return fmt.Sprintf("telegram:%d", msg.Chat.ID)
}

func (c *Channel) senderID(user *User) string {
	if user == nil {
		return ""
	}
	sid := strconv.FormatInt(user.ID, 10)
	if user.Username != nil && *user.Username != "" {
		return fmt.Sprintf("%s|%s", sid, *user.Username)
	}
	return sid
}

func (c *Channel) deriveTopicSessionKey(msg *Message) string {
	if msg == nil || msg.Chat == nil || msg.MessageThreadID == nil {
		return ""
	}
	return fmt.Sprintf("telegram:%d:topic:%d", msg.Chat.ID, *msg.MessageThreadID)
}

func (c *Channel) rememberThreadContext(msg *Message) {
	if msg == nil || msg.Chat == nil || msg.MessageThreadID == nil {
		return
	}
	key := fmt.Sprintf("%d:%d", msg.Chat.ID, msg.MessageID)
	c.threadsMu.Lock()
	defer c.threadsMu.Unlock()
	c.messageThreads[key] = *msg.MessageThreadID
	if len(c.messageThreads) > 1000 {
		for k := range c.messageThreads {
			delete(c.messageThreads, k)
			break
		}
	}
}

func (c *Channel) buildMessageMetadata(msg *Message, user *User) map[string]any {
	meta := map[string]any{
		"message_id": msg.MessageID,
		"is_group":   msg.Chat.Type != "private",
		"is_forum":   msg.Chat.IsForum,
	}
	if user != nil {
		meta["user_id"] = user.ID
		if user.Username != nil {
			meta["username"] = *user.Username
		}
		if user.FirstName != "" {
			meta["first_name"] = user.FirstName
		}
	}
	if msg.MessageThreadID != nil {
		meta["message_thread_id"] = *msg.MessageThreadID
	}
	if msg.ReplyToMessage != nil {
		meta["reply_to_message_id"] = msg.ReplyToMessage.MessageID
	}
	return meta
}

func (c *Channel) processForwardCommand(ctx context.Context, u Update) {
	msg := u.Message
	if msg == nil || msg.From == nil {
		return
	}
	senderID := c.senderID(msg.From)
	c.rememberThreadContext(msg)

	content := msg.Text
	// Strip @bot_username suffix
	if strings.HasPrefix(content, "/") && strings.Contains(content, "@") {
		parts := strings.SplitN(content, " ", 2)
		cmdPart := strings.SplitN(parts[0], "@", 2)[0]
		if len(parts) > 1 {
			content = cmdPart + " " + parts[1]
		} else {
			content = cmdPart
		}
	}
	content = NormalizeCommand(content)

	var sessionKey *string
	if sk := c.deriveTopicSessionKey(msg); sk != "" {
		sessionKey = &sk
	}

	_ = c.HandleMessage(ctx, channels.InboundRequest{
		SenderID:   senderID,
		ChatID:     strconv.FormatInt(msg.Chat.ID, 10),
		Content:    content,
		Metadata:   c.buildMessageMetadata(msg, msg.From),
		SessionKey: sessionKey,
		IsDM:       msg.Chat.Type == "private",
	})
}

func (c *Channel) processMessageUpdate(ctx context.Context, u Update) {
	msg := u.Message
	if msg == nil || msg.From == nil {
		return
	}
	senderID := c.senderID(msg.From)
	c.rememberThreadContext(msg)

	// Check group policy
	if !c.isGroupMessageForBot(msg) {
		return
	}

	content := msg.Text
	if content == "" && msg.Caption != "" {
		content = msg.Caption
	}
	if content == "" {
		content = "[empty message]"
	}

	var sessionKey *string
	if sk := c.deriveTopicSessionKey(msg); sk != "" {
		sessionKey = &sk
	}

	_ = c.HandleMessage(ctx, channels.InboundRequest{
		SenderID:   senderID,
		ChatID:     strconv.FormatInt(msg.Chat.ID, 10),
		Content:    content,
		Metadata:   c.buildMessageMetadata(msg, msg.From),
		SessionKey: sessionKey,
		IsDM:       msg.Chat.Type == "private",
	})
}

func (c *Channel) isGroupMessageForBot(msg *Message) bool {
	var entities []MentionEntity
	for _, e := range msg.Entities {
		var uid *int64
		if e.User != nil {
			uid = &e.User.ID
		}
		offset := e.Offset
		length := e.Length
		entities = append(entities, MentionEntity{
			Type:   e.Type,
			Offset: &offset,
			Length: &length,
			UserID: uid,
		})
	}
	var captionEntities []MentionEntity
	for _, e := range msg.CaptionEntities {
		var uid *int64
		if e.User != nil {
			uid = &e.User.ID
		}
		offset := e.Offset
		length := e.Length
		captionEntities = append(captionEntities, MentionEntity{
			Type:   e.Type,
			Offset: &offset,
			Length: &length,
			UserID: uid,
		})
	}

	var replyUserID *int64
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		replyUserID = &msg.ReplyToMessage.From.ID
	}

	input := GroupMessageInput{
		ChatType:        msg.Chat.Type,
		Text:            msg.Text,
		Entities:        entities,
		Caption:         msg.Caption,
		CaptionEntities: captionEntities,
		ReplyToUserID:   replyUserID,
	}

	var botID *int64
	if c.botUserID != 0 {
		botID = &c.botUserID
	}
	return IsGroupMessageForBot(input, c.cfg.GroupPolicy, botID, c.botUsername)
}

// ---------------------------------------------------------------------------
// Outbound Send
// ---------------------------------------------------------------------------

// Send delivers one outbound message.
func (c *Channel) Send(ctx context.Context, msg core.OutboundMessage) error {
	chatIDInt, err := strconv.ParseInt(msg.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram invalid chat_id %q: %w", msg.ChatID, err)
	}

	// Resolve message_thread_id
	var messageThreadID *int64
	if mtid, ok := msg.Metadata["message_thread_id"].(int64); ok {
		messageThreadID = &mtid
	} else if mtidFloat, ok := msg.Metadata["message_thread_id"].(float64); ok {
		val := int64(mtidFloat)
		messageThreadID = &val
	}

	// Resolve reply parameters
	var replyParams *ReplyParameters
	if c.cfg.ReplyToMessage {
		if rmid, ok := msg.Metadata["message_id"].(int64); ok && rmid > 0 {
			replyParams = &ReplyParameters{
				MessageID:                rmid,
				AllowSendingWithoutReply: true,
			}
			if messageThreadID == nil {
				c.threadsMu.Lock()
				if tid, exists := c.messageThreads[fmt.Sprintf("%s:%d", msg.ChatID, rmid)]; exists {
					messageThreadID = &tid
				}
				c.threadsMu.Unlock()
			}
		}
	}

	// Compaction notice handling
	if compEvent, ok := msg.Event.(events.ContextCompactionEvent); ok {
		return c.sendCompactionNotice(ctx, chatIDInt, msg, compEvent, replyParams, messageThreadID)
	}

	// Text content
	if msg.Content != "" && msg.Content != "[empty message]" {
		text := msg.Content
		if renderAs, ok := msg.Metadata["render_as"].(string); ok && renderAs == "text" {
			text = DisplayCommandText(text)
		}

		chunks := SplitMarkdown(text, MaxMessageLen)
		for _, chunk := range chunks {
			html := MarkdownToHTML(chunk)
			params := SendMessageParams{
				ChatID:          chatIDInt,
				Text:            html,
				ParseMode:       "HTML",
				MessageThreadID: messageThreadID,
				ReplyParameters: replyParams,
			}

			if err := c.sendWithRetry(ctx, func() error {
				_, err := c.client.SendMessage(ctx, params)
				return err
			}); err != nil {
				// HTML parse fallback to plain text if 400 Bad Request
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.ErrorCode == 400 {
					c.Logger().Warn("HTML parse failed, falling back to plain text", "error", err)
					fallbackParams := params
					fallbackParams.Text = chunk
					fallbackParams.ParseMode = ""
					if errFallback := c.sendWithRetry(ctx, func() error {
						_, err := c.client.SendMessage(ctx, fallbackParams)
						return err
					}); errFallback != nil {
						return errFallback
					}
				} else {
					return err
				}
			}
		}
	}

	return nil
}

func (c *Channel) sendCompactionNotice(
	ctx context.Context,
	chatID int64,
	msg core.OutboundMessage,
	event events.ContextCompactionEvent,
	replyParams *ReplyParameters,
	threadID *int64,
) error {
	key := fmt.Sprintf("%s:%s", msg.ChatID, event.CompactionID)

	c.compactionMu.Lock()
	if event.Phase == events.CompactionStarted {
		c.compactionMu.Unlock()
		var sentMsg *Message
		err := c.sendWithRetry(ctx, func() error {
			var sendErr error
			sentMsg, sendErr = c.client.SendMessage(ctx, SendMessageParams{
				ChatID:          chatID,
				Text:            msg.Content,
				ReplyParameters: replyParams,
				MessageThreadID: threadID,
			})
			return sendErr
		})
		if err != nil {
			return err
		}

		c.compactionMu.Lock()
		defer c.compactionMu.Unlock()
		if len(c.compactionNotices) >= CompactionNoticesMax {
			for k := range c.compactionNotices {
				delete(c.compactionNotices, k)
				break
			}
		}
		c.compactionNotices[key] = sentMsg.MessageID
		return nil
	}

	messageID, exists := c.compactionNotices[key]
	delete(c.compactionNotices, key)
	c.compactionMu.Unlock()

	if !exists || msg.Content == "" {
		// Fresh send
		return c.sendWithRetry(ctx, func() error {
			_, err := c.client.SendMessage(ctx, SendMessageParams{
				ChatID:          chatID,
				Text:            msg.Content,
				MessageThreadID: threadID,
			})
			return err
		})
	}

	// Edit existing notice
	err := c.sendWithRetry(ctx, func() error {
		_, err := c.client.EditMessageText(ctx, EditMessageTextParams{
			ChatID:    chatID,
			MessageID: messageID,
			Text:      msg.Content,
		})
		return err
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsNotModified() {
			return nil
		}
		c.Logger().Warn("compaction notice edit failed, sending anew", "error", err)
		return c.sendWithRetry(ctx, func() error {
			_, err := c.client.SendMessage(ctx, SendMessageParams{
				ChatID:          chatID,
				Text:            msg.Content,
				MessageThreadID: threadID,
			})
			return err
		})
	}

	return nil
}

func (c *Channel) sendWithRetry(ctx context.Context, fn func() error) error {
	var lastErr error
	delay := time.Duration(SendRetryBaseDelay * float64(time.Second))

	for attempt := 1; attempt <= SendMaxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.ErrorCode == 429 && apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
				sleepDuration := time.Duration(apiErr.Parameters.RetryAfter) * time.Second
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(sleepDuration):
					continue
				}
			}
			if !apiErr.IsTransient() {
				// Non-transient 4xx error (e.g. 400 Bad Request, 403 Forbidden)
				return err
			}
		}

		if attempt < SendMaxRetries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
	}

	return lastErr
}
