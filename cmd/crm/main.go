package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/jdanielnd/crm-cli/internal/cli"
	"github.com/jdanielnd/crm-cli/internal/model"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	cli.Version = version
	os.Exit(run())
}

// run executes the command line. SIGINT and SIGTERM cancel the command's
// context (a running `crm run` checkpoints and stops); the process then exits
// 128 + the signal number, whatever the command returned.
func run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var received atomic.Int32
	go func() {
		select {
		case s := <-sigs:
			if n, ok := s.(syscall.Signal); ok {
				received.Store(int32(n))
			}
			cancel()
		case <-ctx.Done():
		}
	}()

	rootCmd := cli.NewRootCmd()
	err := rootCmd.ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crm: error: %s\n", err.Error())
	}
	if n := received.Load(); n != 0 {
		return 128 + int(n)
	}
	if err != nil {
		return model.ExitCode(err)
	}
	return 0
}

// Used by GoReleaser for version info.
var _ = commit
