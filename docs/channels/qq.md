# QQ via OneBot 11

HAOSbot can connect a QQ bot through an installed OneBot 11 gateway such as Napcat. This reuses the Napcat WebSocket and HTTP API protocol; it does not log in to QQ directly or implement the QQ client protocol.

```json
{
  "channels": {
    "qq": {
      "enabled": true,
      "websocketUrl": "ws://127.0.0.1:3001/ws",
      "apiBase": "http://127.0.0.1:3000",
      "accessToken": "replace-with-onebot-token",
      "selfId": "123456789",
      "allowFrom": ["987654321"],
      "allowedGroups": ["123456789"]
    }
  }
}
```

The OneBot gateway must already be linked to QQ and reachable from HAOSbot. Private messages use HAOSbot's shared pairing policy. Group traffic can be restricted by `allowedGroups`. Configure either the `qq` alias or `napcat` against a given gateway, not both, to avoid receiving and replying to the same events twice.
