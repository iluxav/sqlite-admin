// Command sqliteadmin serves the SQLite admin UI without a host application.
package main

import (
	"os"

	"github.com/iluxav/sqlite-client/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr)) }
