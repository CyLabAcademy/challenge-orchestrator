package main

import (
	"fmt"
	"os"
)

// BasePin mirrors cmgr.BasePin. The CLI deliberately imports no cmgr package,
// so this is a copy; cmgr/basepins.go is the source of truth.
type BasePin struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	InUse  int    `json:"in_use"`
}

type PinsResponse struct {
	Pins  []BasePin `json:"pins"`
	Error string    `json:"error,omitempty"`
}

func pinListCommand(c *client, args []string) int { return pinsCommand(c, "GET") }

func pinRefreshCommand(c *client, args []string) int { return pinsCommand(c, "POST") }

func pinsCommand(c *client, method string) int {
	var resp PinsResponse
	if err := c.doJSON(method, "/pins", nil, &resp); err != nil {
		return runtimeError(err)
	}
	if len(resp.Pins) == 0 {
		fmt.Println("no base image pins configured on the server")
	} else {
		for _, p := range resp.Pins {
			fmt.Printf("%-30s %-71s used by %d\n", p.Ref, p.Digest, p.InUse)
		}
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", resp.Error)
		return RUNTIME_ERROR
	}
	if method == "POST" {
		fmt.Println()
		fmt.Println("Pins refreshed. Nothing rebuilds on account of this, now or later:")
		fmt.Println("'update' compares the challenge directory, which has not changed.")
		fmt.Println("A moved base reaches a challenge the next time that challenge is")
		fmt.Println("rebuilt for its own reasons, so refresh before a batch update.")
	}
	return NO_ERROR
}
