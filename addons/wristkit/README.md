# wristkit

Runs [wristkit](https://github.com/zbysir/wristkit) in the background, so your Xiaomi band shows the state of your
agents (Agent 猫) and buzzes when one finishes or gets stuck — without the wristkit window or its menu bar icon.

- Settings → Add-ons shows whether the band is connected and its battery; its settings have **Control panel → Open**,
  which opens wristkit's own settings page (http://127.0.0.1:7858) in the browser.
- Gerdr keeps the process running and restarts it if it dies; switching the add-on off lets go of the band.

Needs wristkit: either `wristkit.app` in Applications (its binary runs headless, no window opens), or
`make install-daemon` in the wristkit repo (puts `wristkit` in `~/.local/bin`). The first run sets up the band through
the connection wizard in the settings page. The band takes one connection at a time: don't open the wristkit App while
this runs.
