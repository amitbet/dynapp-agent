# Contributing

## Development loop

```sh
go test ./...
go vet ./...
go build -o dynapp-shell-agent .
```

On Windows, build with CGo and a C++ toolchain to include the native OLE
clipboard provider. `CGO_ENABLED=0` keeps the older eager file clipboard
fallback. macOS tray, hotkeys, and ScreenCaptureKit need a CGO build.

Native tray/hotkey smoke test in a signed-in desktop session:

```sh
go build -o /tmp/dynapp-shell-agent .
node ./scripts/smoke-native-presentation.mjs /tmp/dynapp-shell-agent
```

Every push runs the Go tests. A successful push to `main` also patch-bumps
the latest `v*` tag and publishes signed binaries for macOS, Windows, and
Linux when the signing secrets below are set. Without them, CI stays green
and skips the publish. You can still run **Release** from the Actions tab,
or push a `v*.*.*` tag. Each release also uploads `.deb` packages, publishes
an apt repo to GitHub Pages, and updates `Formula/dynapp-shell-agent.rb`.

Release signing uses these repository secrets:

- `RELEASE_SIGNING_KEY` — Ed25519 private key; every asset gets a `.sig`
- `MACOS_CODESIGN_P12_BASE64` / `MACOS_CODESIGN_P12_PASSWORD` — persistent
  `DynApp Local Signing` identity for macOS binaries

Do not commit tokens, keys, or personal paths.

## License

The repository owner has not yet chosen a license. Until a `LICENSE` file
exists, contributions are accepted on the understanding that they will be
licensed under whatever open-source license the owner selects.
