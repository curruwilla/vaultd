// Command vaultd backs up MySQL, MariaDB, PostgreSQL and MongoDB databases to
// S3-compatible object storage. Everything it does lives in internal/; main
// only wires the CLI to the process.
package main

import (
	"os"

	"github.com/curruwilla/vaultd/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
