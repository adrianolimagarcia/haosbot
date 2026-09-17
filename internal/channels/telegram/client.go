package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBaseURL is the standard Telegram Bot API base URL.
const DefaultAPIBaseURL = "https://api.telegram.org"

// Common Bot API error representations.
var (
	ErrTokenInvalid = errors.New("telegram: token rejected (401/404)")
)

// APIError represents an error response from Telegram Bot API.
type APIError struct {
	ErrorCode   int
	Description string
	Parameters  *ResponseParameters
}

func (e *APIError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("telegram api error (%d): %s", e.ErrorCode, e.Description)
	}
	return fmt.Sprintf("telegram api error (%d)", e.ErrorCode)
}

// IsNotModified reports whether the error is "message is not modified".
func (e *APIError) IsNotModified() bool {
	return e.ErrorCode == 400 && strings.Contains(strings.ToLower(e.Description), "message is not modified")
}

// IsTransient reports whether the error is worth retrying.
func (e *APIError) IsTransient() bool {
	if e.ErrorCode == 429 {
		return true
	}
	if e.ErrorCode >= 500 {
		return true
	}
	return false
}

// ResponseParameters describes why a request was unsuccessful and how long to wait.
type ResponseParameters struct {
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
	RetryAfter      int   `json:"retry_after,omitempty"`
}

// apiResponse is the Telegram Bot API JSON envelope.
type apiResponse[T any] struct {
	OK          bool                `json:"ok"`
	Result      T                   `json:"result,omitempty"`
	ErrorCode   int                 `json:"error_code,omitempty"`
	Description string              `json:"description,omitempty"`
	Parameters  *ResponseParameters `json:"parameters,omitempty"`
}

// User represents a Telegram user or bot.
type User struct {
	ID        int64   `json:"id"`
	IsBot     bool    `json:"is_bot"`
	FirstName string  `json:"first_name"`
	LastName  *string `json:"last_name,omitempty"`
	Username  *string `json:"username,omitempty"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID       int64   `json:"id"`
	Type     string  `json:"type"` // "private", "group", "supergroup", "channel"
	Title    *string `json:"title,omitempty"`
	Username *string `json:"username,omitempty"`
	IsForum  bool    `json:"is_forum,omitempty"`
}

// Message represents a Telegram message.
type Message struct {
	MessageID       int64              `json:"message_id"`
	MessageThreadID *int64             `json:"message_thread_id,omitempty"`
	From            *User              `json:"from,omitempty"`
	Chat            *Chat              `json:"chat,omitempty"`
	Date            int64              `json:"date"`
	Text            string             `json:"text,omitempty"`
	Entities        []APIMessageEntity `json:"entities,omitempty"`
	Caption         string             `json:"caption,omitempty"`
	CaptionEntities []APIMessageEntity `json:"caption_entities,omitempty"`
	ReplyToMessage  *Message           `json:"reply_to_message,omitempty"`
	MediaGroupID    *string            `json:"media_group_id,omitempty"`
}

// APIMessageEntity mirrors MessageEntity in Telegram Bot API.
type APIMessageEntity struct {
	Type   string `json:"type"` // "mention", "text_mention", "bot_command", etc.
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	URL    string `json:"url,omitempty"`
	User   *User  `json:"user,omitempty"`
}

// CallbackQuery represents an incoming callback query from an inline keyboard.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from,omitempty"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Update represents an incoming update from getUpdates or a webhook.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// BotCommand represents a bot command registered with setMyCommands.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// DefaultBotCommands returns the standard bot commands (matching runtime.py:515-534).
func DefaultBotCommands() []BotCommand {
	return []BotCommand{
		{Command: "start", Description: "Start the bot"},
		{Command: "new", Description: "Start a new conversation"},
		{Command: "compact", Description: "Compact this chat's context"},
		{Command: "stop", Description: "Stop the current task"},
		{Command: "restart", Description: "Restart the bot"},
		{Command: "status", Description: "Show bot status"},
		{Command: "history", Description: "Show recent conversation messages"},
		{Command: "goal", Description: "Start a sustained objective (long-running task)"},
		{Command: "trigger", Description: "Create a named local trigger"},
		{Command: "pairing", Description: "Manage DM pairing (approve/deny/list)"},
		{Command: "model", Description: "Switch runtime model preset"},
		{Command: "skill", Description: "List enabled skills"},
		{Command: "dream", Description: "Run Dream memory consolidation now"},
		{Command: "dream_log", Description: "Show the latest Dream memory change"},
		{Command: "dream_restore", Description: "Restore Dream memory to an earlier version"},
		{Command: "dream_prompt", Description: "Tell Dream how to organize memory"},
		{Command: "evaluator_prompt", Description: "Customize the heartbeat evaluator prompt"},
		{Command: "help", Description: "Show available commands"},
	}
}

// ReplyParameters mirrors ReplyParameters in Bot API.
type ReplyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply,omitempty"`
}

// InlineKeyboardButton represents one button of an inline keyboard.
type InlineKeyboardButton struct {
	Text         string  `json:"text"`
	CallbackData *string `json:"callback_data,omitempty"`
	URL          *string `json:"url,omitempty"`
}

// InlineKeyboardMarkup represents an inline keyboard.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// SendMessageParams contains parameters for sendMessage.
type SendMessageParams struct {
	ChatID          any                   `json:"chat_id"` // int64 or string
	Text            string                `json:"text"`
	ParseMode       string                `json:"parse_mode,omitempty"`
	MessageThreadID *int64                `json:"message_thread_id,omitempty"`
	ReplyParameters *ReplyParameters      `json:"reply_parameters,omitempty"`
	ReplyMarkup     *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// EditMessageTextParams contains parameters for editMessageText.
type EditMessageTextParams struct {
	ChatID          any                   `json:"chat_id,omitempty"` // int64 or string
	MessageID       int64                 `json:"message_id,omitempty"`
	InlineMessageID string                `json:"inline_message_id,omitempty"`
	Text            string                `json:"text"`
	ParseMode       string                `json:"parse_mode,omitempty"`
	ReplyMarkup     *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// SendChatActionParams contains parameters for sendChatAction.
type SendChatActionParams struct {
	ChatID          any    `json:"chat_id"`
	Action          string `json:"action"` // "typing", etc.
	MessageThreadID *int64 `json:"message_thread_id,omitempty"`
}

// SetMyCommandsParams contains parameters for setMyCommands.
type SetMyCommandsParams struct {
	Commands []BotCommand `json:"commands"`
}

// SetWebhookParams contains parameters for setWebhook.
type SetWebhookParams struct {
	URL                string   `json:"url"`
	MaxConnections     int      `json:"max_connections,omitempty"`
	AllowedUpdates     []string `json:"allowed_updates,omitempty"`
	DropPendingUpdates bool     `json:"drop_pending_updates,omitempty"`
	SecretToken        string   `json:"secret_token,omitempty"`
}

// DeleteWebhookParams contains parameters for deleteWebhook.
type DeleteWebhookParams struct {
	DropPendingUpdates bool `json:"drop_pending_updates,omitempty"`
}

// AnswerCallbackQueryParams contains parameters for answerCallbackQuery.
type AnswerCallbackQueryParams struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
	URL             string `json:"url,omitempty"`
	CacheTime       int    `json:"cache_time,omitempty"`
}

// GetUpdatesParams contains parameters for getUpdates.
type GetUpdatesParams struct {
	Offset         int64    `json:"offset,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	Timeout        int      `json:"timeout,omitempty"` // seconds
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// HTTPDoer is an interface satisfied by *http.Client.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// BotClient interacts with the Telegram Bot API using net/http.
type BotClient struct {
	token      string
	baseURL    string
	httpClient HTTPDoer
}

// ClientOption configures BotClient.
type ClientOption func(*BotClient)

// WithBaseURL overrides the API base URL (useful for testing or local Bot API servers).
func WithBaseURL(rawURL string) ClientOption {
	return func(c *BotClient) {
		c.baseURL = strings.TrimRight(rawURL, "/")
	}
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(client HTTPDoer) ClientOption {
	return func(c *BotClient) {
		c.httpClient = client
	}
}

// NewBotClient creates a new Bot API client.
func NewBotClient(token string, proxy string, opts ...ClientOption) *BotClient {
	c := &BotClient{
		token:   token,
		baseURL: DefaultAPIBaseURL,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	if c.httpClient == nil {
		c.httpClient = defaultHTTPClient(proxy)
	}
	return c
}

func defaultHTTPClient(proxy string) *http.Client {
	if proxy != "" && !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	transport := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	if proxy != "" {
		if proxyURL, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Transport: transport,
	}
}

// do executes an API call against method.
func (c *BotClient) do(ctx context.Context, method string, payload any, out any) error {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal %s payload: %w", method, err)
		}
		bodyReader = bytes.NewReader(data)
	}

	endpoint := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("create request %s: %w", method, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &TransportError{Err: err}
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return &TransportError{Err: fmt.Errorf("read response body: %w", err)}
	}

	// Telegram returns JSON even on 4xx/5xx errors.
	var envelope apiResponse[json.RawMessage]
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		if resp.StatusCode >= 400 {
			return &HTTPStatusError{StatusCode: resp.StatusCode}
		}
		return fmt.Errorf("decode %s response: %w", method, err)
	}

	if !envelope.OK {
		if envelope.ErrorCode == 401 || envelope.ErrorCode == 404 {
			return ErrTokenInvalid
		}
		return &APIError{
			ErrorCode:   envelope.ErrorCode,
			Description: envelope.Description,
			Parameters:  envelope.Parameters,
		}
	}

	if out != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("unmarshal %s result: %w", method, err)
		}
	}

	return nil
}

// GetMe calls getMe.
func (c *BotClient) GetMe(ctx context.Context) (*User, error) {
	var user User
	if err := c.do(ctx, "getMe", nil, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUpdates calls getUpdates.
func (c *BotClient) GetUpdates(ctx context.Context, params GetUpdatesParams) ([]Update, error) {
	var updates []Update
	if err := c.do(ctx, "getUpdates", params, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// SendMessage calls sendMessage.
func (c *BotClient) SendMessage(ctx context.Context, params SendMessageParams) (*Message, error) {
	var msg Message
	if err := c.do(ctx, "sendMessage", params, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// EditMessageText calls editMessageText.
func (c *BotClient) EditMessageText(ctx context.Context, params EditMessageTextParams) (*Message, error) {
	var msg Message
	if err := c.do(ctx, "editMessageText", params, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// SendChatAction calls sendChatAction.
func (c *BotClient) SendChatAction(ctx context.Context, params SendChatActionParams) error {
	var ok bool
	return c.do(ctx, "sendChatAction", params, &ok)
}

// SetMyCommands calls setMyCommands.
func (c *BotClient) SetMyCommands(ctx context.Context, params SetMyCommandsParams) error {
	var ok bool
	return c.do(ctx, "setMyCommands", params, &ok)
}

// SetWebhook calls setWebhook.
func (c *BotClient) SetWebhook(ctx context.Context, params SetWebhookParams) error {
	var ok bool
	return c.do(ctx, "setWebhook", params, &ok)
}

// DeleteWebhook calls deleteWebhook.
func (c *BotClient) DeleteWebhook(ctx context.Context, params DeleteWebhookParams) error {
	var ok bool
	return c.do(ctx, "deleteWebhook", params, &ok)
}

// AnswerCallbackQuery calls answerCallbackQuery.
func (c *BotClient) AnswerCallbackQuery(ctx context.Context, params AnswerCallbackQueryParams) error {
	var ok bool
	return c.do(ctx, "answerCallbackQuery", params, &ok)
}
