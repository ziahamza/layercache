//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/layercache/layercache/internal/publicbuild/sandbox/actionsjob"
)

const maximumRequestBytes = 64 << 10

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "maintained Actions runner:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("layercache-actions-public-job", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	requestPath := flags.String("request", "", "root-owned request JSON")
	outputDirectory := flags.String("output", "", "root-owned output directory")
	if err := flags.Parse(os.Args[1:]); err != nil || flags.NArg() != 0 || *requestPath == "" || *outputDirectory == "" {
		return errors.New("usage: layercache-actions-public-job --request FILE --output DIRECTORY")
	}
	info, err := os.Lstat(*requestPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumRequestBytes {
		return errors.New("request must be one bounded regular file")
	}
	encoded, err := os.ReadFile(*requestPath)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var request actionsjob.Request
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request contains trailing JSON")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	_, err = actionsjob.Run(ctx, request, *outputDirectory)
	return err
}
