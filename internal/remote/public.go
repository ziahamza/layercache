package remote

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/publictrust"
)

type PublicClient struct {
	baseURL *url.URL
	key     ed25519.PublicKey
	http    *http.Client
}

type PublicDownload struct {
	Publication publictrust.Publication
	Envelope    publictrust.Envelope
	Body        io.ReadCloser
}

type publicResolveRequest struct {
	Integration   string `json:"integration"`
	Project       string `json:"project"`
	Compatibility string `json:"compatibility"`
	NativeKey     string `json:"nativeKey"`
}

type publicResolveResponse struct {
	Envelope    publictrust.Envelope `json:"envelope"`
	ArtifactURL string               `json:"artifactUrl"`
}

func NewPublicClient(baseURL, encodedKey string) (*PublicClient, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Public Cache URL %q", baseURL)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Public Cache URL cannot contain credentials, a query, or a fragment")
	}
	if parsed.Scheme == "http" && !publicLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("Public Cache URL must use HTTPS except on loopback")
	}
	key, err := publictrust.DecodePublicKey(encodedKey)
	if err != nil {
		return nil, err
	}
	return &PublicClient{
		baseURL: parsed,
		key:     key,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				ResponseHeaderTimeout: 2 * time.Second,
			},
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("Public Cache redirect limit exceeded")
				}
				if request.URL.Scheme != parsed.Scheme || request.URL.Host != parsed.Host {
					return errors.New("Public Cache redirect changed origin")
				}
				return nil
			},
		},
	}, nil
}

func (client *PublicClient) Resolve(ctx context.Context, expected publictrust.Expected) (publictrust.Publication, publictrust.Envelope, string, error) {
	requestBody, err := json.Marshal(publicResolveRequest{
		Integration: expected.Integration, Project: expected.Project,
		Compatibility: expected.Compatibility, NativeKey: expected.NativeKey,
	})
	if err != nil {
		return publictrust.Publication{}, publictrust.Envelope{}, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.route("/v1/public/resolve").String(), strings.NewReader(string(requestBody)))
	if err != nil {
		return publictrust.Publication{}, publictrust.Envelope{}, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return publictrust.Publication{}, publictrust.Envelope{}, "", fmt.Errorf("resolve Public Cache: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone || response.StatusCode == http.StatusConflict {
		return publictrust.Publication{}, publictrust.Envelope{}, "", ErrMiss
	}
	if response.StatusCode != http.StatusOK {
		return publictrust.Publication{}, publictrust.Envelope{}, "", fmt.Errorf("Public Cache resolve returned HTTP %d", response.StatusCode)
	}
	var resolved publicResolveResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&resolved); err != nil {
		return publictrust.Publication{}, publictrust.Envelope{}, "", fmt.Errorf("decode Public Cache publication: %w", err)
	}
	publication, err := publictrust.Verify(client.key, resolved.Envelope, expected, time.Now().UTC())
	if err != nil {
		return publictrust.Publication{}, publictrust.Envelope{}, "", err
	}
	artifactURL, err := url.Parse(resolved.ArtifactURL)
	if err != nil {
		return publictrust.Publication{}, publictrust.Envelope{}, "", errors.New("Public Cache returned an invalid artifact URL")
	}
	if !artifactURL.IsAbs() {
		artifactURL = client.baseURL.ResolveReference(artifactURL)
	}
	if artifactURL.Scheme != client.baseURL.Scheme || artifactURL.Host != client.baseURL.Host {
		return publictrust.Publication{}, publictrust.Envelope{}, "", errors.New("Public Cache artifact URL changed origin")
	}
	return publication, resolved.Envelope, artifactURL.String(), nil
}

func (client *PublicClient) route(path string) *url.URL {
	target := *client.baseURL
	target.Path = strings.TrimRight(client.baseURL.Path, "/") + path
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	return &target
}

func publicLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (client *PublicClient) Get(ctx context.Context, expected publictrust.Expected) (PublicDownload, error) {
	publication, envelope, artifactURL, err := client.Resolve(ctx, expected)
	if err != nil {
		return PublicDownload{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return PublicDownload{}, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return PublicDownload{}, fmt.Errorf("download Public Cache artifact: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
			return PublicDownload{}, ErrMiss
		}
		return PublicDownload{}, fmt.Errorf("Public Cache artifact returned HTTP %d", response.StatusCode)
	}
	return PublicDownload{Publication: publication, Envelope: envelope, Body: response.Body}, nil
}
