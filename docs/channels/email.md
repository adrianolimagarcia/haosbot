# Email channel

The Email transport gives HAOSbot a dedicated mailbox. It polls IMAP for unseen
messages and sends replies with SMTP.

## Configuration

```json
{
  "channels": {
    "email": {
      "enabled": true,
      "consentGranted": true,
      "imapHost": "imap.gmail.com",
      "imapPort": 993,
      "imapUsername": "bot@example.com",
      "imapPassword": "app-password",
      "imapUseTls": true,
      "mailbox": "INBOX",
      "smtpHost": "smtp.gmail.com",
      "smtpPort": 587,
      "smtpUsername": "bot@example.com",
      "smtpPassword": "app-password",
      "smtpUseTls": true,
      "smtpUseSsl": false,
      "fromAddress": "bot@example.com",
      "allowFrom": ["owner@example.com"],
      "pollIntervalSeconds": 30,
      "autoReplyEnabled": true
    }
  }
}
```

`consentGranted` is a hard safety gate. The runtime refuses to access the
mailbox unless it is explicitly true. IMAP uses implicit TLS by default. SMTP
uses STARTTLS by default; set `smtpUseSsl=true` and `smtpUseTls=false` for
implicit TLS (typically port 465).

Incoming mail is accepted only from `allowFrom` (or `["*"]`). Messages are
marked Seen only after they are safely ignored or successfully delivered to the
agent pipeline; transient dispatch failures remain unseen for retry. Progress
and tool-hint events are disabled for this transport so a single agent turn does
not generate multiple status emails.

Current scope is deliberately text-first: MIME text/plain is preferred and
text/html is reduced to text. Attachments and DKIM/SPF policy enforcement are
not yet implemented in this Go transport.
