package cli

import (
	"flag"
	"fmt"
	"io"
	"strconv"
)

type Mode string

const (
	ModeRun       Mode = "run"
	ModeConfigure Mode = "configure"
)

type Options struct {
	Mode         Mode
	WorkflowPath string
	Port         int
	PortSet      bool
	DataDir      string
	OpenBrowser  bool
}

func Parse(args []string) (Options, error) {
	opts := Options{Mode: ModeRun, WorkflowPath: "./WORKFLOW.md"}
	if len(args) > 0 && args[0] == string(ModeConfigure) {
		opts.Mode = ModeConfigure
		args = args[1:]
	}
	return parseFlagSet(opts, args)
}

func parseFlagSet(opts Options, args []string) (Options, error) {
	flags := flag.NewFlagSet("symphony", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var((*portValue)(&opts), "port", "port to listen on")
	flags.StringVar(&opts.DataDir, "data-dir", "", "directory for Symphony data")
	flags.BoolVar(&opts.OpenBrowser, "open", false, "open Symphony in a browser")

	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		opts.WorkflowPath = args[0]
		args = args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return Options{}, err
	}
	if extras := flags.Args(); len(extras) > 0 {
		return Options{}, fmt.Errorf("unexpected argument %q", extras[0])
	}
	return opts, nil
}

type portValue Options

func (value *portValue) String() string {
	return strconv.Itoa(value.Port)
}

func (value *portValue) Set(text string) error {
	port, err := strconv.Atoi(text)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	value.Port = port
	value.PortSet = true
	return nil
}
