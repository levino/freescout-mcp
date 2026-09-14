# freescout-mcp

Claude talks to the FreeScout helpdesk at `shared-inbox.levinkeller.de`, and
people log into it with their ZITADEL account instead of a FreeScout password.

Three pieces, one repository:

| Piece | What it is |
| --- | --- |
| `server/` | An MCP server (Go, standard library only). The only thing exposed to the internet besides FreeScout itself. |
| `modules/Mcp/` | A FreeScout module: the bridge the MCP server calls, reachable over the pod's loopback interface and nowhere else. |
| `modules/Zitadel/` | A FreeScout module: sends the web login to ZITADEL (`id.levinkeller.de`). |

There is no REST API and there are no API keys. The public surface is the MCP
endpoint with OAuth, and the login page.

## How it fits together

```
Claude ──MCP over OAuth──▶ mcp-server ──loopback──▶ FreeScout (Modules/Mcp)
                              │                         │
                              └──login──▶ ZITADEL ◀─────┘ (Modules/Zitadel)
```

The MCP server runs as a sidecar in the FreeScout pod. It reaches the bridge at
`http://127.0.0.1`, which never leaves the pod, and the bridge refuses anything
that does not arrive on loopback with the shared token. Writes are not made in
SQL: a reply goes through `Conversation::createUserThread`, so mail delivery,
threading, folder counters and the `UserReplied` event happen exactly as they
do when a person clicks Reply.

Who may use it is decided in FreeScout, not here. After the ZITADEL login the
server looks the email address up among the active FreeScout users; if there is
no account, there is no token. Nobody is created automatically.

## Tools

`list_mailboxes`, `search_conversations`, `get_conversation`,
`reply_to_conversation`, `add_note`, `set_status`, `assign_conversation`.

`reply_to_conversation` really sends an email. `add_note` never does.

## Configuration

### MCP server (`server/`)

| Variable | Required | Meaning |
| --- | --- | --- |
| `BASE_URL` | yes | Public origin, e.g. `https://shared-inbox.levinkeller.de`. Every discovery document is derived from it. |
| `LISTEN` | no | Listen address, default `:8080`. |
| `BRIDGE_URL` | no | Where FreeScout answers inside the pod, default `http://127.0.0.1`. |
| `BRIDGE_TOKEN` | yes | Shared secret for the bridge; must equal FreeScout's `MCP_BRIDGE_TOKEN`. |
| `OIDC_ISSUER` | yes | `https://id.levinkeller.de`. |
| `OIDC_CLIENT_ID` | yes | ZITADEL application. |
| `OIDC_CLIENT_SECRET` | no | Set for a confidential client. |
| `OIDC_SCOPES` | no | Default `openid profile email`. |
| `TOKEN_SIGNING_KEY` | yes | At least 32 bytes. Tokens are HMAC-signed, so a restart keeps connections alive and no database is needed. |
| `APP_NAME` | no | Shown on the consent page. |
| `INSECURE_ALLOW_LOCAL_CLIENTS` | no | Tests only. Allows client metadata documents on loopback addresses. |

### FreeScout modules

Set these in FreeScout's `.env`. With the `nfrastack/freescout` image, prefix
them with `FREESCOUT_` in the container environment and the image writes them
into the `.env` without the prefix.

| Variable | Module | Meaning |
| --- | --- | --- |
| `MCP_BRIDGE_TOKEN` | Mcp | Shared secret, same value as the server's `BRIDGE_TOKEN`. |
| `ZITADEL_ISSUER` | Zitadel | `https://id.levinkeller.de`. Unset means the module does nothing. |
| `ZITADEL_CLIENT_ID` | Zitadel | ZITADEL application for the web login. |
| `ZITADEL_CLIENT_SECRET` | Zitadel | Optional, for a confidential client. |
| `ZITADEL_SCOPES` | Zitadel | Default `openid profile email`. |
| `ZITADEL_FORCE_LOGIN` | Zitadel | Default true: `/login` redirects to ZITADEL. `/login?local=1` always shows the built-in form. |

### The ZITADEL application

One application with two redirect URIs:

- `https://shared-inbox.levinkeller.de/oauth/callback` — the MCP server
- `https://shared-inbox.levinkeller.de/zitadel/callback` — the web login

It needs the `email` scope and the address in the token has to match the
FreeScout user's address.

## Deployment

`main` publishes the built artifacts to the orphan branch `production-dist`
(no image, no registry — the cluster may only use the GitHub App). The pod's
init container clones that branch, copies `modules/Mcp` and `modules/Zitadel`
into `/data/Modules/` and the `mcp-server` binary into the shared volume the
sidecar runs it from. The commit is pinned in `levino/server-config`,
`apps/freescout/workloads.yaml`.

## Tests

```bash
go test ./...              # the server
php modules/tests/run.php  # the modules, without Laravel
ci/e2e.sh                  # the whole stack, see below
```

`ci/e2e.sh` starts a real FreeScout with a real MariaDB, mounts both modules
the way the cluster mounts them, runs the MCP server *inside* the FreeScout
container so the bridge is reached over loopback as in production, and then
drives the whole path: OAuth with PKCE, every tool, the ZITADEL login — and
checks in the database that the replies, notes, status changes and assignments
really landed. `ci/e2e.sh up` leaves the stack running, `ci/e2e.sh down`
removes it.

All three run in CI on every pull request.

## License

MIT, see `LICENSE` and the notes in `NOTICE`.
