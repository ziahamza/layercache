package buildkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	promotionAcquirePath    = "/v1/buildkit/promotion-leases/acquire"
	promotionRenewPath      = "/v1/buildkit/promotion-leases/renew"
	promotionReleasePath    = "/v1/buildkit/promotion-leases/release"
	promotionRequestTimeout = 3 * time.Second
	promotionMaxBackoff     = 500 * time.Millisecond
	maximumPromotionBody    = 64 << 10
)

type HTTPPromotionCoordinator struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

type promotionLeaseRequest struct {
	Reference  string `json:"reference"`
	LeaseToken string `json:"leaseToken,omitempty"`
}

type promotionLeaseResponse struct {
	LeaseToken string    `json:"leaseToken"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

func NewHTTPPromotionCoordinator(baseURL, token string) (*HTTPPromotionCoordinator, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid Team Cache URL %q", baseURL)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Team Cache URL cannot contain credentials, a query, or a fragment")
	}
	if parsed.Scheme == "http" && !promotionLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("Team Cache promotion coordination requires HTTPS except on loopback")
	}
	if token == "" {
		return nil, errors.New("Team Cache token is required for promotion coordination")
	}
	return &HTTPPromotionCoordinator{
		baseURL: parsed,
		token:   token,
		http: &http.Client{
			Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Second},
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("Team Cache redirect limit exceeded")
				}
				if request.URL.Scheme != parsed.Scheme || request.URL.Host != parsed.Host {
					return errors.New("Team Cache redirect changed origin")
				}
				return nil
			},
		},
	}, nil
}

func (coordinator *HTTPPromotionCoordinator) Acquire(ctx context.Context, reference string) (PromotionLease, error) {
	if reference == "" {
		return nil, errors.New("Team Cache promotion reference is required")
	}
	backoff := 50 * time.Millisecond
	for {
		requestCtx, cancel := context.WithTimeout(ctx, promotionRequestTimeout)
		response, err := coordinator.request(requestCtx, promotionAcquirePath, promotionLeaseRequest{Reference: reference})
		if err != nil {
			cancel()
			return nil, err
		}
		if response.StatusCode == http.StatusConflict {
			closePromotionResponse(response)
			cancel()
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			backoff *= 2
			if backoff > promotionMaxBackoff {
				backoff = promotionMaxBackoff
			}
			continue
		}
		if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
			closePromotionResponse(response)
			cancel()
			return nil, fmt.Errorf("Team Cache promotion lease returned HTTP %d", response.StatusCode)
		}
		leased, decodeErr := decodePromotionLeaseResponse(response.Body)
		closePromotionResponse(response)
		cancel()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode Team Cache promotion lease: %w", decodeErr)
		}
		if leased.LeaseToken == "" || !leased.ExpiresAt.After(time.Now().UTC()) {
			return nil, errors.New("Team Cache returned an invalid promotion lease")
		}
		return &httpPromotionLease{
			coordinator: coordinator, reference: reference, token: leased.LeaseToken,
			expiresAt: leased.ExpiresAt,
		}, nil
	}
}

type httpPromotionLease struct {
	coordinator *HTTPPromotionCoordinator
	reference   string
	token       string
	expiresAt   time.Time
	mu          sync.Mutex
	released    bool
}

// Maintain renews the lease halfway to its current expiry. Any failed renewal
// returns immediately so the caller can cancel the mutable registry mutation
// while the previous lease is still valid.
func (lease *httpPromotionLease) Maintain(ctx context.Context) error {
	for {
		lease.mu.Lock()
		if lease.released {
			lease.mu.Unlock()
			return errors.New("Team Cache promotion lease was released while still in use")
		}
		expiresAt := lease.expiresAt
		lease.mu.Unlock()
		remaining := time.Until(expiresAt)
		if remaining <= 0 {
			return errors.New("Team Cache promotion lease expired")
		}
		timer := time.NewTimer(remaining / 2)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if err := lease.renew(ctx); err != nil {
			return err
		}
	}
}

func (lease *httpPromotionLease) renew(ctx context.Context) error {
	lease.mu.Lock()
	if lease.released {
		lease.mu.Unlock()
		return errors.New("Team Cache promotion lease was released")
	}
	token := lease.token
	reference := lease.reference
	lease.mu.Unlock()
	requestCtx, cancel := context.WithTimeout(ctx, promotionRequestTimeout)
	defer cancel()
	response, err := lease.coordinator.request(requestCtx, promotionRenewPath, promotionLeaseRequest{
		Reference: reference, LeaseToken: token,
	})
	if err != nil {
		return err
	}
	defer closePromotionResponse(response)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Team Cache promotion lease renewal returned HTTP %d", response.StatusCode)
	}
	renewed, err := decodePromotionLeaseResponse(response.Body)
	if err != nil {
		return fmt.Errorf("decode Team Cache promotion lease renewal: %w", err)
	}
	if renewed.LeaseToken != token || !renewed.ExpiresAt.After(time.Now().UTC()) {
		return errors.New("Team Cache returned an invalid renewed promotion lease")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.released || lease.token != token {
		return errors.New("Team Cache promotion lease changed during renewal")
	}
	lease.expiresAt = renewed.ExpiresAt
	return nil
}

func decodePromotionLeaseResponse(body io.Reader) (promotionLeaseResponse, error) {
	encoded, err := io.ReadAll(io.LimitReader(body, maximumPromotionBody+1))
	if err != nil {
		return promotionLeaseResponse{}, err
	}
	if len(encoded) > maximumPromotionBody {
		return promotionLeaseResponse{}, fmt.Errorf("response exceeds %d bytes", maximumPromotionBody)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var response promotionLeaseResponse
	if err := decoder.Decode(&response); err != nil {
		return promotionLeaseResponse{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return promotionLeaseResponse{}, errors.New("response contains trailing JSON")
	}
	return response, nil
}

func (lease *httpPromotionLease) Release(ctx context.Context) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.released {
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, promotionRequestTimeout)
	defer cancel()
	response, err := lease.coordinator.request(requestCtx, promotionReleasePath, promotionLeaseRequest{
		Reference: lease.reference, LeaseToken: lease.token,
	})
	if err != nil {
		return err
	}
	defer closePromotionResponse(response)
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return fmt.Errorf("Team Cache promotion lease release returned HTTP %d", response.StatusCode)
	}
	lease.released = true
	return nil
}

func (coordinator *HTTPPromotionCoordinator) request(
	ctx context.Context,
	path string,
	body promotionLeaseRequest,
) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, coordinator.route(path).String(), bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+coordinator.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := coordinator.http.Do(request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (coordinator *HTTPPromotionCoordinator) route(path string) *url.URL {
	target := *coordinator.baseURL
	target.Path = strings.TrimRight(coordinator.baseURL.Path, "/") + path
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	return &target
}

func closePromotionResponse(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumPromotionBody))
	_ = response.Body.Close()
}

func promotionLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
