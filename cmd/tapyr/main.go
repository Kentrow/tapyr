// Command tapyr is the Tapyr API-to-metrics gateway.
package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // quota windows use IANA timezones, also in scratch images

	"github.com/kentrow/tapyr/internal/clock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], &env{
		stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin, lookupEnv: os.LookupEnv, clock: clock.Real(), listen: (&net.ListenConfig{}).Listen,
		reload: reloads(ctx),
	})
	stop()
	os.Exit(code)
}

// reloads turns SIGHUP into reload requests until ctx is done. Requests
// arriving during a reload are coalesced into one.
func reloads(ctx context.Context) <-chan struct{} {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	out := make(chan struct{}, 1)
	go func() {
		defer signal.Stop(hup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}

// run executes the CLI and returns the process exit code. Its dependencies
// are explicit so tests can drive it without touching the process state.
func run(ctx context.Context, args []string, e *env) int {
	e.getenv = func(k string) string { v, _ := e.lookupEnv(k); return v }
	cmd := newRootCmd(e)
	cmd.SetArgs(args)
	return report(cmd.ExecuteContext(ctx), e.stderr)
}
