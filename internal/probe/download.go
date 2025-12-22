package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

type DownloadConfig struct {
	Timeout time.Duration
	Bytes   int64
	// Fixed for Cloudflare speed test; can be exposed later if needed.
	SNI      string
	HostName string
	Path     string
}

type DownloadResult struct {
	IP      netip.Addr `json:"ip"`
	OK      bool       `json:"ok"`
	Status  int        `json:"status"`
	Error   string     `json:"error,omitempty"`
	Bytes   int64      `json:"bytes"`
	TotalMS int64      `json:"total_ms"`
	Mbps    float64    `json:"mbps"`
	When    time.Time  `json:"when"`
}

type DownloadProber struct {
	cfg    DownloadConfig
	client *http.Client
}

func NewDownloadProber(cfg DownloadConfig) *DownloadProber {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	if cfg.Bytes <= 0 {
		cfg.Bytes = 50_000_000
	}
	if cfg.SNI == "" {
		cfg.SNI = "speed.cloudflare.com"
	}
	if cfg.HostName == "" {
		cfg.HostName = "speed.cloudflare.com"
	}
	if cfg.Path == "" {
		cfg.Path = "/__down"
	}

	transport := &http.Transport{
		Proxy: nil, // critical: ignore HTTP(S)_PROXY and NO_PROXY env vars
		DialContext: (&net.Dialer{
			Timeout:   cfg.Timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			ServerName: cfg.SNI,
		},
	}

	return &DownloadProber{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}
}

func (p *DownloadProber) Download(ctx context.Context, ip netip.Addr) DownloadResult {
	start := time.Now()
	out := DownloadResult{
		IP:   ip,
		When: start,
	}

	host := ip.String()
	if ip.Is6() {
		host = "[" + host + "]"
	}

	// https://speed.cloudflare.com/__down?bytes=50000000
	url := "https://" + host + p.cfg.Path + "?bytes=" + strconv.FormatInt(p.cfg.Bytes, 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		out.Error = err.Error()
		out.TotalMS = time.Since(start).Milliseconds()
		return out
	}
	req.Host = p.cfg.HostName
	req.Header.Set("User-Agent", "mcis/0.1")
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := p.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			out.Error = "timeout"
		} else {
			out.Error = err.Error()
		}
		out.TotalMS = time.Since(start).Milliseconds()
		return out
	}
	defer func() { _ = resp.Body.Close() }()

	out.Status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out.Error = fmt.Sprintf("http_status_%d", resp.StatusCode)
		out.TotalMS = time.Since(start).Milliseconds()
		return out
	}

	// Read exactly cfg.Bytes or until EOF, whichever comes first.
	n, err := io.CopyN(io.Discard, resp.Body, p.cfg.Bytes)
	if err != nil && !errors.Is(err, io.EOF) {
		out.Error = err.Error()
		out.TotalMS = time.Since(start).Milliseconds()
		return out
	}

	elapsed := time.Since(start)
	out.TotalMS = elapsed.Milliseconds()
	// bits per second -> Mbps (10^6)
	if elapsed > 0 {
		out.Mbps = (float64(n) * 8) / elapsed.Seconds() / 1e6
	}
	out.OK = true
	out.Bytes = n
	return out
}
