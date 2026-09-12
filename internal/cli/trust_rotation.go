package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publictrust"
)

func runTrustUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("public trust-update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "consumer configuration file")
	rotationPath := flags.String("rotation", "", "rotation envelope signed by the current trust key")
	apply := flags.Bool("apply", false, "persist the verified key rotation")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *rotationPath == "" || flags.NArg() != 0 {
		return errors.New("--rotation is required")
	}
	if *apply {
		unlock, err := lockConfiguration(ctx, *configPath)
		if err != nil {
			return err
		}
		defer unlock()
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.PublicPrivateKey != "" || cfg.PublicURL == "" {
		return errors.New("trust-update requires a consumer configuration with a Public Cache URL")
	}
	key, err := publictrust.DecodePublicKey(cfg.PublicTrustKey)
	if err != nil {
		return err
	}
	file, err := os.Open(*rotationPath)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 96<<10))
	decoder.DisallowUnknownFields()
	var envelope publictrust.Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("read trust rotation: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("trust rotation must contain exactly one envelope")
	}
	rotation, err := publictrust.VerifyRotation(key, envelope, cfg.PublicURL, cfg.PublicTrustSequence, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("verify trust rotation: %w", err)
	}
	if *apply {
		if err := rejectConfigurationMutationWhileRuntimeActive(cfg); err != nil {
			return err
		}
		cfg.PublicTrustKey, cfg.PublicTrustSequence = rotation.NextKey, rotation.Sequence
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := config.Save(*configPath, cfg); err != nil {
			return err
		}
	}
	return printResult(stdout, *jsonOutput, map[string]any{"applied": *apply, "endpoint": rotation.Endpoint, "sequence": rotation.Sequence, "publicKey": rotation.NextKey}, "Public Cache trust rotation verified")
}

func runTrustSign(args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("public trust-sign", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "Public Cache signing configuration")
	endpoint := flags.String("endpoint", "", "exact HTTPS Public Cache endpoint")
	next := flags.String("next-key", "", "next Ed25519 public key")
	sequence := flags.Uint64("sequence", 0, "monotonically increasing rotation sequence")
	validFor := flags.Duration("valid-for", 24*time.Hour, "distribution validity window (at most 30 days)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected trust-sign arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Role != "public" {
		return errors.New("trust-sign requires a Public Cache signing configuration")
	}
	private, err := publictrust.DecodePrivateKey(cfg.PublicPrivateKey)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	envelope, err := publictrust.SignRotation(private, publictrust.Rotation{Version: 1, Endpoint: *endpoint, Sequence: *sequence,
		PreviousKey: cfg.PublicTrustKey, NextKey: *next, IssuedAt: now, ExpiresAt: now.Add(*validFor)})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(envelope)
}
