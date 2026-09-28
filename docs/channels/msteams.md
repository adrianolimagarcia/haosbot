# Microsoft Teams channel

HAOSbot can receive Microsoft Teams Bot Framework activities over HTTPS and
reply to the same personal conversation through the Bot Framework connector.

## Configuration

```json
{
  "channels": {
    "msteams": {
      "enabled": true,
      "appId": "AZURE_BOT_APP_ID",
      "appPassword": "AZURE_BOT_SECRET",
      "tenantId": "YOUR_TENANT_ID",
      "host": "0.0.0.0",
      "port": 3978,
      "path": "/api/messages",
      "allowFrom": ["AAD_OBJECT_ID"],
      "replyInThread": true,
      "validateInboundAuth": true
    }
  }
}
```

Expose `/api/messages` through a public HTTPS reverse proxy and configure that
URL as the bot messaging endpoint.

The transport validates inbound Bot Framework bearer tokens by fetching the
Bot Framework OpenID metadata/JWKS, requiring RS256, issuer
`https://api.botframework.com`, the configured app ID as audience, token
lifetime, and matching `serviceUrl` when the claim is present. It accepts
outbound connector URLs only from the configured trusted host list and obtains
outbound tokens through Microsoft identity client-credentials OAuth.

The current runtime is DM-first: non-personal conversations are ignored. It
keeps conversation references in process memory, so a new inbound message is
required after a HAOSbot restart before proactive replies can resume.
