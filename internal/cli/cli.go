package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

const usage = `Usage: sqliteadmin [serve|start|status|stop] [options]

  serve   Run in the foreground (default); stop with Ctrl-C
  start   Run in the background; wait for startup confirmation
  status  Show the running instance for a database (exit 3 if stopped)
  stop    Gracefully stop that instance

Examples:
  sqliteadmin serve --db ./app.db --env-file .env
  sqliteadmin start --db ./app.db --addr 127.0.0.1:8081
  sqliteadmin status --db ./app.db
  sqliteadmin stop --db ./app.db

Use "sqliteadmin serve --help" to see options. Background management is
supported on Linux and macOS. No database is created automatically.
`

func Run(args []string, out, errOut io.Writer) int {
	command := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	if command == "help" || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h") && command == "serve") {
		fmt.Fprint(out, usage)
		// Also show the flags without loading configuration.
		_, _ = parseOptions([]string{"--help"}, out)
		return 0
	}
	if command != "serve" && command != "start" && command != "status" && command != "stop" && command != "_daemon" {
		fmt.Fprintf(errOut, "Unknown command %q\n%s", command, usage)
		return 2
	}
	options, err := parseOptions(args, errOut)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err == nil {
		switch command {
		case "serve":
			err = serveManaged(options, nil)
		case "_daemon":
			err = daemonChild(options)
		case "start":
			var state instance
			state, err = startDaemon(options)
			if err == nil {
				fmt.Fprintf(out, "Started sqliteadmin (PID %d) at http://%s\nDatabase: %s\nLog: %s\n", state.PID, state.Address, state.Database, state.LogFile)
			}
		case "status":
			var state instance
			state, err = daemonStatus(options)
			if errors.Is(err, errNotRunning) {
				fmt.Fprintln(out, "Not running")
				return 3
			}
			if err == nil {
				mode := "read & write"
				if state.ReadOnly {
					mode = "read-only"
				}
				fmt.Fprintf(out, "Running (PID %d) at http://%s\nDatabase: %s\nMode: %s\nStarted: %s\n", state.PID, state.Address, state.Database, mode, state.StartedAt.Format("2006-01-02 15:04:05 MST"))
				if state.LogFile != "" {
					fmt.Fprintf(out, "Log: %s\n", state.LogFile)
				}
			}
		case "stop":
			err = stopDaemon(options)
			if errors.Is(err, errNotRunning) {
				fmt.Fprintln(out, "Not running")
				return 0
			}
			if err == nil {
				fmt.Fprintln(out, "Stopped sqliteadmin")
			}
		}
	}
	if err != nil {
		fmt.Fprintf(errOut, "sqliteadmin: %v\n", err)
		return 1
	}
	return 0
}
