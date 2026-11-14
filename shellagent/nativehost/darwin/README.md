# macOS app host

`main.swift` is the window process copied into every natively installed
DynApp bundle (`~/Applications/DynApp/<Name>.app`). The agent embeds the
committed universal binary `dynapp-app-host` and writes it into each bundle.

Run `./build.sh` after changing `main.swift` and commit the new binary. The
build is reproducible, so an unchanged source produces identical bytes.

Rebuilding is not free. macOS ties camera, microphone, and Local Network
grants of the ad-hoc signed bundles to the binary's bytes, so a new binary
makes installed apps ask for those again once the agent rewrites them. Keep
behavior that may change often in the agent: the page bridge script
(`../../native_host_bridge.js`) and every capability are served by the agent at
runtime and need no host rebuild.

CI compiles the host on the macOS runner and uploads `native-macos-host` for
source validation. For an intentional host change made from another platform,
commit that artifact as `dynapp-app-host` before cutting the agent release.
Release builds continue to embed the committed bytes; a routine CI build does
not replace the binary in existing installed apps.
