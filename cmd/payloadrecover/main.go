// payloadrecover inspects or recovers one detached experimental payload value.
// It never opens an AgentsView archive or loads application configuration.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"go.kenn.io/agentsview/internal/payloadcodec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("payloadrecover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	input := flags.String("input", "", "Detached encoded value file; never an archive database")
	output := flags.String("output", "", "New recovered file; existing files are never replaced")
	inspect := flags.Bool("inspect", false, "Inspect framing without verifying content checksums")
	maxBytes := flags.Uint64("max-bytes", payloadcodec.MaxDecodedBytes, "Maximum decoded bytes to accept")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *input == "" || (!*inspect && *output == "") || (*inspect && *output != "") {
		return errors.New("specify --input and either --inspect or --output")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := os.Open(*input)
	if err != nil {
		return fmt.Errorf("open detached value: %w", err)
	}
	defer source.Close()
	stat, err := source.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() {
		return errors.New("input must be a regular detached-value file")
	}
	if stat.Size() > payloadcodec.MaxEncodedBytes {
		return payloadcodec.ErrLimit
	}
	if *inspect {
		info, err := payloadcodec.Inspect(ctx, source, *maxBytes)
		if err != nil {
			return err
		}
		return json.MarshalWrite(stdout, info)
	}
	// Stage on the destination filesystem. A successful hard link publishes a
	// complete verified file without an overwrite race. Platforms/filesystems
	// without hard-link support fail safely and leave the input untouched.
	staged, err := os.CreateTemp(filepath.Dir(*output), ".payload-recovery-*")
	if err != nil {
		return fmt.Errorf("create recovery staging file: %w", err)
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	codec, err := payloadcodec.New(1)
	if err != nil {
		return err
	}
	if _, err := codec.Recover(ctx, staged, source, *maxBytes); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(staged.Name(), *output); err != nil {
		return fmt.Errorf("publish recovered file without replacing an existing file: %w", err)
	}
	return nil
}
