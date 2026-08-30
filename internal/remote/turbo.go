package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/compatibility"
)

var (
	ErrMiss         = errors.New("remote cache miss")
	ErrUnauthorized = errors.New("remote cache authorization failed")
)

type TurboClient struct {
	baseURL       string
	token         string
	compatibility string
	http          *http.Client
}

type TurboDownload struct {
	Body           io.ReadCloser
	ExpectedDigest string
	Size           int64
	Metadata       artifact.Metadata
}

// NewTurboClient preserves the original constructor with an OS/architecture
// default. Callers whose outputs depend on more ABI facts should use
// NewTurboClientForCompatibility.
func NewTurboClient(baseURL, token string) (*TurboClient, error) {
	return NewTurboClientForCompatibility(baseURL, token, runtime.GOOS+"-"+runtime.GOARCH+"-schema1")
}

// NewTurboClientForCompatibility creates a Team Cache client that selects the
// caller's compatibility namespace after bearer authentication. The Team
// server must never substitute its own host identity for this value.
func NewTurboClientForCompatibility(baseURL, token, compatibilityID string) (*TurboClient, error) {
	if err := compatibility.Validate(compatibilityID); err != nil {
		return nil, fmt.Errorf("invalid remote cache compatibility: %w", err)
	}
	return newTurboClient(baseURL, token, compatibilityID)
}

func newTurboClient(baseURL, token, compatibilityID string) (*TurboClient, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid remote cache URL %q", baseURL)
	}
	if parsed.Scheme == "http" && !publicLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("remote cache URL must use HTTPS except on loopback")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("remote cache URL cannot contain credentials, a query, or a fragment")
	}
	if token == "" {
		return nil, errors.New("remote cache token is required")
	}
	return &TurboClient{
		baseURL:       parsed.String(),
		token:         token,
		compatibility: compatibilityID,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				ResponseHeaderTimeout: 2 * time.Second,
			},
			CheckRedirect: func(request *http.Request, _ []*http.Request) error {
				if request.URL.Scheme != parsed.Scheme || request.URL.Host != parsed.Host {
					return errors.New("remote cache redirect changed origin")
				}
				return nil
			},
		},
	}, nil
}

func (client *TurboClient) Get(ctx context.Context, hash string) (TurboDownload, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/v8/artifacts/"+url.PathEscape(hash), nil)
	if err != nil {
		return TurboDownload{}, err
	}
	client.authorize(request)
	response, err := client.http.Do(request)
	if err != nil {
		return TurboDownload{}, fmt.Errorf("read remote cache: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		return TurboDownload{}, ErrMiss
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		response.Body.Close()
		return TurboDownload{}, ErrUnauthorized
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return TurboDownload{}, fmt.Errorf("remote cache returned HTTP %d", response.StatusCode)
	}
	duration, _ := strconv.ParseInt(response.Header.Get("x-artifact-duration"), 10, 64)
	return TurboDownload{
		Body:           response.Body,
		ExpectedDigest: response.Header.Get("x-layercache-digest"),
		Size:           response.ContentLength,
		Metadata: artifact.Metadata{
			DurationMS: duration,
			Tag:        response.Header.Get("x-artifact-tag"),
		},
	}, nil
}

func (client *TurboClient) Put(ctx context.Context, hash string, entry artifact.Entry, body io.Reader) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, client.baseURL+"/v8/artifacts/"+url.PathEscape(hash), body)
	if err != nil {
		return err
	}
	client.authorize(request)
	request.ContentLength = entry.Size
	request.Header.Set("x-artifact-duration", strconv.FormatInt(entry.Metadata.DurationMS, 10))
	if entry.Metadata.Tag != "" {
		request.Header.Set("x-artifact-tag", entry.Metadata.Tag)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("publish remote cache: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return fmt.Errorf("remote cache publication returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (client *TurboClient) authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+client.token)
	if client.compatibility != "" {
		request.Header.Set(compatibility.Header, client.compatibility)
	}
}
