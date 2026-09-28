# Signal channel

Signal uses the companion [`signal-cli-rest-api`](https://github.com/bbernhard/signal-cli-rest-api) service. HAOSbot connects to its receive WebSocket and send REST endpoint; the companion handles Signal registration, device linking, and the Signal protocol.

```json
{
  "channels": {
    "signal": {
      "enabled": true,
      "apiBase": "http://127.0.0.1:8080",
      "number": "+15551234567",
      "apiToken": "",
      "allowFrom": ["+15557654321"]
    }
  }
}
```

The account number must already be registered and linked in the companion service. If the service is exposed through a reverse proxy, set `apiToken` only when that proxy expects a Bearer token. `allowFrom` optionally restricts inbound senders. Direct-message pairing uses the shared HAOSbot pairing policy; group messages use the Signal group ID as the chat ID.

This version handles text messages only. Attachments, reactions, typing state, message edits/deletes, and registration/QR linking are not surfaced in HAOSbot.
