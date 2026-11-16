package serverkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnnounceInterval is how often Announce repeats its announcement.
const AnnounceInterval = 60 * time.Second

// AnnounceOnce posts {"port","scheme"} to the local agent's
// /v1/servers/announce. The agent then fetches the descriptor from
// 127.0.0.1:<port> and runs the challenge itself, so the server must listen
// on loopback (or on all interfaces).
func (k *Kit) AnnounceOnce(ctx context.Context) error {
	if k.cfg.Port == 0 {
		return errors.New("serverkit: Config.Port is required to announce")
	}
	body, _ := json.Marshal(map[string]any{"port": k.cfg.Port, "scheme": k.cfg.Scheme})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(k.cfg.AgentURL, "/")+"/v1/servers/announce", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 != 2 {
		return errors.New("serverkit: agent answered " + res.Status)
	}
	return nil
}

// Announce announces the server to the local agent now and every
// AnnounceInterval until ctx ends. A missing agent or a refused announcement
// is not an error: the next round tries again. It returns ctx.Err(), or an
// error at once when Config.Port is unset.
func (k *Kit) Announce(ctx context.Context) error {
	if k.cfg.Port == 0 {
		return errors.New("serverkit: Config.Port is required to announce")
	}
	t := time.NewTicker(AnnounceInterval)
	defer t.Stop()
	for {
		_ = k.AnnounceOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
