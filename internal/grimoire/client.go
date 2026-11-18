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

func (c *Client) get(ctx context.Context, path string, out any) error {
	u := c.baseURL + "/" + path + "?environment=" + url.QueryEscape(c.environment)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Grimoire unreachable: %w", err)
	}
	defer resp.Body.Close()
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
