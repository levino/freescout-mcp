# freescout-mcp — developer notes

Claude reaches the FreeScout helpdesk through an MCP server, and people log in
with their ZITADEL account. See `README.md` for the user-facing overview and
every configuration variable.

## Architecture

- **MCP server** (`server/`, Go, standard library only): the only public
  surface besides FreeScout itself. `mcp.go` is the JSON-RPC endpoint, tool
  definitions live in `toolDefs()` and dispatch in `dispatchTool` (`tools.go`).
  Runs as a sidecar in the FreeScout pod.
- **Auth** (`server/oauth.go`, `oauth_routes.go`, `cimd.go`, `oidc.go`): its own
  OAuth 2.1 authorization server for MCP clients — Client ID Metadata Documents
  only (no dynamic registration, public clients with PKCE S256), login
  delegated to ZITADEL. Ported from `levino/surveys`, which is the version
  already proven against Claude.
- **Bridge** (`modules/Mcp/`): a FreeScout module. The server calls it over the
  pod's loopback interface; it is not a REST API and must never become one.
- **Web login** (`modules/Zitadel/`): a FreeScout module that sends `/login` to
  ZITADEL.
- **Deployment**: `main` publishes binary and modules to the orphan branch
  `production-dist`; the pod's init container clones it. The commit is pinned
  in `levino/server-config`, `apps/freescout/workloads.yaml`.

## Invariants worth keeping

- **The bridge stays pod-local.** `PodLocalOnly` has two locks: no forwarding
  headers (`X-Forwarded-For` and friends) and `REMOTE_ADDR` on loopback, plus
  the shared token. The header check is not redundant: the image's nginx
  rewrites `REMOTE_ADDR` from `X-Forwarded-For` for its own network, so the
  address alone can be made to lie.
- **Writes go through FreeScout.** A reply is `Conversation::createUserThread`,
  which fires `UserReplied` and with it the mail delivery, threading and folder
  counters. Never write threads in SQL.
- **No auto-provisioning.** Both the MCP login and the web login refuse an
  address that has no active FreeScout user. Who gets into the helpdesk stays a
  decision made in the helpdesk.
- **Two discovery flags** in `mountOauth` are what make Claude pick CIMD
  without asking anyone to register anything:
  `client_id_metadata_document_supported` and `"none"` among
  `token_endpoint_auth_methods_supported`. Keep them, along with the
  path-suffixed protected-resource document and the `WWW-Authenticate`
  challenge in `mcp.go`.
- **Loopback redirect URIs are matched without the port** (RFC 8252 §7.3);
  everything else is an exact match. Claude Code declares
  `http://localhost/callback` and relies on exactly this.
- **Tokens are HMAC-signed, nothing is stored.** Pending authorizations and
  one-time codes live in memory for minutes. A restart keeps connections alive
  and the server needs no volume. `TOKEN_SIGNING_KEY` is therefore the one
  secret that must not change casually — rotating it disconnects every client.
- **Read settings through `Env::get()`, never `env()`.** FreeScout ships a
  cached config (`bootstrap/cache/config.php`), and Laravel skips loading the
  `.env` file entirely when that exists, so `env()` returns null in production.
  `Env` falls back to parsing the file.
- **No PHP that is deprecated in 8.5.** FreeScout turns deprecations into
  exceptions, so something merely frowned upon (`curl_close()`, an implicitly
  nullable parameter) takes a whole route down. `TestNeitherModuleLoggedAnError`
  in the e2e suite guards this.
- **Modules need a row in `modules`.** FreeScout only boots a module that is
  active in the database; dropping files into `Modules/` is not enough.
  `deploy/activate-modules.sh` does what the "Activate" click does and runs
  from the image's start hook on every boot.

## Build / test

```bash
go test ./...                 # server, no network, no docker
php modules/tests/run.php     # modules, plain PHP, no PHPUnit
ci/e2e.sh                     # the whole stack; up / test / logs / down
```

`ci/e2e.sh` starts FreeScout and MariaDB, mounts both modules, runs the server
*inside* the FreeScout container (loopback, as in the pod), and checks every
write in the database afterwards. All three run in CI on every pull request.

When the e2e stack misbehaves: `ci/e2e.sh logs` prints the container log, the
MCP server's log and FreeScout's `laravel.log`.

## Conventions

- Configuration is environment-only, on both sides. No config files.
- German for commit messages and pull requests, English for code, comments and
  documentation.
- The server has no dependencies beyond the standard library. Keep it that way;
  it is what makes the sidecar a single static binary.
- New tools go into `toolDefs()` and `dispatchTool` together, and the bridge
  route they need goes into `modules/Mcp/Routes/bridge.php` — never a direct
  database query from the server.
