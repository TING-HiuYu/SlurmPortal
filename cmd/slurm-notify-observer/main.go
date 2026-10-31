// slurm-notify-observer is the controller-local, GET-only mTLS REST gateway that lets the slave read
// job and node details from slurmrestd without holding the Slurm JWT key itself.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
)

func main() {
	path := flag.String("config", "/etc/go-ldap-admin/job-notify-observer.yaml", "private controller observer configuration")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected observer arguments")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if jobnotify.RunObserver(ctx, *path) != nil {
		fmt.Fprintln(os.Stderr, "notification observer failed")
		os.Exit(1)
	}
}
