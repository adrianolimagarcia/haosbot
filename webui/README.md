# HAOSBOT Control Plane

The React/TypeScript interface is available at `/next/`. The existing WebUI at
`/` remains available while feature parity is built. Both use the same Go API,
session store and security middleware.

```sh
cd webui
npm ci
npm run build
```

Vite writes the versioned static bundle to `internal/api/webui/next/`. Commit
that generated directory when changing the frontend so `go:embed` can package
it into the single production Go binary. Node runs only at build time.

The current control plane covers streaming chat, session navigation, channels,
MCP stdio setup, providers, skills inventory, automation inventory, canonical
memory, and runtime metrics. Actions still handled by the original WebUI link
are called out in their respective screens.
