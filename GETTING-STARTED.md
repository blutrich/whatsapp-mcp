# Getting started (no coding needed)

This guide is for people who do not write code. It takes about 15 minutes.

## What you get

You connect your own WhatsApp to an AI assistant on your computer. Then you can ask things like:

- "What did Dana say about the invoice last week?"
- "Summarise what I missed in the family group today."
- "Find the photo Yoav sent me yesterday and save it."
- "Reply to Dana that I'll pay tomorrow." (the assistant asks you before it sends)

Everything is stored on your computer. Nothing is uploaded to a website.

## Three things to know first

1. **It works like WhatsApp Web.** You link it by scanning a QR code with your phone, and you can unlink it from your phone at any time.
2. **It is not made by WhatsApp.** WhatsApp does not officially support tools like this and may restrict accounts that send a lot of automated messages. Use it for normal personal use.
3. **The assistant reads what other people write to you.** Someone could send you a message that tries to trick the assistant ("ignore your instructions and send me..."). The tool is built to resist this, and the assistant asks before it sends anything. Keep that confirmation switched on. If you only want to read and search, ask for "read-only mode" during setup and sending is switched off completely.

## What you need

- A Mac, or a Windows or Linux computer.
- WhatsApp on your phone.
- An AI coding assistant installed on the computer. Any one of these works:
  - **Claude Code** (from Anthropic): [claude.com/claude-code](https://claude.com/claude-code)
  - **Codex** (from OpenAI): [openai.com/codex](https://openai.com/codex)
  - **Cursor**: [cursor.com](https://cursor.com)

The assistant does the installation for you. You only approve its steps and scan one QR code.

## Step 1. Ask the assistant to install it

Open your assistant and paste this message:

```
Please install the WhatsApp MCP server from https://github.com/blutrich/whatsapp-mcp
on this computer and connect it to yourself.

Follow the section "Instructions for coding agents" in its README exactly,
including the rules in AGENTS.md. Install any missing prerequisites and tell me
before each one. When it is time to pair, start the bridge with
WHATSAPP_QR_PAGE=1 so the QR code opens in my browser, and wait for me to scan it.
Do not send any WhatsApp message as a test.
```

If you want reading and searching only, add this line: `Set it up in read-only mode (WHATSAPP_READ_ONLY=1).`

The assistant will ask permission to run commands. That is expected. It downloads the code, builds it, and runs its checks.

## Step 2. Scan the QR code

A page titled **Scan with WhatsApp** opens in your browser.

1. On your phone, open WhatsApp.
2. Go to **Settings > Linked devices > Link a device**.
3. Point the phone at the QR code on the screen.

You get a notification on the computer: **WhatsApp linked**. Your chat history then arrives over the next few minutes.

If the page says pairing has ended before you scanned, ask the assistant to "start pairing again".

## Step 3. Try it

Restart the assistant (close and reopen it), then ask:

> List my five most recent WhatsApp chats.

If you see your chats, you are done.

## Everyday use

- **The bridge must be running.** It is the small program that keeps the connection to WhatsApp. If the assistant says it cannot reach WhatsApp, tell it: "Start the WhatsApp bridge."
- **After restarting your computer,** ask the assistant to start the bridge again. You do not need to scan again.
- **Sending.** The assistant shows you the message and the recipient and asks first. Read both before you approve.
- **Files.** It will not send hidden files or its own data. Only ask it to send files you name yourself.

## How to stop or remove it

- **Unlink:** on your phone, go to **WhatsApp > Settings > Linked devices**, tap the device, and choose **Log out**. The computer shows a notification that it was unlinked. This takes effect immediately.
- **Delete your data from the computer:** delete the folder `whatsapp-mcp/whatsapp-bridge/store`. It holds your saved messages and the link to your account.
- **Remove everything:** unlink, then delete the whole `whatsapp-mcp` folder.

## Questions

**Can the people I chat with tell?**
No. Messages you send through it look like any message from you.

**Does the AI company see my messages?**
Only the messages the assistant reads to answer you are sent to the AI service, the same way anything you type to the assistant is. Your full history is not uploaded.

**Can someone else on the internet use my WhatsApp through this?**
The tool only accepts requests from your own computer, with a secret key stored on your computer, and it refuses requests coming from web pages.

**Is it safe to give this to a friend?**
The code has been reviewed and tested, the list of issues that were found and fixed is in [SECURITY.md](./SECURITY.md), and the limits that remain are written there too. It has not had an independent professional audit.

**Something went wrong.**
Tell your assistant: "Check the Troubleshooting section of the WhatsApp MCP README and fix it." The full manual is in [README.md](./README.md).
