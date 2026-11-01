#!/bin/bash
# Install fmux as a per-user LaunchAgent so it starts at login.
#
# This is a thin wrapper: the real implementation lives in `fmux service`, so
# the plist layout and the launchctl handling only exist in one place. Prefer
# `fmux service install` directly; this script exists for `make launchagent`
# and for people who expect a shell entry point.
set -euo pipefail

BIN="${FMUX_BIN:-$HOME/.local/bin/fmux}"

if [[ ! -x "$BIN" ]]; then
  echo "error: $BIN not found. Run 'make install' first, or set FMUX_BIN." >&2
  exit 1
fi

exec "$BIN" service install --bin "$BIN" "$@"
