package email

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

const maxMessageBytes = 5 << 20

type Channel struct {
	*channels.Base
	cfg Config
}

func New(section channels.Section, publisher channels.InboundPublisher) (*Channel, error) {
	cfg, err := sectionConfig(section)
	if err != nil {
		return nil, err
	}
	c := &Channel{cfg: cfg}
	c.Base = channels.NewBase(c, section, publisher,
		channels.WithName(ChannelName), channels.WithDisplayName("Email"))
	return c, nil
}

func (c *Channel) ProgressTransportDefaults() (bool, bool, bool) {
	return false, false, true
}

func (c *Channel) Start(ctx context.Context) error {
	if err := c.cfg.validateRuntime(); err != nil {
		return err
	}
	c.SetRunning(true)
	defer c.SetRunning(false)

	for {
		if err := c.pollOnce(ctx); err != nil && ctx.Err() == nil {
			c.Logger().Warn("email poll failed", "error", err)
		}
		timer := time.NewTimer(c.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
			if !c.IsRunning() {
				return nil
			}
		}
	}
}

func (c *Channel) Stop(context.Context) error {
	c.SetRunning(false)
	return nil
}

func (c *Channel) Send(ctx context.Context, message core.OutboundMessage) error {
	if !c.cfg.AutoReplyEnabled || strings.TrimSpace(message.Content) == "" {
		return nil
	}
	to := strings.TrimSpace(message.ChatID)
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("email: invalid recipient %q: %w", to, err)
	}
	subject := "HAOSbot reply"
	if value, ok := message.Metadata["subject"].(string); ok && strings.TrimSpace(value) != "" {
		subject = strings.TrimSpace(value)
	}
	return c.sendSMTP(ctx, to, subject, message.Content)
}

func (c *Channel) pollOnce(ctx context.Context) error {
	client, err := dialIMAP(ctx, c.cfg)
	if err != nil {
		return err
	}
	defer client.close()
	if err := client.login(c.cfg.IMAPUsername, c.cfg.IMAPPassword); err != nil {
		return err
	}
	if _, _, err := client.command("SELECT " + imapQuote(c.cfg.Mailbox)); err != nil {
		return fmt.Errorf("email: select mailbox: %w", err)
	}
	lines, _, err := client.command("UID SEARCH UNSEEN")
	if err != nil {
		return fmt.Errorf("email: search unseen: %w", err)
	}
	uids := parseSearchUIDs(lines)
	for _, uid := range uids {
		if ctx.Err() != nil {
			return nil
		}
		_, literals, err := client.command("UID FETCH " + uid + " (BODY.PEEK[])")
		if err != nil {
			c.Logger().Warn("email fetch failed", "uid", uid, "error", err)
			continue
		}
		if len(literals) == 0 || len(literals[0]) > maxMessageBytes {
			_, _, _ = client.command("UID STORE " + uid + " +FLAGS.SILENT (\\Seen)")
			continue
		}
		handled, err := c.handleRawEmail(ctx, literals[0])
		if err != nil {
			c.Logger().Warn("email dispatch failed; leaving message unseen for retry", "uid", uid, "error", err)
			continue
		}
		if handled {
			if _, _, err := client.command("UID STORE " + uid + " +FLAGS.SILENT (\\Seen)"); err != nil {
				c.Logger().Warn("email could not mark message seen", "uid", uid, "error", err)
			}
		}
	}
	return nil
}

func (c *Channel) handleRawEmail(ctx context.Context, raw []byte) (bool, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return true, nil
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Address == "" {
		return true, nil
	}
	if !c.IsAllowed(from.Address) {
		return true, nil
	}
	body, err := extractBody(msg.Header, msg.Body)
	if err != nil {
		return true, nil
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return true, nil
	}
	subject := strings.TrimSpace(msg.Header.Get("Subject"))
	content := body
	if subject != "" {
		content = "Subject: " + subject + "\n\n" + body
	}
	metadata := map[string]any{
		"subject": subject,
		"message_id": strings.TrimSpace(msg.Header.Get("Message-ID")),
	}
	if err := c.HandleMessage(ctx, channels.InboundRequest{
		SenderID: from.Address,
		ChatID: from.Address,
		Content: content,
		Metadata: metadata,
		IsDM: true,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func extractBody(header mail.Header, body io.Reader) (string, error) {
	mediaType, params, _ := mime.ParseMediaType(header.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		var htmlFallback string
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
			partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			text, err := readDecoded(part, part.Header.Get("Content-Transfer-Encoding"))
			_ = part.Close()
			if err != nil {
				continue
			}
			if partType == "text/plain" {
				return text, nil
			}
			if partType == "text/html" && htmlFallback == "" {
				htmlFallback = stripHTML(text)
			}
		}
		return htmlFallback, nil
	}
	text, err := readDecoded(body, header.Get("Content-Transfer-Encoding"))
	if err != nil {
		return "", err
	}
	if mediaType == "text/html" {
		return stripHTML(text), nil
	}
	return text, nil
}

func readDecoded(r io.Reader, transfer string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(transfer)) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxMessageBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxMessageBytes {
		return "", errors.New("email body exceeded size limit")
	}
	return string(data), nil
}

func stripHTML(value string) string {
	var out strings.Builder
	inTag := false
	for _, r := range value {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
			out.WriteRune(' ')
		default:
			if !inTag {
				out.WriteRune(r)
			}
		}
	}
	return strings.Join(strings.Fields(html.UnescapeString(out.String())), " ")
}

func (c *Channel) sendSMTP(ctx context.Context, recipient, subject, content string) error {
	address := net.JoinHostPort(c.cfg.SMTPHost, strconv.Itoa(c.cfg.SMTPPort))
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	tlsConfig := &tls.Config{ServerName: c.cfg.SMTPHost, MinVersion: tls.VersionTLS12}
	if c.cfg.SMTPUseSSL {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("email: SMTP connect: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	client, err := smtp.NewClient(conn, c.cfg.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()
	if c.cfg.SMTPUseTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("email: SMTP server does not advertise STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if c.cfg.SMTPUsername != "" {
		if err := client.Auth(smtp.PlainAuth("", c.cfg.SMTPUsername, c.cfg.SMTPPassword, c.cfg.SMTPHost)); err != nil {
			return fmt.Errorf("email: SMTP auth: %w", err)
		}
	}
	from, _ := mail.ParseAddress(c.cfg.FromAddress)
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(recipient); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	message := "From: " + from.Address + "\r\n" +
		"To: " + recipient + "\r\n" +
		"Subject: " + sanitizeHeader(subject) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" + content
	if _, err := io.WriteString(writer, message); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func sanitizeHeader(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

type imapClient struct {
	conn net.Conn
	r *bufio.Reader
	w *bufio.Writer
	seq int
}

func dialIMAP(ctx context.Context, cfg Config) (*imapClient, error) {
	address := net.JoinHostPort(cfg.IMAPHost, strconv.Itoa(cfg.IMAPPort))
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	if cfg.IMAPUseTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{
			ServerName: cfg.IMAPHost, MinVersion: tls.VersionTLS12,
		}}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("email: IMAP connect: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	client := &imapClient{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
	greeting, err := client.r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(strings.ToUpper(greeting), "* OK") {
		conn.Close()
		return nil, fmt.Errorf("email: IMAP rejected connection: %s", strings.TrimSpace(greeting))
	}
	return client, nil
}

func (c *imapClient) close() {
	if c == nil || c.conn == nil {
		return
	}
	_, _, _ = c.command("LOGOUT")
	_ = c.conn.Close()
}

func (c *imapClient) login(username, password string) error {
	_, _, err := c.command("LOGIN " + imapQuote(username) + " " + imapQuote(password))
	return err
}

func (c *imapClient) command(command string) ([]string, [][]byte, error) {
	c.seq++
	tag := fmt.Sprintf("A%04d", c.seq)
	if _, err := fmt.Fprintf(c.w, "%s %s\r\n", tag, command); err != nil {
		return nil, nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, nil, err
	}
	var lines []string
	var literals [][]byte
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return lines, literals, err
		}
		lines = append(lines, line)
		if size, ok := trailingLiteralSize(line); ok {
			if size > maxMessageBytes {
				return lines, literals, errors.New("email: IMAP literal exceeded size limit")
			}
			payload := make([]byte, size)
			if _, err := io.ReadFull(c.r, payload); err != nil {
				return lines, literals, err
			}
			literals = append(literals, payload)
		}
		if strings.HasPrefix(line, tag+" ") {
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, tag+" OK") {
				return lines, literals, nil
			}
			return lines, literals, fmt.Errorf("IMAP command failed: %s", strings.TrimSpace(line))
		}
	}
}

func trailingLiteralSize(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	start := strings.LastIndex(line, "{")
	if start < 0 {
		return 0, false
	}
	size, err := strconv.Atoi(strings.TrimSuffix(line[start+1:], "}"))
	return size, err == nil
}

func parseSearchUIDs(lines []string) []string {
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "* SEARCH") {
			continue
		}
		for _, token := range strings.Fields(strings.TrimSpace(line[len("* SEARCH"):])) {
			if _, err := strconv.ParseUint(token, 10, 64); err == nil {
				out = append(out, token)
			}
		}
	}
	return out
}

func imapQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return "\"" + value + "\""
}
