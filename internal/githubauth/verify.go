package githubauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultAPIURL = "https://api.github.com"

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func VerifyUser(ctx context.Context, apiURL, token string) (User, error) {
	if token == "" {
		return User{}, errors.New("GitHub access token is required")
	}
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) {
		return User{}, errors.New("GitHub API URL must use HTTPS except on loopback")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return User{}, errors.New("GitHub API URL cannot contain credentials, query, or fragment")
	}
	origin := parsed.Scheme + "://" + parsed.Host
	endpoint := strings.TrimRight(apiURL, "/") + "/user"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return User{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(next *http.Request, redirects []*http.Request) error {
			if len(redirects) >= 3 || next.URL.Scheme+"://"+next.URL.Host != origin {
				return errors.New("refusing GitHub API redirect outside its origin")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return User{}, fmt.Errorf("verify GitHub identity: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return User{}, fmt.Errorf("verify GitHub identity: API returned HTTP %d", response.StatusCode)
	}
	var user User
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&user); err != nil {
		return User{}, fmt.Errorf("decode GitHub identity: %w", err)
	}
	user.Login = strings.ToLower(strings.TrimSpace(user.Login))
	if user.ID <= 0 || user.Login == "" {
		return User{}, errors.New("GitHub API returned an incomplete identity")
	}
	return user, nil
}
