package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

func main() {
	defaultScenario := os.Getenv("SYMPHONY_FAKE_CODEX_SCENARIO")
	if defaultScenario == "" {
		defaultScenario = "happy"
	}
	scenario := flag.String("scenario", defaultScenario, "deterministic fake app-server scenario")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "fake app-server does not accept positional arguments")
		os.Exit(2)
	}
	if *scenario == "full" {
		for _, name := range []string{"SYMPHONY_E2E_SECRET_CANARY", "SYMPHONY_GITHUB_TEST_TOKEN", "SYMPHONY_LINEAR_TEST_TOKEN"} {
			if os.Getenv(name) != "" {
				fmt.Fprintln(os.Stderr, "fake app-server received a forbidden credential variable")
				os.Exit(1)
			}
		}
		fmt.Fprintln(os.Stderr, "fixture stderr phase4-secret-canary")
	}
	if *scenario == "stderr-noise" {
		for range 40 {
			fmt.Fprintln(os.Stderr, "fixture stderr noise phase4-secret-canary")
		}
	}
	if err := runScenario(*scenario, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fake app-server failed:", err)
		os.Exit(1)
	}
	// The production app-server remains alive until its owner stops the process
	// tree. Do the same so a normal stdin close cannot race the client's orderly
	// shutdown path and look like an unexpected transport EOF.
	if runtime.GOOS == "windows" {
		return
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	<-interrupt
}
