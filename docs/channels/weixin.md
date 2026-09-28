# WeChat / Weixin channel

This transport speaks the personal WeChat iLink protocol directly from Go. It
uses HTTP long-poll for inbound messages and `ilink/bot/sendmessage` for text
replies, so no desktop WeChat process is required.

## Configuration

```json
{
  "channels": {
    "weixin": {
      "enabled": true,
      "token": "YOUR_ILINK_BOT_TOKEN",
      "allowFrom": ["YOUR_WECHAT_USER_ID"],
      "baseUrl": "https://ilinkai.weixin.qq.com",
      "pollTimeout": 35
    }
  }
}
```

`routeTag` is optional for deployments that require the `SKRouteTag` header.
The token is intentionally a secret setup field. Obtain it through an iLink QR
login flow before enabling the runtime.

The Go runtime matches the text-message core of the current iLink transport:
per-request `X-WECHAT-UIN`, bearer token, app/client-version headers,
`get_updates_buf` cursor, bounded deduplication, context-token caching,
long-poll reconnect/backoff, lifecycle notifications, and 1800-character text
chunking.

Current scope is text-only. Media upload/download, QR-login persistence, typing
indicators and streaming blocks remain separate capabilities and are not
advertised by this transport.
