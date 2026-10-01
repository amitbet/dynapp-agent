package main

// appHostOptions mirrors winhost.Options; the Windows Start menu shortcut
// passes them to `dynapp-shell-agent app`.
type appHostOptions struct {
	StoreID string
	Pipe    string
	URL     string
	Origin  string
	Name    string
	Icon    string
}
