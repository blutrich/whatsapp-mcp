#!/bin/sh
# Start the WhatsApp bridge automatically when you log in.
#
#   scripts/autostart.sh install     build the bridge if needed and register it
#   scripts/autostart.sh uninstall   remove the registration (data is kept)
#   scripts/autostart.sh status      show whether it is registered and running
#
# macOS: a per-user LaunchAgent. Linux: a systemd user service.
# Nothing is installed system-wide and nothing runs as root.
# Settings such as WHATSAPP_CONFIRM_SENDS go in whatsapp-bridge/bridge.env,
# one NAME=value per line; they are read at install time.

set -eu

LABEL="com.whatsapp-mcp.bridge"
REPO="$(cd "$(dirname "$0")/.." && pwd -P)"
BRIDGE_DIR="$REPO/whatsapp-bridge"
BINARY="$BRIDGE_DIR/whatsapp-bridge"
LOG="$BRIDGE_DIR/whatsapp.log"
ENV_FILE="$BRIDGE_DIR/bridge.env"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
UNIT="$HOME/.config/systemd/user/whatsapp-bridge.service"

die() { echo "error: $*" >&2; exit 1; }

case "$(id -u)" in 0) die "run this as your own user, not as root";; esac

# Only NAME=value lines with WHATSAPP_ names and plain values are accepted,
# so the file cannot smuggle anything else into the service definition.
read_settings() {
    [ -f "$ENV_FILE" ] || return 0
    grep -E '^WHATSAPP_[A-Z_]+=[A-Za-z0-9_@.,:/+ -]*$' "$ENV_FILE" || true
}

build_if_needed() {
    if [ ! -x "$BINARY" ]; then
        command -v go >/dev/null 2>&1 || die "Go is not installed, and $BINARY does not exist"
        echo "Building the bridge..."
        (cd "$BRIDGE_DIR" && go build -o whatsapp-bridge .) || die "build failed (see the README troubleshooting table)"
    fi
}

xml_escape() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

install_macos() {
    build_if_needed
    mkdir -p "$HOME/Library/LaunchAgents"
    {
        printf '%s\n' '<?xml version="1.0" encoding="UTF-8"?>'
        printf '%s\n' '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
        printf '%s\n' '<plist version="1.0"><dict>'
        printf '  <key>Label</key><string>%s</string>\n' "$LABEL"
        printf '  <key>ProgramArguments</key><array><string>%s</string></array>\n' "$(xml_escape "$BINARY")"
        # No WorkingDirectory or log paths here: macOS refuses to start a
        # service whose paths are in a protected folder (Desktop, Documents,
        # Downloads). The bridge changes folder and opens its log itself.
        printf '%s\n' '  <key>RunAtLoad</key><true/>'
        # Not restarted automatically: when the phone unlinks the device the
        # bridge exits on purpose, and it should not loop on a QR code.
        printf '%s\n' '  <key>KeepAlive</key><false/>'
        printf '%s\n' '  <key>ProcessType</key><string>Background</string>'
        printf '%s\n' '  <key>Umask</key><integer>63</integer>'
        printf '%s\n' '  <key>EnvironmentVariables</key><dict>'
        # If the device was unlinked, show the QR code in the browser at login.
        printf '%s\n' '    <key>WHATSAPP_QR_PAGE</key><string>1</string>'
        printf '    <key>WHATSAPP_BRIDGE_DIR</key><string>%s</string>\n' "$(xml_escape "$BRIDGE_DIR")"
        printf '    <key>WHATSAPP_LOG_FILE</key><string>%s</string>\n' "$(xml_escape "$LOG")"
        read_settings | while IFS='=' read -r name value; do
            printf '    <key>%s</key><string>%s</string>\n' "$name" "$(xml_escape "$value")"
        done
        printf '%s\n' '  </dict>'
        printf '%s\n' '</dict></plist>'
    } > "$PLIST.tmp"
    plutil -lint "$PLIST.tmp" >/dev/null || die "generated plist is not valid"
    chmod 600 "$PLIST.tmp"
    mv "$PLIST.tmp" "$PLIST"
    touch "$LOG" && chmod 600 "$LOG"
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST" || die "launchctl could not load $PLIST"
    echo "Installed. The bridge starts now and at every login."
    echo "If macOS asks whether whatsapp-bridge may access a folder, allow it:"
    echo "the bridge keeps its data inside this repository."
}

uninstall_macos() {
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    rm -f "$PLIST"
    echo "Removed. Your messages and WhatsApp link in $BRIDGE_DIR/store are untouched."
}

status_macos() {
    if [ -f "$PLIST" ]; then echo "registered: yes ($PLIST)"; else echo "registered: no"; fi
    if launchctl print "gui/$(id -u)/$LABEL" 2>/dev/null | grep -q 'state = running'; then
        echo "running: yes"
    else
        echo "running: no"
    fi
}

install_linux() {
    command -v systemctl >/dev/null 2>&1 || die "systemd is not available; start the bridge yourself"
    build_if_needed
    mkdir -p "$(dirname "$UNIT")"
    {
        echo "[Unit]"
        echo "Description=WhatsApp bridge for the WhatsApp MCP server"
        echo "After=network-online.target"
        echo
        echo "[Service]"
        echo "WorkingDirectory=$BRIDGE_DIR"
        echo "ExecStart=$BINARY"
        echo "StandardOutput=append:$LOG"
        echo "StandardError=append:$LOG"
        echo "UMask=0077"
        echo "Restart=no"
        echo "Environment=WHATSAPP_QR_PAGE=1"
        read_settings | while IFS= read -r line; do echo "Environment=\"$line\""; done
        echo
        echo "[Install]"
        echo "WantedBy=default.target"
    } > "$UNIT"
    chmod 600 "$UNIT"
    systemctl --user daemon-reload
    systemctl --user enable --now whatsapp-bridge.service
    echo "Installed. The bridge starts now and at every login."
}

uninstall_linux() {
    systemctl --user disable --now whatsapp-bridge.service 2>/dev/null || true
    rm -f "$UNIT"
    systemctl --user daemon-reload 2>/dev/null || true
    echo "Removed. Your messages and WhatsApp link in $BRIDGE_DIR/store are untouched."
}

status_linux() {
    if [ -f "$UNIT" ]; then echo "registered: yes ($UNIT)"; else echo "registered: no"; fi
    if systemctl --user is-active --quiet whatsapp-bridge.service 2>/dev/null; then echo "running: yes"; else echo "running: no"; fi
}

case "$(uname -s)" in
    Darwin) os=macos ;;
    Linux)  os=linux ;;
    *)      die "automatic start is supported on macOS and Linux only" ;;
esac

case "${1:-}" in
    install)   "install_$os" ;;
    uninstall) "uninstall_$os" ;;
    status)    "status_$os" ;;
    *)         echo "usage: $0 install | uninstall | status" >&2; exit 2 ;;
esac
