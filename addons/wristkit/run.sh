#!/bin/sh
# Start wristkit headless for gerdr (`wristkit gerdr`: no window, settings served on http://127.0.0.1:7858).
# Which binary: $WRISTKIT_BIN, else `wristkit` on PATH (`make install-daemon` in the wristkit repo puts it in
# ~/.local/bin), else the one inside wristkit.app — the App's binary runs headless too, without opening a window.
for b in "$WRISTKIT_BIN" "$(command -v wristkit)" "$HOME/.local/bin/wristkit" \
  /Applications/wristkit.app/Contents/MacOS/wristkit "$HOME/Applications/wristkit.app/Contents/MacOS/wristkit"; do
  if [ -n "$b" ] && [ -x "$b" ]; then exec "$b" gerdr "$@"; fi
done
echo '{"type":"status","text":"wristkit not found: install wristkit.app, or run make install-daemon in the wristkit repo"}'
echo "wristkit not found" >&2
# Don't exit at once: gerdr would restart this every second until it backs off; stay until gerdr stops it
exec cat >/dev/null
