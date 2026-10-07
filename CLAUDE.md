# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Two-part MCP (Model Context Protocol) server exposing file/git/docker/db/exec tools over stdio, fronted by an HTTP+OAuth bridge:

- `main.go` — the actual MCP server. Speaks JSON-RPC 2.0 over stdin/stdout (one JSON message per line). Implements `initialize`, `tools/list`, `tools/call`.
- `http-bridge.js` — Node HTTP server (no deps, uses only `http`/`crypto`/`url`/`child_process`) that adds a minimal OAuth 2.0 layer (dynamic client registration, authorize, token, refresh) in front of the MCP server, then spawns the compiled Go binary per-request and pipes the JSON-RPC request/response through its stdin/stdout.

## Build & run

```bash
# Build the Go MCP server
go build -o mcp-server main.go

# Run it directly (stdio JSON-RPC, for local/manual testing)
APP_DIR=/path/to/app DATABASE_PATH=/path/to/db.sqlite ./mcp-server
# then feed it newline-delimited JSON-RPC, e.g.:
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./mcp-server

# Run the HTTP/OAuth bridge (expects compiled binary at /opt/mcp/mcp-server, see below)
node http-bridge.js
```

There is no test suite, linter, or CI config in this repo.

## Configuration (env vars)

`http-bridge.js` reads: `MCP_ISSUER` (OAuth issuer URL, default `https://mcp.snaptape.in`), `APP_DIR` (app dir passed through to the spawned MCP server, default `/opt/app`), `MCP_BINARY_PATH` (compiled Go binary to exec, default `/opt/mcp/mcp-server`), `MCP_BRIDGE_HOST`/`MCP_BRIDGE_PORT` (listen address, default `localhost:8765`). `main.go` reads `APP_DIR` (default `/opt/app`) and `DATABASE_PATH` (default `/opt/app/data/pgease.db`). No values are hardcoded secrets — there are none in this repo; OAuth tokens are generated at runtime via `crypto.randomBytes` and kept in-memory only.

## Architecture notes

- **Transport split**: `http-bridge.js` does NOT reimplement the tools — it is purely a stdio↔HTTP adapter plus OAuth gate. Every `/mcp` request spawns a *fresh* `bash -c` subprocess that exports `APP_DIR`, marks that dir git-safe, and execs `MCP_BINARY_PATH`. Both are env-configurable (see above) — set them per deployment; keep whatever you build with `go build -o $MCP_BINARY_PATH main.go` in sync with the `MCP_BINARY_PATH` value the bridge uses.
- **One process per request**: the bridge spawns a brand-new `mcp-server` process per HTTP call (10s timeout, then killed) rather than keeping a long-lived MCP session — there is no persistent server state between calls on the Go side.
- **OAuth state is in-memory only**: `clients`/`codes`/`tokens`/`refreshTokens` are plain `Map`s in `http-bridge.js`. Restarting the bridge invalidates all issued tokens — acceptable per the code comment ("single-user personal use"). Authorization is auto-approved with no login screen/consent UI.
- **Path sandboxing**: `main.go`'s `isAllowedPath()` restricts `file_read`/`file_write` to paths prefixed by `appDir` (env `APP_DIR`, default `/opt/app`). Note it's a `strings.HasPrefix` check on the absolute path — not a hardened containment check (e.g. `/opt/app-evil` would pass). `exec` and the git/docker tool handlers do **not** go through `isAllowedPath` — they trust `appDir`/caller-supplied `dir` directly.
- **DB access is read-only by convention, not by permissions**: `dbQuery()` only string-checks that the query starts with `SELECT` (case-insensitive prefix match) before opening sqlite3 at `DATABASE_PATH` (default `/opt/app/data/pgease.db`). This is a soft guard, not a privilege restriction.
- **Tool registry pattern**: tools are declared twice and must be kept in sync — once in `handleToolsList()` (name + JSON schema) and once in the `switch` in `handleToolCall()` (dispatch to the `xxxParams`-typed handler). Adding a tool means updating both plus a new handler function following the existing `(args map[string]interface{}) (interface{}, string)` signature (result, errMsg).
- **Error convention**: handler functions return `(result, errMsg string)` — a non-empty `errMsg` causes `handleToolCall` to emit a JSON-RPC error response instead of a result; they never use Go's `error` type directly in this return path.
