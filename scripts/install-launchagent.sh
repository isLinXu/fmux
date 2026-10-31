#!/bin/bash
# Install fmux as a per-user LaunchAgent so it starts at login.
#
# Note: launchd watches ~/Library/LaunchAgents and picks up new plists on its
# own. `launchctl bootstrap` often reports "Input/output error" for a job that
# has already been loaded — that error is not a failure. Check with `launchctl
# print` instead of trusting the exit code.
set -euo pipefail

BIN="${FMUX_BIN:-$HOME/.local/bin/fmux}"
CONFIG="${FMUX_CONFIG:-$HOME/.fmux/config.yaml}"
LABEL="com.fmux.gateway"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME/Library/Logs/fmux.log"

if [[ ! -x "$BIN" ]]; then
  echo "error: $BIN not found. Run 'make install' first, or set FMUX_BIN." >&2
  exit 1
fi

mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs" "$(dirname "$CONFIG")"

if [[ ! -f "$CONFIG" ]]; then
  echo "note: no config at $CONFIG; the gateway will start with defaults"
fi

cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$BIN</string>
		<string>serve</string>
		<string>--config</string>
		<string>$CONFIG</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>$LOG</string>
	<key>StandardErrorPath</key>
	<string>$LOG</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
	</dict>
</dict>
</plist>
PLIST_EOF

plutil -lint "$PLIST" >/dev/null

echo "wrote $PLIST"
echo
echo "launchd picks this up automatically at next login."
echo "to start it now:"
echo "  launchctl bootstrap gui/$(id -u) $PLIST   # EIO here usually means it is already loaded"
echo "  launchctl print gui/$(id -u)/$LABEL | grep -E 'state|pid'"
echo
echo "logs: $LOG"
echo "remove with: launchctl bootout gui/$(id -u)/$LABEL && rm $PLIST"
