package githubauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultDeviceEndpoint = "https://github.com/login/device/code"
	DefaultTokenEndpoint  = "https://github.com/login/oauth/access_token"
	deviceGrantType       = "urn:ietf:params:oauth:grant-type:device_code"
)

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`
}

type Session struct {
	AccessToken           string    `json:"access_token"`
	TokenType             string    `json:"token_type"`
	Scope                 string    `json:"scope"`
	RefreshToken          string    `json:"refresh_token,omitempty"`
	ExpiresIn             int64     `json:"expires_in,omitempty"`
	RefreshTokenExpiresIn int64     `json:"refresh_token_expires_in,omitempty"`
	ClientID              string    `json:"client_id,omitempty"`
	TokenEndpoint         string    `json:"token_endpoint,omitempty"`
	ObtainedAt            time.Time `json:"obtained_at,omitempty"`
}

type DeviceClient struct {
	ClientID       string
	DeviceEndpoint string
	TokenEndpoint  string
	HTTPClient     *http.Client
}

func (client DeviceClient) Begin(ctx context.Context) (DeviceCode, error) {
	if strings.TrimSpace(client.ClientID) == "" {
		return DeviceCode{}, errors.New("GitHub OAuth client ID is required")
	}
	endpoint := client.DeviceEndpoint
	if endpoint == "" {
		endpoint = DefaultDeviceEndpoint
	}
	values := url.Values{"client_id": {client.ClientID}, "scope": {"read:user offline_access"}}
	var response DeviceCode
	if err := client.postForm(ctx, endpoint, values, &response); err != nil {
		return DeviceCode{}, fmt.Errorf("start GitHub device authorization: %w", err)
	}
	if response.DeviceCode == "" || response.UserCode == "" || response.VerificationURI == "" || response.ExpiresIn <= 0 {
		return DeviceCode{}, errors.New("GitHub device authorization returned an incomplete response")
	}
	if response.Interval <= 0 {
		response.Interval = 5
	}
	return response, nil
}

func (client DeviceClient) Poll(ctx context.Context, device DeviceCode) (Session, error) {
	if device.DeviceCode == "" || device.ExpiresIn <= 0 {
		return Session{}, errors.New("valid GitHub device code is required")
	}
	endpoint := client.TokenEndpoint
	if endpoint == "" {
		endpoint = DefaultTokenEndpoint
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	deadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	for {
		if !time.Now().Before(deadline) {
			return Session{}, errors.New("GitHub device authorization expired")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Session{}, ctx.Err()
		case <-timer.C:
		}
		values := url.Values{
			"client_id":   {client.ClientID},
			"device_code": {device.DeviceCode},
			"grant_type":  {deviceGrantType},
		}
		var response struct {
			Session
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			Interval         int64  `json:"interval"`
		}
		if err := client.postForm(ctx, endpoint, values, &response); err != nil {
			return Session{}, fmt.Errorf("poll GitHub device authorization: %w", err)
		}
		if response.AccessToken != "" {
			return client.finalizeSession(response.Session, endpoint)
		}
		switch response.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			increase := time.Duration(response.Interval) * time.Second
			if increase <= 0 {
				increase = 5 * time.Second
			}
			interval += increase
			continue
		case "expired_token":
			return Session{}, errors.New("GitHub device authorization expired")
		case "access_denied":
			return Session{}, errors.New("GitHub device authorization was denied")
		case "incorrect_device_code":
			return Session{}, errors.New("GitHub rejected the device code")
		case "incorrect_client_credentials":
			return Session{}, errors.New("GitHub rejected the OAuth client ID")
		default:
			detail := strings.TrimSpace(response.ErrorDescription)
			if detail == "" {
				detail = response.Error
			}
			if detail == "" {
				detail = "response contained neither a token nor an OAuth error"
			}
			return Session{}, fmt.Errorf("GitHub device authorization failed: %s", detail)
		}
	}
}

func (client DeviceClient) Refresh(ctx context.Context, session Session) (Session, error) {
	clientID := client.ClientID
	if clientID == "" {
		clientID = session.ClientID
	}
	if strings.TrimSpace(clientID) == "" || session.RefreshToken == "" {
		return Session{}, errors.New("GitHub OAuth session cannot be refreshed; run layercache login again")
	}
	endpoint := client.TokenEndpoint
	if endpoint == "" {
		endpoint = session.TokenEndpoint
	}
	if endpoint == "" {
		endpoint = DefaultTokenEndpoint
	}
	values := url.Values{
		"client_id": {clientID}, "grant_type": {"refresh_token"},
		"refresh_token": {session.RefreshToken},
	}
	var response struct {
		Session
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	refreshClient := client
	refreshClient.ClientID = clientID
	if err := refreshClient.postForm(ctx, endpoint, values, &response); err != nil {
		return Session{}, fmt.Errorf("refresh GitHub access token: %w", err)
	}
	if response.Error != "" || response.AccessToken == "" {
		detail := strings.TrimSpace(response.ErrorDescription)
		if detail == "" {
			detail = response.Error
		}
		if detail == "" {
			detail = "response did not contain an access token"
		}
		return Session{}, fmt.Errorf("refresh GitHub access token: %s; run layercache login again", detail)
	}
	return refreshClient.finalizeSession(response.Session, endpoint)
}

func (client DeviceClient) finalizeSession(session Session, endpoint string) (Session, error) {
	if session.AccessToken == "" {
		return Session{}, errors.New("GitHub OAuth response did not contain an access token")
	}
	if session.ExpiresIn > 0 && session.RefreshToken == "" {
		return Session{}, errors.New("GitHub returned an expiring access token without refresh material")
	}
	session.ClientID = client.ClientID
	session.TokenEndpoint = endpoint
	session.ObtainedAt = time.Now().UTC()
	return session, nil
}

func (session Session) AccessTokenNeedsRefresh(now time.Time, skew time.Duration) bool {
	if session.ExpiresIn <= 0 {
		return false
	}
	if session.ObtainedAt.IsZero() {
		return true
	}
	return !now.Add(skew).Before(session.ObtainedAt.Add(time.Duration(session.ExpiresIn) * time.Second))
}

func (client DeviceClient) postForm(ctx context.Context, endpoint string, values url.Values, result any) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) {
		return errors.New("OAuth endpoint must use HTTPS except on loopback")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("OAuth endpoint cannot contain credentials, query, or fragment")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Content-Length", strconv.Itoa(len(values.Encode())))
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	origin := parsed.Scheme + "://" + parsed.Host
	redirectSafeClient := *httpClient
	priorRedirectPolicy := httpClient.CheckRedirect
	redirectSafeClient.CheckRedirect = func(next *http.Request, redirects []*http.Request) error {
		if len(redirects) >= 3 || next.URL.Scheme+"://"+next.URL.Host != origin {
			return errors.New("refusing OAuth redirect outside its origin")
		}
		if priorRedirectPolicy != nil {
			return priorRedirectPolicy(next, redirects)
		}
		return nil
	}
	response, err := redirectSafeClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("OAuth endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(result); err != nil {
		return fmt.Errorf("decode OAuth response: %w", err)
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
