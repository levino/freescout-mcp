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
SQL but through FreeScout's models, the way its own controller does them: a
reply fires `UserReplied`, so mail delivery, threading and folder counters
happen exactly as they do when a person clicks Reply; moving and merging are
`Conversation::moveToMailbox` and `mergeConversations`.

Who may use it is decided in FreeScout, not here. After the ZITADEL login the
server looks the email address up among the active FreeScout users; if there is
no account, there is no token. Nobody is created automatically.

## Tools

| Tool | What it does |
| --- | --- |
| `list_mailboxes` | Mailboxes the user can see |
| `search_conversations` | FreeScout's own search (subject, number, customer, message text, senders, recipients), filters for status, mailbox, `after`/`before` (start date), `from`, `to`, `state` (`draft`) |
| `get_conversation`, `get_attachment` | Read a conversation with all messages, notes and drafts; download a file |
| `find_customers` | Customers by name, company or address, with their latest conversations |
| `reply_to_conversation` | **Sends** a reply, optionally with cc, bcc and attachments |
| `create_draft`, `create_new_draft`, `update_draft`, `delete_draft` | Drafts — never send |
| `send_draft` | **Sends** a draft the user created, only on explicit request after review |
| `forward_conversation` | Saves a forward draft (default) or, with `send`, **sends** the forward |
| `add_note`, `set_status`, `assign_conversation` | Internal note, status, assignee |
| `move_conversation`, `merge_conversations` | Move to another mailbox, merge into one conversation |
| `token` | A one-hour bearer token for the [REST API](#rest-api), acting as the connected user |
| `api` | Short guide to the REST API with curl examples |

Only `reply_to_conversation`, `send_draft` and `forward_conversation` with
`send: true` send email. The mailbox signature is never part of the text: FreeScout
adds it to every outgoing message from an agent, as it does for the web editor.

Not available, because FreeScout has no such feature without paid modules:
tags and snoozing.

### Drafts

`create_draft` (a reply in an existing conversation) and `create_new_draft`
(a new email, starting a new conversation) **never send anything**. They write
a draft exactly the way FreeScout's own editor does when it saves one
(`save_draft` in `ConversationsController`): a thread with
`Thread::STATE_DRAFT`, for a new email also a conversation with
`Conversation::STATE_DRAFT` and recipients on the thread, and the conversation
in the mailbox's Drafts folder. No `UserReplied` event, nothing in the mail
queue. A person opens the draft in FreeScout, checks it and clicks Send.

Attachments (`[{filename, upload_id | url | content_base64, mime_type?}]`, at most 20 files
and 20 MB together) go through `Attachment::create` like an upload from the
editor: the file lands in FreeScout's private storage
(`storage/app/attachment/…`), the row in `attachments`, bound to the draft
thread, so it shows in the editor and goes out with the mail. A `url` is
downloaded by the MCP server, https only, and never from loopback, private or
link-local addresses. An `upload_id` names a file uploaded through the REST API
(`POST /mcp/api/v1/uploads`) — the way to attach a local file without writing
it into the tool call as base64.

`update_draft` and `delete_draft` only touch drafts the connected user created.
`get_conversation` marks unsent drafts with `"draft": true`; their thread id is
the `draft_id`.

## REST API

Everything the tools do is also a REST API under `/mcp/api/v1` on the same
host, described as OpenAPI 3 at `/mcp/api/v1/openapi.json`. The MCP connection
stays the way in: the `token` tool hands out a bearer token for the API, valid
one hour, carrying the same user (and OAuth client and scope) as the MCP
session. MCP access tokens and API tokens are different kinds and neither opens
the other door. Every call acts as that FreeScout user through the same bridge,
with the same checks; the tools and the routes run the same Go functions
(`server/ops.go`).

| Route | Tool |
| --- | --- |
| `GET /mailboxes` | `list_mailboxes` |
| `GET /conversations?query=&…&fields=a,b` | `search_conversations` |
| `GET /conversations/{id}?fields=&last_threads=` | `get_conversation` |
| `POST /conversations/{id}/drafts` | `create_draft` |
| `POST /conversations/{id}/reply` | `reply_to_conversation` (**sends**) |
| `POST /conversations/{id}/forward` | `forward_conversation` (**sends** with `send=true`) |
| `POST /conversations/{id}/notes`, `…/status`, `…/assign`, `…/move`, `…/merge` | the tools of the same name |
| `POST /drafts`, `PATCH /drafts/{id}`, `DELETE /drafts/{id}` | `create_new_draft`, `update_draft`, `delete_draft` |
| `POST /drafts/{id}/send` | `send_draft` (**sends**) |
| `GET /customers?query=` | `find_customers` |
| `GET /attachments/{id}` | `get_attachment`, as the file itself |
| `POST /uploads`, `DELETE /uploads/{id}` | temporary upload for `upload_id` |
| `GET /me` | who the token acts as |

Write routes take the tool's fields as JSON, or as multipart form fields (all
of them as JSON in a field `payload`, or one field each; a repeated field is a
list). Every file part of a multipart request is an attachment. Errors are
`{"error": "…"}`: 400 for bad arguments, 401 for a missing or expired token,
FreeScout's own 403/404/422, 413 above 20 MB, 429 when the upload quota is
full, 502 when FreeScout does not answer.

```bash
# A new email as a draft, with a PDF — nothing is sent.
curl -H "Authorization: Bearer $TOKEN" \
  -F mailbox_id=1 -F to=kunde@example.org -F subject='Ihr Angebot' \
  -F body='Guten Tag, anbei das Angebot.' -F file=@angebot.pdf \
  https://shared-inbox.levinkeller.de/mcp/api/v1/drafts
```

Uploads (`POST /uploads`) wait in a private directory of the sidecar, owned by
the user who sent them, for one hour or until a draft, reply or forward uses
them; then FreeScout stores the file and the upload is deleted. At most 20
pending files and 100 MB per user. The directory is emptied on start.

### Why both, and what goes where

MCP is how Claude finds the helpdesk and logs in; every MCP client speaks it,
including claude.ai without a shell. But everything a tool returns lands in
the model's context, and everything it sends has to be written out by the
model token by token — a 700 KB PDF as base64 is a million tokens nobody can
afford. A client that can run commands does better with plain HTTP: curl
streams files from disk, writes downloads to disk, and a script can work
through a hundred conversations without the model reading each answer.

So the split is:

- **MCP**: logging in, `token` and `api`, and lean tools for working without a
  shell — search with a compact hit list (`fields` for more), reading a
  conversation (`last_threads` for long ones), drafts, replies, notes, status.
  Tool answers are compact JSON. Base64 and URLs for attachments remain for
  small generated files and for clients without a shell.
- **REST**: files (multipart upload, attachments as bytes) and anything in
  bulk. Same operations, same rights, same limits.

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
| `UPLOAD_DIR` | no | Where REST uploads wait, at most an hour. Emptied on start. Default `$TMPDIR/freescout-mcp-uploads`. |
| `INSECURE_ALLOW_LOCAL_CLIENTS` | no | Tests only. Allows client metadata documents and attachment URLs on loopback and private addresses, and over plain http. |

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
really landed, that drafts are stored as FreeScout drafts and never queue a
mail. `ci/e2e.sh up` leaves the stack running, `ci/e2e.sh down`
removes it.

All three run in CI on every pull request.

## License

MIT, see `LICENSE` and the notes in `NOTICE`.
