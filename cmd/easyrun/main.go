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
	code := run(ctx, cli.RestoreInterrupt, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run — stopSignals dipanggil `expose` setelah sinyal berhenti pertama
// (lihat App.StopSignals; main memakai cli.RestoreInterrupt).
func run(ctx context.Context, stopSignals func(), args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := cli.New(stdin, stdout, stderr)
	a.StopSignals = stopSignals
	return a.Run(ctx, args)
}
