// Package portainer reads stacks out of a running Portainer so they can be
// moved into stack folders without redeploying anything.
//
// Field names were checked against Portainer 2.x: stacks carry Id, Name,
// Type, Status, EndpointId, EntryPoint, ProjectPath, Env and GitConfig, and
// /api/stacks/{id}/file returns StackFileContent. Decoding is
// case-insensitive, so a release that changes only the casing still works.
package portainer

import (
	"bytes"
	"context"
	"crypto/tls"
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

// Stack types as Portainer numbers them.
const (
	TypeSwarm      = 1
	TypeCompose    = 2
	TypeKubernetes = 3
)

// Endpoint types worth naming. 1 is a local Docker socket.
const EndpointDockerLocal = 1

// Pair is one UI env var.
type Pair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// GitConfig is set on stacks deployed from a repository.
type GitConfig struct {
	URL            string `json:"URL"`
	ReferenceName  string `json:"ReferenceName"`
	ConfigFilePath string `json:"ConfigFilePath"`
}

// Stack is one entry from GET /api/stacks.
type Stack struct {
	ID          int        `json:"Id"`
	Name        string     `json:"Name"`
	Type        int        `json:"Type"`
	Status      int        `json:"Status"` // 1 active, 2 inactive
	EndpointID  int        `json:"EndpointId"`
	EntryPoint  string     `json:"EntryPoint"`
	ProjectPath string     `json:"ProjectPath"`
	Env         []Pair     `json:"Env"`
	GitConfig   *GitConfig `json:"GitConfig"`
}

// Endpoint is one environment from GET /api/endpoints.
type Endpoint struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
	Type int    `json:"Type"`
	URL  string `json:"URL"`
}

// Client talks to one Portainer. Credentials live only in this struct, for
// the length of one import session; nothing here touches the disk.
type Client struct {
	base   *url.URL
	http   *http.Client
	apiKey string
	jwt    string
}

// Options for Connect.
type Options struct {
	URL           string
	APIKey        string
	Username      string
	Password      string
	SkipTLSVerify bool // Portainer ships a self-signed certificate
}

// Connect checks the URL, authenticates, and returns a ready client.
// An API key is preferred; username and password fall back to POST /api/auth.
func Connect(ctx context.Context, o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(o.URL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not a Portainer URL; it usually looks like https://192.168.1.10:9443", o.URL)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: o.SkipTLSVerify} //nolint:gosec // user's explicit choice, for a self-signed cert on the LAN
	tr.Proxy = nil                                                        // Portainer is on the LAN; never route it through a proxy from the environment
	c := &Client{base: u, http: &http.Client{Transport: tr, Timeout: 30 * time.Second}, apiKey: strings.TrimSpace(o.APIKey)}

	if c.apiKey == "" {
		if o.Username == "" || o.Password == "" {
			return nil, errors.New("give an API token, or a username and password")
		}
		var res struct {
			JWT string `json:"jwt"`
		}
		if err := c.do(ctx, http.MethodPost, "/api/auth", map[string]string{"username": o.Username, "password": o.Password}, &res); err != nil {
			return nil, fmt.Errorf("log in to Portainer: %w", err)
		}
		if res.JWT == "" {
			return nil, errors.New("the login was accepted, but Portainer returned no token")
		}
		c.jwt = res.JWT
	}
	// One cheap authenticated call proves the credentials work.
	if _, err := c.Endpoints(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// APIError is a non-2xx answer from Portainer.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("Portainer answered %d", e.Status)
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " (check the token or password, and that the user is an administrator)"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	} else if c.jwt != "" {
		req.Header.Set("Authorization", "Bearer "+c.jwt)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var cv *tls.CertificateVerificationError
		if errors.As(err, &cv) {
			return fmt.Errorf("%w. Portainer uses a self-signed certificate by default; tick Skip TLS verify", err)
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return &APIError{Status: resp.StatusCode, Body: snippet}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", path, err)
	}
	return nil
}

// Endpoints lists environments.
func (c *Client) Endpoints(ctx context.Context) ([]Endpoint, error) {
	var out []Endpoint
	return out, c.do(ctx, http.MethodGet, "/api/endpoints", nil, &out)
}

// Stacks lists every stack Portainer knows.
func (c *Client) Stacks(ctx context.Context) ([]Stack, error) {
	var out []Stack
	return out, c.do(ctx, http.MethodGet, "/api/stacks", nil, &out)
}

// StackFile returns a stack's compose text exactly as stored.
func (c *Client) StackFile(ctx context.Context, id int) (string, error) {
	var out struct {
		StackFileContent string `json:"StackFileContent"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/stacks/"+strconv.Itoa(id)+"/file", nil, &out); err != nil {
		return "", err
	}
	return out.StackFileContent, nil
}
