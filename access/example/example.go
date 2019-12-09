package main

import (
	"context"
	"fmt"
	"os"

	"github.com/gravitational/teleport-plugins/access"

	"github.com/gravitational/trace"
)

// eprintln prints an optionally formatted string to stderr.
func eprintln(msg string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, msg, a...)
	fmt.Fprintf(os.Stderr, "\n")
}

func main() {
	pgrm := os.Args[0]
	args := os.Args[1:]
	if len(args) < 1 {
		eprintln("USAGE: %s (configure | <config-path>)", pgrm)
		os.Exit(1)
	}
	if args[0] == "configure" {
		fmt.Print(exampleConfig)
		return
	}
	if err := run(args[0]); err != nil {
		eprintln("ERROR: %s", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	conf, err := LoadConfig(configPath)
	if err != nil {
		return trace.Wrap(err)
	}
	tc, err := conf.LoadTLSConfig()
	if err != nil {
		return trace.Wrap(err)
	}
	ctx := context.TODO()
	// Establish new client connection to the Teleport auth server.
	client, err := access.NewClient(ctx, conf.AuthServer, tc)
	if err != nil {
		return trace.Wrap(err)
	}
	// Register a watcher for pending access requests.
	watcher, err := client.WatchRequests(ctx, access.Filter{
		State: access.StatePending,
	})
	if err != nil {
		return trace.Wrap(err)
	}
	defer watcher.Close()
	for {
		select {
		case event := <-watcher.Events():
			req, op := event.Request, event.Type
			switch op {
			case access.OpInit:
				// OpInit is a sentinel value indicating that the watcher channel is fully
				// established.  `req` is empty in this case.
				eprintln("watcher initialized...\n")
			case access.OpPut:
				// OpPut indicates that a request has been created or updated.  Since we specified
				// StatePending in our filter, only pending requests should appear here.
				eprintln("Handling request: %+v", req)
				whitelisted := false
			CheckWhitelist:
				for _, user := range conf.Whitelist {
					if req.User == user {
						whitelisted = true
						break CheckWhitelist
					}
				}
				var state access.State
				if whitelisted {
					eprintln("User %q in whitelist, approving request...", req.User)
					state = access.StateApproved
				} else {
					eprintln("User %q not in whitelist, denying request...", req.User)
					state = access.StateDenied
				}
				if err := client.SetRequestState(ctx, req.ID, state); err != nil {
					return trace.Wrap(err)
				}
				eprintln("ok.\n")
			case access.OpDelete:
				// request has been removed (expired).
				// Only the ID is non-zero in this case.
				// Due to some limitations in Teleport's event system, filters
				// don't really work with OpDelete events.  As such, we may get
				// OpDelete events for requests that would not typically match
				// the filter argument we supplied above.
				eprintln("Request %s has automatically expired.\n", req.ID)
			default:
				return trace.BadParameter("unexpected event operation %s", op)
			}
		case <-watcher.Done():
			return watcher.Error()
		}
	}
}