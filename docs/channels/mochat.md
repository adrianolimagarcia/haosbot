# MoChat channel

The Go transport uses MoChat's HTTP fallback APIs directly, avoiding a
Socket.IO runtime dependency.

Configure `clawToken`, optionally explicit `sessions` / `panels`, and
`allowFrom`. A value of `"*"` in the sessions or panels list enables target
discovery. Sessions use `/api/claw/sessions/watch`; panels use bounded polling
of `/api/claw/groups/panels/messages`. Replies use the corresponding
`/send` endpoints with `X-Claw-Token`.

The first response for each newly discovered target is treated as a cold
snapshot and not dispatched to the agent. Subsequent messages are deduplicated
with bounded per-target caches. Session cursors are held in memory for the
runtime lifetime.

This transport deliberately favors the documented polling fallback over
Socket.IO: fewer dependencies and simpler recovery at the cost of slightly
higher idle request traffic.
