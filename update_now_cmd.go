package main

import (
	"context"
	"fmt"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent"
)

// updateRestartWait is how long --update waits for the new version once the
// agent is no longer held up by active bridges.
const updateRestartWait = 3 * time.Minute

// runUpdateNow is `dynapp-shell-agent --update`: it asks the running agent to
// install the latest release and waits until the new version answers. The
// agent waits for open bridges (remote sessions, port forwards) to close
// first; force closes them instead.
func runUpdateNow(address string, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	result, err := shellagent.RequestUpdate(ctx, address, force)
	cancel()
	if err != nil {
		return err
	}
	if !result.Updating {
		fmt.Printf("DynApp agent %s is up to date.\n", result.Current)
		return nil
	}
	fmt.Printf("Updating DynApp agent %s -> %s...\n", result.Current, result.Latest)
	if force && result.ActiveBridges > 0 {
		fmt.Printf("Closing %s now.\n", bridgeCount(result.ActiveBridges))
	}
	deadline := time.Now().Add(updateRestartWait)
	lastBridges := -1
	hintedOldAgent := false
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		running := shellagent.RunningAgentVersion(ctx, address)
		cancel()
		if running == result.Latest {
			fmt.Printf("DynApp agent %s is running.\n", running)
			return nil
		}
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		status, ok := shellagent.FetchUpdateStatus(ctx, address)
		cancel()
		switch {
		case ok && status.WaitingForBridges:
			if status.ActiveBridges != lastBridges {
				lastBridges = status.ActiveBridges
				fmt.Printf("Waiting for %s to close; the update applies anyway at %s.\n",
					bridgeCount(status.ActiveBridges), status.BridgeWaitUntil.Local().Format("15:04"))
				fmt.Println("Run `dynapp-shell-agent --update --force` to close them now, or press Ctrl+C to stop watching (the update stays scheduled).")
			}
			// Waiting on bridges is expected; do not count it against the
			// restart deadline.
			deadline = time.Now().Add(updateRestartWait)
		case ok && !status.Updating && status.Error != "":
			return fmt.Errorf("the update failed: %s", status.Error)
		case ok:
			if lastBridges > 0 {
				fmt.Println("Bridges closed; installing the update.")
				lastBridges = 0
			}
		case running != "" && !hintedOldAgent && time.Until(deadline) < updateRestartWait-10*time.Second:
			// An agent older than the status endpoint cannot say why it is
			// waiting; the usual reason is an open bridge.
			hintedOldAgent = true
			fmt.Printf("Agent %s is still running. Agents older than 0.1.79 wait up to 2 hours for open bridges (remote sessions, port forwards) to close before updating, and cannot report it.\n", running)
		}
	}
	return fmt.Errorf("the agent did not come back as %s within %s; check its update.log", result.Latest, updateRestartWait)
}

func bridgeCount(count int) string {
	if count == 1 {
		return "1 active bridge"
	}
	return fmt.Sprintf("%d active bridges", count)
}
