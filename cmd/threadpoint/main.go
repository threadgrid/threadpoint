// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(rootCtx, os.Stdout, os.Stderr, os.Stdin, os.Args[1:])
	stop()
	os.Exit(code)
}

func execute(ctx context.Context, stdout io.Writer, stderr io.Writer, stdin io.Reader, args []string) int {
	if handled, code := runInheritedInstallLockHelper(stdout, stderr, stdin, args); handled {
		return code
	}
	app := newCLI(stdout, stderr, stdin)
	if err := runWithRootContext(ctx, app, args); err != nil {
		_ = writeCLIError(stderr, app, args, err)
		return exitCode(err)
	}
	return ExitOK
}

func runWithRootContext(rootCtx context.Context, app *cli, args []string) error {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	return classifyRuntimeError(app.run(rootCtx, args))
}
