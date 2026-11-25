// Package grimoire is a minimal client for the Grimoire consumer API
// (guibranco/grimoire-api): configurations and secrets for one environment,
// authenticated with the application's X-Api-Key.
package grimoire

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

// ErrNotFound is returned when a secret or configuration key does not exist.
var ErrNotFound = errors.New("not found in Grimoire")

// Client reads one Grimoire environment.
type Client struct {
	baseURL     string
	apiKey      string
	environment string
	userAgent   string
	http        *http.Client
}

// New creates a client. baseURL is the Grimoire API root (e.g. https://api.grimoire.example.com).
func New(baseURL, apiKey, environment, userAgent string) *Client {
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		environment: environment,
		userAgent:   userAgent,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
}

// Environment returns the environment slug the client reads.
func (c *Client) Environment() string { return c.environment }

// Configurations returns every configuration entry of the environment
// (GET consumer/configurations).
func (c *Client) Configurations(ctx context.Context) (map[string]string, error) {
	var body struct {
		Items []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"items"`
	}
	if err := c.get(ctx, "consumer/configurations", &body); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(body.Items))
	for _, it := range body.Items {
		out[it.Key] = it.Value
	}
	return out, nil
}

// Configuration returns one configuration value (GET consumer/configurations/{key}).
func (c *Client) Configuration(ctx context.Context, key string) (string, error) {
	var body struct {
		Value string `json:"value"`
	}
	if err := c.get(ctx, "consumer/configurations/"+url.PathEscape(key), &body); err != nil {
		return "", err
	}
	return body.Value, nil
}

// Secret returns the decrypted value of a secret (GET consumer/secrets/{name}).
func (c *Client) Secret(ctx context.Context, name string) (string, error) {
	var body struct {
		Value string `json:"value"`
	}
	if err := c.get(ctx, "consumer/secrets/"+url.PathEscape(name), &body); err != nil {
		return "", err
	}
	return body.Value, nil
}

// KeyState is what a conditional read saw: whether the key exists, its value
// and the ETag to send next time.
type KeyState struct {
	Exists bool
	Value  string
	ETag   string
}

// ConfigurationIfChanged reads one configuration key, sending prev's ETag as
// If-None-Match. On 304 it returns prev with notModified = true. A missing key
// is a KeyState with Exists = false, not an error.
func (c *Client) ConfigurationIfChanged(ctx context.Context, key string, prev *KeyState) (state KeyState, notModified bool, err error) {
	header := http.Header{}
	if prev != nil && prev.ETag != "" {
		header.Set("If-None-Match", prev.ETag)
	}
	var body struct {
		Value string `json:"value"`
	}
	resp, err := c.do(ctx, "consumer/configurations/"+url.PathEscape(key), header)
	if err != nil {
		return KeyState{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && prev != nil {
		return *prev, true, nil
	}
	if err := c.decode(resp, key, &body); err != nil {
		if errors.Is(err, ErrNotFound) {
			return KeyState{}, false, nil
		}
		return KeyState{}, false, err
	}
	return KeyState{Exists: true, Value: body.Value, ETag: resp.Header.Get("ETag")}, false, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	resp, err := c.do(ctx, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.decode(resp, path, out)
}

func (c *Client) do(ctx context.Context, path string, header http.Header) (*http.Response, error) {
	u := c.baseURL + "/" + path + "?environment=" + url.QueryEscape(c.environment)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Grimoire unreachable: %w", err)
	}
	return resp, nil
}

func (c *Client) decode(resp *http.Response, path string, out any) error {
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
			return fmt.Errorf("Grimoire returned invalid JSON for %s: %w", path, err)
		}
		return nil
	case resp.StatusCode == http.StatusNotFound:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if strings.Contains(string(msg), "Environment not found") {
			return fmt.Errorf("Grimoire does not know the environment %q", c.environment)
		}
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("Grimoire rejected the API key (%d)", resp.StatusCode)
	default:
		return fmt.Errorf("Grimoire answered %d for %s", resp.StatusCode, path)
	}
}
