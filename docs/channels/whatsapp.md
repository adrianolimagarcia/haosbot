# WhatsApp Cloud API

This transport uses Meta's official WhatsApp Cloud API. It receives text and interactive reply webhooks and sends text messages through Graph API. It does not use WhatsApp Web, QR login, or a direct phone protocol.

```json
{
  "channels": {
    "whatsapp": {
      "enabled": true,
      "listenAddr": "127.0.0.1:8089",
      "webhookPath": "/webhooks/whatsapp",
      "phoneNumberId": "123456789012345",
      "accessToken": "replace-with-a-private-cloud-api-token",
      "appSecret": "replace-with-meta-app-secret",
      "verifyToken": "choose-a-random-webhook-verification-token",
      "graphVersion": "vNN.N",
      "allowFrom": ["15557654321"]
    }
  }
}
```

Configure Meta's webhook URL to reach the `listenAddr` and `webhookPath` through a public HTTPS reverse proxy, and subscribe the app to `messages`. The GET handshake uses `verifyToken`; POST deliveries are checked with `X-Hub-Signature-256` and the app secret. Use a currently supported Graph API version in `graphVersion`. `allowFrom` optionally restricts senders.

Only text and button/list reply messages are passed to the agent. Media, templates, status receipts, and group chats are not handled. Meta's customer-service window and messaging rules still apply to outbound sends.
