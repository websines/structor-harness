// Command forge-harness-pilot validates behavioral fixtures and runs bounded
// native harness experiments in an explicit private state directory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/harnesseval"
	"github.com/websines/structor-harness/record"
)

func main() {
	err := run()
	harnesseval.CleanupToolchain()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: forge-harness-pilot validate|run|resume|report -state ABSOLUTE [-config FILE]")
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	state := flags.String("state", "", "dedicated private absolute state directory")
	configPath := flags.String("config", "", "strict JSON configuration file")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if !filepath.IsAbs(*state) {
		return errors.New("absolute -state required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if command == "report" {
		cas, err := record.NewLocalCAS(filepath.Join(*state, "cas"))
		if err != nil {
			return err
		}
		journal, err := improve.NewJournal(filepath.Join(*state, "journal"), cas)
		if err != nil {
			return err
		}
		var report harnesseval.Report
		if _, err = journal.Read("report", &report); err != nil {
			return err
		}
		return output(report)
	}
	if command == "validate" {
		validation, err := harnesseval.ValidateCatalog(ctx, *state, func(id string) { fmt.Fprintln(os.Stderr, "validated", id) })
		if err != nil {
			return err
		}
		return output(validation)
	}
	if command != "run" && command != "resume" {
		return errors.New("unknown command")
	}
	if !filepath.IsAbs(*configPath) {
		return errors.New("absolute -config required")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var config harnesseval.Config
	if err = improve.Decode(data, &config); err != nil {
		return err
	}
	report, err := harnesseval.RunPilot(ctx, config, *state, command == "resume", nil)
	if err != nil {
		return err
	}
	if err = output(report); err != nil {
		return err
	}
	if !report.Complete {
		return errors.New("pilot incomplete; inspect retained report")
	}
	return nil
}
func output(v any) error { e := json.NewEncoder(os.Stdout); e.SetIndent("", "  "); return e.Encode(v) }
