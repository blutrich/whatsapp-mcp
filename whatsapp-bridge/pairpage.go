package main

// Optional pairing page: with WHATSAPP_QR_PAGE=1 the QR code is shown in the
// default browser on a small local page that refreshes itself, instead of
// being drawn in the terminal. Useful when the bridge is started by an agent
// or in the background. The page and image live in a private temporary
// folder that is removed when pairing ends.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"rsc.io/qr"
)

const pairingPageHTML = `<!doctype html>
<meta charset="utf-8">
<title>Link WhatsApp</title>
<body style="font-family:-apple-system,system-ui,sans-serif;text-align:center;background:#fff;color:#111;padding:24px">
<h2>Scan with WhatsApp</h2>
<p>On your phone: WhatsApp &gt; Settings &gt; Linked devices &gt; Link a device</p>
<img id="qr" src="qr.png" width="420" height="420" style="image-rendering:pixelated" alt="Pairing QR code">
<p id="status">The code refreshes by itself. Keep this window open until you have scanned it.</p>
<script>
var misses = 0;
setInterval(function () {
  var next = new Image();
  next.onload = function () { misses = 0; document.getElementById('qr').src = next.src; };
  next.onerror = function () {
    if (++misses < 3) return;
    document.getElementById('qr').style.display = 'none';
    document.getElementById('status').textContent =
      'Pairing has ended. If you scanned the code, look for the "WhatsApp linked" notification. You can close this window.';
  };
  next.src = 'qr.png?t=' + Date.now();
}, 1500);
</script>
`

type pairingPage struct {
	dir    string
	opened bool
	open   func(path string) // replaceable in tests
}

func newPairingPage() (*pairingPage, error) {
	dir, err := os.MkdirTemp("", "whatsapp-pairing-")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(pairingPageHTML), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &pairingPage{dir: dir, open: openInBrowser}, nil
}

// show replaces the QR image with a new code and opens the page the first time.
func (p *pairingPage) show(code string) error {
	encoded, err := qr.Encode(code, qr.L)
	if err != nil {
		return err
	}
	encoded.Scale = 8
	tmp := filepath.Join(p.dir, "qr.png.tmp")
	if err := os.WriteFile(tmp, encoded.PNG(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(p.dir, "qr.png")); err != nil {
		return err
	}
	if !p.opened {
		p.opened = true
		page := filepath.Join(p.dir, "index.html")
		fmt.Println("PAIRING_PAGE_READY " + page)
		p.open(page)
	}
	return nil
}

// close removes the page and the QR image.
func (p *pairingPage) close() {
	if p != nil && p.dir != "" {
		os.RemoveAll(p.dir)
	}
}

// openInBrowser opens a local file with the system's default handler.
func openInBrowser(path string) {
	var program string
	switch runtimeGOOS {
	case "darwin":
		program = "/usr/bin/open"
	case "linux":
		found, err := exec.LookPath("xdg-open")
		if err != nil {
			return
		}
		program = found
	default:
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, program, path).Run()
	}()
}
