package wporg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	ThemeApiUrl  = "https://api.wordpress.org/themes/info/1.2/?action=theme_information"
	PluginApiUrl = "https://api.wordpress.org/plugins/info/1.2/?action=plugin_information"

	themeRequestParams  = "&fields%5Bsections%5D=0"
	pluginRequestParams = "&fields%5Bsections%5D=0&fields%5Btags%5D=0"

	themeApiUrlWithParams  = ThemeApiUrl + themeRequestParams
	pluginApiUrlWithParams = PluginApiUrl + pluginRequestParams
)

var (
	nullBytes = []byte("null")

	ErrNotFound = errors.New("package not found")
)

type FlexString string

func (fs *FlexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, (*string)(fs))
	}

	if bytes.Equal(b, nullBytes) {
		*fs = ""
		return nil
	}

	*fs = FlexString(b)

	return nil
}

type Info struct {
	Slug        string     `json:"slug"`
	Version     FlexString `json:"version"`
	Error       string     `json:"error"`
	Description string     `json:"description"`
	ClosedDate  string     `json:"closed_date"`
}

type Client struct {
	http       *http.Client
	sem        chan struct{}
	maxRetries int
	retryDelay time.Duration
}

type Options struct {
	Timeout     time.Duration
	MaxRetries  int
	RetryDelay  time.Duration
	Concurrency int
}

type Option func(*Options)

func WithTimeout(d time.Duration) Option    { return func(o *Options) { o.Timeout = d } }
func WithRetries(r int) Option              { return func(o *Options) { o.MaxRetries = r } }
func WithRetryDelay(d time.Duration) Option { return func(o *Options) { o.RetryDelay = d } }
func WithConcurrency(c int) Option          { return func(o *Options) { o.Concurrency = c } }

func New(opts ...Option) *Client {
	config := Options{
		Timeout:     10 * time.Second,
		MaxRetries:  3,
		RetryDelay:  500 * time.Millisecond,
		Concurrency: 100,
	}

	for _, opt := range opts {
		opt(&config)
	}

	transport := &http.Transport{
		MaxIdleConns:          config.Concurrency,
		MaxIdleConnsPerHost:   config.Concurrency,
		MaxConnsPerHost:       config.Concurrency,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableKeepAlives:     false,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   config.Timeout,
		},
		retryDelay: config.RetryDelay,
		maxRetries: config.MaxRetries,
		sem:        make(chan struct{}, config.Concurrency),
	}
}

func (c *Client) FetchThemeInfo(ctx context.Context, slug string) (*Info, error) {
	return c.doRequest(ctx, themeApiUrlWithParams+"&request%5Bslug%5D="+url.QueryEscape(slug))
}

func (c *Client) FetchPluginInfo(ctx context.Context, slug string) (*Info, error) {
	return c.doRequest(ctx, pluginApiUrlWithParams+"&request%5Bslug%5D="+url.QueryEscape(slug))
}

func (c *Client) doRequest(ctx context.Context, reqURL string) (*Info, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.retryDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("fetching %s: %w", reqURL, err)
			continue
		}

		info, err := c.processResponse(resp)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, err
			}

			lastErr = err
			continue
		}

		return info, nil
	}

	return nil, fmt.Errorf("all %d attempts failed: %w", c.maxRetries, lastErr)
}

func (c *Client) processResponse(resp *http.Response) (*Info, error) {
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}

		return nil, fmt.Errorf("parsing JSON response: %w", err)
	}

	if info.Error != "" {
		return &info, nil
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}

	return &info, nil
}
