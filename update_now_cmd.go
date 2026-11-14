package main

import (
	"context"
	"fmt"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent"
)

// runUpdateNow is `dynapp-shell-agent --update`: it asks the running agent to
// install the latest release and waits until the new version answers.
func runUpdateNow(address string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	result, err := shellagent.RequestUpdate(ctx, address)
	cancel()
	if err != nil {
		return err
	}
	if !result.Updating {
		fmt.Printf("DynApp agent %s is up to date.\n", result.Current)
		return nil
	}
	fmt.Printf("Updating DynApp agent %s -> %s...\n", result.Current, result.Latest)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		running := shellagent.RunningAgentVersion(ctx, address)
		cancel()
		if running == result.Latest {
			fmt.Printf("DynApp agent %s is running.\n", running)
			return nil
		}
	}
	return fmt.Errorf("the agent did not come back as %s within 3 minutes; check its update.log", result.Latest)
}
