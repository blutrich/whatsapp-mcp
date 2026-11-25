# Security

This file lists the security issues found in this code, their status, and how to report a new one. The README's [Security section](./README.md#security) explains the protections in plain terms and maps them to the OWASP Top 10 for LLM Applications.

## Reporting a problem

Use GitHub's private reporting: **Security > Report a vulnerability** on this repository. If that is not available, open an issue that says a security problem exists, without the details, and ask for a private channel. Please do not post session files, tokens or message content.

## Issues found and fixed

Severity is a judgement for a single-user computer running the bridge.

| # | Issue | Severity | Fixed in |
| --- | --- | --- | --- |
| 1 | **Local API had no authentication.** Any web page open in a browser, and any program on the computer, could send WhatsApp messages and files as the user by posting to `localhost:8080`. No preflight was triggered because the API accepted `text/plain` bodies. | Critical | `1ca9595` |
| 2 | **DNS rebinding.** The API did not check the `Host` header, so a hostile site could reach it through a rebinding domain. | High | `1ca9595` |
| 3 | **Any local file could be sent.** `media_path` was read without checks, including the bridge's own session database (account takeover as a linked device), SSH keys and `.env` files. Combined with issue 1 a web page could exfiltrate files; combined with prompt injection an assistant could. | Critical | `1ca9595` |
| 4 | **Path traversal on download.** A document's file name is chosen by its sender. A name such as `../../x` was joined into the save path, so downloading it could write outside the store folder. | High | `1ca9595` |
| 5 | **Session keys and message history readable by other user accounts.** The store folder was created `755` and its databases `644`. | High | `1ca9595` |
| 6 | **API reachable from the network.** Upstream bound the API to all interfaces. | High | `9d58234` |
| 7 | **Message text and contact names written to the log.** Every incoming and synced message was printed, and upload responses including media keys were printed. | Medium | `1ca9595` |
| 8 | **Wrong attachment returned.** Generated media names were based on the second they were processed; two attachments in the same second shared a file, and the second download returned the first file. | Medium | `1ca9595` |
| 9 | **Wrong person or chat returned to the assistant.** Sender names and direct chats matched on any part of a number; message context could come from another chat with the same message ID. An assistant could attribute a message to, or reply to, the wrong contact. | Medium | `1ca9595` |
| 10 | **Forged message lines (prompt-injection aid).** Message text was inserted into tool output unescaped, so a message could imitate the header line of another message, or carry terminal control characters. | Medium | `1ca9595` |
| 11 | **No limit on recipients or volume.** A manipulated assistant could message anyone, as often as it liked. | Medium | `1ca9595` (allowlist, send limit, read-only mode) |
| 12 | **No bounds on requests.** Unlimited request bodies, result sizes and media sizes; no timeouts on uploads, downloads or HTTP reads. | Low | `1ca9595` |
| 13 | **MCP server could create or modify the database.** It opened the SQLite file read-write and created an empty one when the bridge had never run. | Low | `1ca9595` |
| 14 | **Diagnostics printed to stdout,** the channel that carries the MCP protocol. | Low | `1ca9595` |
| 15 | **Dependencies with published advisories** (`starlette`, `urllib3`, `golang.org/x/net`, `golang.org/x/crypto`). | Medium | `1ca9595` |
| 16 | **Lockfile pointed at a private local package proxy,** so installs elsewhere failed and the package source was not the public index. | Medium | `4be78cb` |
| 17 | **Silent half-start.** When the API port was taken the bridge kept running without an API, and the MCP server would talk to whichever other process held the port. | Low | `1ca9595` |
| 18 | **Pairing QR images left on disk** after pairing ended or was interrupted. | Low | `1ca9595`, and this commit for interruption |

## Known limitations (not fixed, by nature)

| Limitation | What to do |
| --- | --- |
| **Prompt injection.** Anyone who can message you can put text in front of the assistant. Labelling and limits reduce the damage; nothing removes the risk. | Keep your client's approval prompt on for the send tools. Use `WHATSAPP_READ_ONLY=1` if you only read. Set `WHATSAPP_ALLOWED_RECIPIENTS`. |
| **Message content goes to the AI provider** whenever a tool returns it. That is how the assistant reads it. | Do not connect accounts whose content must not leave the computer. |
| **Programs running as your user** can read the store folder and the API token. | Keep the computer free of untrusted software. |
| **Databases are not encrypted at rest.** | Turn on full-disk encryption. |
| **Non-hidden files are sendable by default.** | Set `WHATSAPP_MEDIA_ROOTS` to one folder. |
| **Unofficial WhatsApp client.** WhatsApp may restrict accounts that use one. | Keep usage personal and low-volume. |
| **Downloaded attachments are untrusted files.** They are saved without execute permission, but opening them is your decision. | Treat them like any attachment. |
| **Go standard library advisories** depend on the Go version you build with. | Build with the latest Go patch release; `govulncheck` reports them. |

## How this was checked

- Unit tests and real-HTTP tests of the API guard, file policy, path handling, recipient rules and send limit (`whatsapp-bridge/*_test.go`), and tests of the MCP server against a made-up database and a fake bridge (`whatsapp-mcp-server/tests/`).
- `govulncheck` and `pip-audit` on every push (see `.github/workflows/ci.yml`).
- Against a live session: QR pairing, sending text and an image, downloading sent and received images, and confirming that unauthenticated, cross-site and rebinding-style requests are refused.

This was a code review and test pass by the maintainer with an AI assistant, not an independent professional audit.
