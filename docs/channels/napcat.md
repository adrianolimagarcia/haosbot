# Napcat / OneBot 11 channel

The Napcat transport uses a OneBot 11 forward WebSocket for events and the HTTP API's `send_msg` action for replies. Configure Napcat with both a WebSocket server and HTTP API, and set the same access token on each.

```json
{
  "channels": {
    "napcat": {
      "enabled": true,
      "websocketUrl": "ws://127.0.0.1:3001/onebot/v11/ws",
      "apiBase": "http://127.0.0.1:3000",
      "accessToken": "replace-with-a-private-token",
      "selfId": "123456789",
      "allowFrom": ["987654321"],
      "allowedGroups": ["1122334455"]
    }
  }
}
```

`allowFrom` matches QQ user IDs through the common channel policy; private messages can use HAOSbot's pairing flow. Group messages require an allowlist entry or `*`. `allowedGroups` can further restrict which group IDs are monitored. For non-local endpoints, use TLS and keep the token private.

This first version handles private/group text messages, filters the bot's own ID, and sends plain text. It does not handle media segments, message edits/deletes, multiple bot instances, or streaming.
