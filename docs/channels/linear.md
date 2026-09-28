# Linear channel

The Linear transport turns new issue comments into HAOSbot turns and posts the
assistant response back to the same issue through Linear's GraphQL API.

## Configuration

```json
{
  "channels": {
    "linear": {
      "enabled": true,
      "apiKey": "lin_api_...",
      "webhookSigningSecret": "...",
      "listenAddr": "0.0.0.0:3979",
      "webhookPath": "/linear/webhook",
      "allowFrom": ["LINEAR_USER_ID"]
    }
  }
}
```

Create a Linear webhook for Comment events and point it at the externally
reachable HTTPS URL that proxies to `listenAddr + webhookPath`.

The runtime verifies `Linear-Signature` against the exact raw request body,
rejects webhooks older/newer than five minutes, deduplicates
`Linear-Delivery`, ignores comments created by the API actor itself, and only
publishes comments from `allowFrom` (or `["*"]`).

This first Go implementation uses a personal API key and normal Comment
webhooks. It intentionally does not yet implement Linear's richer OAuth Agent
Session protocol; the transport contract is isolated so that OAuth/session
state can be layered on without changing the ChannelManager.
