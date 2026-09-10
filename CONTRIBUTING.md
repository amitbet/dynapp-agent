# Contributing

## Development loop

```sh
go test ./...
go vet ./...
go build ./cmd/dynapp-shell-agent
```

On Windows, build with CGo and a C++ toolchain to include the native OLE
clipboard provider. `CGO_ENABLED=0` keeps the older eager file clipboard
fallback. macOS tray, hotkeys, and ScreenCaptureKit need a CGO build.

Native tray/hotkey smoke test in a signed-in desktop session:

```sh
go build -o /tmp/dynapp-shell-agent ./cmd/dynapp-shell-agent
node ./scripts/smoke-native-presentation.mjs /tmp/dynapp-shell-agent
```

Every push runs the Go tests. Trigger **Release** from the Actions tab to
stamp a version, build signed assets, and publish a GitHub release. Leave the
version blank to patch-bump the latest `v*` tag.

Release signing uses these repository secrets:

- `PAINTJS_PAYLOAD_SIGNING_KEY` — Ed25519 private key; every asset gets a `.sig`
- `MACOS_CODESIGN_P12_BASE64` / `MACOS_CODESIGN_P12_PASSWORD` — persistent
  `DynApp Local Signing` identity for macOS binaries

Do not commit tokens, keys, or personal paths.

## License

The repository owner has not yet chosen a license. Until a `LICENSE` file
exists, contributions are accepted on the understanding that they will be
licensed under whatever open-source license the owner selects.
