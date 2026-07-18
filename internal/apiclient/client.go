// Package apiclient is the CLI's HTTP client for main's status API,
// configured via HOOKPLOY_URL and HOOKPLOY_ADMIN_TOKEN.
package apiclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/reorx/hookploy/internal/api"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// FromEnv builds a client from HOOKPLOY_URL + HOOKPLOY_ADMIN_TOKEN.
func FromEnv() (*Client, error) {
	base := os.Getenv("HOOKPLOY_URL")
	tok := os.Getenv("HOOKPLOY_ADMIN_TOKEN")
	if base == "" || tok == "" {
		return nil, errors.New("HOOKPLOY_URL and HOOKPLOY_ADMIN_TOKEN must be set (remote CLI mode)")
	}
	return &Client{BaseURL: strings.TrimRight(base, "/"), Token: tok, HTTP: http.DefaultClient}, nil
}

// do performs a request and returns the raw body; non-2xx becomes an error
// carrying the server's message.
func (c *Client) do(method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr api.Error
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Error != "" {
			return nil, fmt.Errorf("%s (HTTP %d)", apiErr.Error, resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// GetRaw fetches a path returning the raw JSON — the CLI --json output is
// exactly this body, guaranteeing API ≡ CLI output.
func (c *Client) GetRaw(path string) ([]byte, error) { return c.do(http.MethodGet, path, nil) }

func decode[T any](data []byte, err error) (T, []byte, error) {
	var v T
	if err != nil {
		return v, nil, err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, nil, err
	}
	return v, data, nil
}

func (c *Client) GetDeploy(id string) (*api.Deploy, []byte, error) {
	return decode[*api.Deploy](c.GetRaw("/deploys/" + url.PathEscape(id)))
}

func (c *Client) Services() ([]api.ServiceSummary, []byte, error) {
	return decode[[]api.ServiceSummary](c.GetRaw("/services"))
}

func (c *Client) ServiceDeploys(name string) ([]*api.Deploy, []byte, error) {
	return decode[[]*api.Deploy](c.GetRaw("/services/" + url.PathEscape(name) + "/deploys"))
}

func (c *Client) Servers() ([]api.ServerInfo, []byte, error) {
	return decode[[]api.ServerInfo](c.GetRaw("/servers"))
}

func (c *Client) TriggerDeploy(service string, payload []byte) (*api.Accepted, []byte, error) {
	return decode[*api.Accepted](c.do(http.MethodPost, "/services/"+url.PathEscape(service)+"/deploy", payload))
}

func (c *Client) TriggerTask(service, task, instance string, payload []byte) (*api.Accepted, []byte, error) {
	path := "/services/" + url.PathEscape(service) + "/tasks/" + url.PathEscape(task)
	if instance != "" {
		path += "?instance=" + url.QueryEscape(instance)
	}
	return decode[*api.Accepted](c.do(http.MethodPost, path, payload))
}

// Logs opens the log stream of a deploy: NDJSON when json or follow is set,
// plain text otherwise. Caller closes.
func (c *Client) Logs(id string, follow, ndjson bool) (io.ReadCloser, error) {
	q := url.Values{}
	if follow {
		q.Set("follow", "1")
	} else if ndjson {
		q.Set("format", "json")
	}
	path := "/deploys/" + url.PathEscape(id) + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp.Body, nil
}
