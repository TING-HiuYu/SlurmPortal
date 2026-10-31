// slurm-lifecycle-hook registers every execution start/end without delaying
// scheduling for plugin work. Operational failures are deliberately fail-open.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	joblifecycle "github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/runtime"
)

func run(args []string, getenv func(string) string, out io.Writer) int {
	flags := flag.NewFlagSet("slurm-lifecycle-hook", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "/etc/go-ldap-admin/lifecycle-hook.yaml", "private hook configuration")
	kind := flags.String("event", "", "starting or ending")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*kind != "starting" && *kind != "ending") {
		return 2
	}
	cfg, err := joblifecycle.LoadHookConfig(*path)
	if err != nil {
		_, _ = fmt.Fprintln(out, "lifecycle hook configuration unavailable")
		return 0
	}
	id, err := strconv.ParseUint(getenv("SLURM_JOB_ID"), 10, 32)
	if err != nil {
		_, _ = fmt.Fprintln(out, "lifecycle hook execution identity invalid")
		return 0
	}
	c := jobnotify.Callback{JobID: uint32(id), SLUID: getenv("SLURM_JOB_SLUID")}
	event := joblifecycle.StartingType
	if *kind == "ending" {
		event = joblifecycle.EndingType
	}
	if joblifecycle.SendHook(context.Background(), cfg, event, c) != nil {
		_, _ = fmt.Fprintln(out, "lifecycle event was not accepted")
	}
	return 0
}
func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stderr)) }
