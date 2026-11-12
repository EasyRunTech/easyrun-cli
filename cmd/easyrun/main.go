// easyrun — program baris perintah EasyRun: login CLI, daftar box, dan SSH ke
// EasyRun Box lewat terowongan HTTPS (kontrak SSH box §6). Perintahnya ada di
// internal/cli.
package main

import (
	"context"
	"io"
	"os"

	"github.com/EasyRunTech/easyrun-cli/internal/cli"
)

func main() {
	ctx, stop := cli.SignalContext()
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return cli.New(stdin, stdout, stderr).Run(ctx, args)
}
