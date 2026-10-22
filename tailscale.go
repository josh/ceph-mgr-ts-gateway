package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const defaultTailscaledSocket = "/var/run/tailscale/tailscaled.sock"

type tsClient struct {
	http http.Client
}

func newTSClient(socket string) *tsClient {
	if socket == "" {
		socket = defaultTailscaledSocket
	}
	var d net.Dialer
	return &tsClient{http: http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *tsClient) do(ctx context.Context, method, path string, header http.Header, body any) ([]byte, http.Header, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://local-tailscaled.sock"+path, r)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	slurp, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(slurp)))
	}
	return slurp, res.Header, nil
}

func (c *tsClient) magicDNSSuffix(ctx context.Context) (string, error) {
	body, _, err := c.do(ctx, "GET", "/localapi/v0/status?peers=false", nil, nil)
	if err != nil {
		return "", err
	}
	var st struct {
		BackendState   string
		CurrentTailnet *struct{ MagicDNSSuffix string }
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return "", fmt.Errorf("decode status: %w", err)
	}
	if st.CurrentTailnet == nil {
		return "", fmt.Errorf("tailscale is not connected to a tailnet (backend state: %s)", st.BackendState)
	}
	return st.CurrentTailnet.MagicDNSSuffix, nil
}

func (c *tsClient) advertisedServices(ctx context.Context) ([]string, error) {
	body, _, err := c.do(ctx, "GET", "/localapi/v0/prefs", nil, nil)
	if err != nil {
		return nil, err
	}
	var prefs struct{ AdvertiseServices []string }
	if err := json.Unmarshal(body, &prefs); err != nil {
		return nil, fmt.Errorf("decode prefs: %w", err)
	}
	return prefs.AdvertiseServices, nil
}

func (c *tsClient) setAdvertisedServices(ctx context.Context, services []string) error {
	_, _, err := c.do(ctx, "PATCH", "/localapi/v0/prefs", nil, map[string]any{
		"AdvertiseServicesSet": true,
		"AdvertiseServices":    services,
	})
	return err
}

func (c *tsClient) setServiceProxy(ctx context.Context, service, mds string, port uint16, proxyURL string) error {
	body, h, err := c.do(ctx, "GET", "/localapi/v0/serve-config", nil, nil)
	if err != nil {
		return fmt.Errorf("get serve config: %w", err)
	}
	var sc map[string]json.RawMessage
	if err := json.Unmarshal(body, &sc); err != nil {
		return fmt.Errorf("decode serve config: %w", err)
	}
	if sc == nil {
		sc = map[string]json.RawMessage{}
	}
	var services map[string]json.RawMessage
	if raw, ok := sc["Services"]; ok {
		if err := json.Unmarshal(raw, &services); err != nil {
			return fmt.Errorf("decode serve config services: %w", err)
		}
	}
	if services == nil {
		services = map[string]json.RawMessage{}
	}

	ps := strconv.Itoa(int(port))
	hostPort := net.JoinHostPort(strings.TrimPrefix(service, "svc:")+"."+mds, ps)
	entry, err := json.Marshal(map[string]any{
		"TCP": map[string]any{ps: map[string]bool{"HTTPS": true}},
		"Web": map[string]any{hostPort: map[string]any{
			"Handlers": map[string]any{"/": map[string]string{"Proxy": proxyURL}},
		}},
	})
	if err != nil {
		return err
	}
	services[service] = entry
	if sc["Services"], err = json.Marshal(services); err != nil {
		return err
	}

	_, _, err = c.do(ctx, "POST", "/localapi/v0/serve-config", http.Header{"If-Match": {h.Get("Etag")}}, sc)
	return err
}

var serviceNameRe = regexp.MustCompile(`^svc:[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func validServiceName(s string) bool { return serviceNameRe.MatchString(s) }

func expandProxyTarget(target string) (string, error) {
	if _, err := strconv.ParseUint(target, 10, 16); err == nil {
		target = "127.0.0.1:" + target
	}
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid upstream %q", target)
	}
	if !slices.Contains([]string{"http", "https", "https+insecure"}, u.Scheme) {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return u.String(), nil
}
