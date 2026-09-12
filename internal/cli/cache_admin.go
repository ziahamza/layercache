package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
)

func runCacheAdmin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cache requires quota, pins, pin, or unpin")
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("cache "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "Team or Public server configuration")
	maximum := flags.Int64("max-bytes", -1, "change the project quota, evicting unpinned entries if needed")
	owner := flags.String("name", "", "administrator pin name")
	input := flags.String("input", "", "JSON containing the complete pin key, digest, and size")
	after := flags.String("after", "", "exclusive pin pagination cursor")
	limit := flags.Int("limit", 100, "pin page size (1-1000)")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected cache command arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Role != "team" && cfg.Role != "public" {
		return errors.New("cache administration requires the Team or Public server configuration; run it on that host")
	}
	method, path := http.MethodGet, "/v1/cache/"
	var body []byte
	switch args[0] {
	case "quota":
		path += "quota"
		if *maximum != -1 {
			if *maximum <= 0 {
				return errors.New("--max-bytes must be positive")
			}
			method = http.MethodPut
			body, err = json.Marshal(map[string]int64{"maxBytes": *maximum})
		}
	case "pins":
		if *limit < 1 || *limit > 1000 {
			return errors.New("--limit must be between 1 and 1000")
		}
		path += "pins?" + url.Values{"after": {*after}, "limit": {fmt.Sprint(*limit)}}.Encode()
	case "pin", "unpin":
		if *owner == "" {
			return errors.New("--name is required")
		}
		path += "pins/" + url.PathEscape(*owner)
		method = http.MethodDelete
		if args[0] == "pin" {
			method = http.MethodPut
			if *input == "" {
				return errors.New("--input is required")
			}
			file, openErr := os.Open(*input)
			if openErr != nil {
				return openErr
			}
			decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
			decoder.DisallowUnknownFields()
			var pin artifact.Pin
			decodeErr := decoder.Decode(&pin)
			var trailing any
			if decodeErr == nil && decoder.Decode(&trailing) != io.EOF {
				decodeErr = errors.New("pin input must contain one JSON object")
			}
			_ = file.Close()
			if decodeErr != nil {
				return decodeErr
			}
			if pin.Owner != "" && pin.Owner != *owner {
				return errors.New("input pin owner does not match --name")
			}
			pin.Owner = *owner
			body, err = json.Marshal(pin)
		}
	default:
		return errors.New("cache requires quota, pins, pin, or unpin")
	}
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, localRuntimeURL(cfg.Listen)+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := newLocalCLIHTTPClient(controlRequestTimeout).Do(request)
	if err != nil {
		return fmt.Errorf("cache administration: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("cache administration returned HTTP %d; inspect quota or pins before retrying", response.StatusCode)
	}
	result := map[string]any{}
	if response.StatusCode == http.StatusNoContent {
		result = map[string]any{"removed": true, "name": *owner}
	}
	if response.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
			return fmt.Errorf("decode cache administration result: %w", err)
		}
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(result)
	}
	pretty, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(pretty))
	return err
}
