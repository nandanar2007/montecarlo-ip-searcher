package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Leo-Mu/montecarlo-ip-searcher/internal/output"
	"github.com/Leo-Mu/montecarlo-ip-searcher/internal/probe"
	"github.com/Leo-Mu/montecarlo-ip-searcher/internal/search"
)

type repeatStringFlag []string

func (r *repeatStringFlag) String() string { return strings.Join(*r, ",") }
func (r *repeatStringFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func main() {
	var (
		cidrs     repeatStringFlag
		cidrFile  string
		budget    int
		topN      int
		concur    int
		heads     int
		beam      int
		timeout   time.Duration
		host      string
		sni       string
		hostHdr   string
		path      string
		dlTop     int
		dlBytes   int64
		dlTimeout time.Duration
		outFmt    string
		outPath   string
		splitV4   int
		splitV6   int
		minSplit  int
		maxBitsV4 int
		maxBitsV6 int
		seed      int64
		verbose   bool
	)

	flag.Var(&cidrs, "cidr", "CIDR to search (repeatable). Example: 1.1.0.0/16 or 2606:4700::/32")
	flag.StringVar(&cidrFile, "cidr-file", "", "Path to a file containing CIDRs (one per line, # comment supported)")
	flag.IntVar(&budget, "budget", 2000, "Total probe budget (number of IPs to probe)")
	flag.IntVar(&topN, "top", 20, "Top N IPs to output")
	flag.IntVar(&concur, "concurrency", 200, "Probe concurrency")
	flag.IntVar(&heads, "heads", 4, "Number of search heads (diversification)")
	flag.IntVar(&beam, "beam", 32, "Beam width per head (kept candidate prefixes)")
	flag.DurationVar(&timeout, "timeout", 3*time.Second, "Per-probe timeout")
	flag.StringVar(&host, "host", "example.com", "Host name used for BOTH TLS SNI and HTTP Host header (recommended)")
	flag.StringVar(&sni, "sni", "", "TLS SNI server name (deprecated: use --host)")
	flag.StringVar(&hostHdr, "host-header", "", "HTTP Host header (deprecated: use --host)")
	flag.StringVar(&path, "path", "/cdn-cgi/trace", "HTTP path to request")
	flag.IntVar(&dlTop, "download-top", 5, "After search, run download speed test for top N IPs (0 to disable)")
	flag.Int64Var(&dlBytes, "download-bytes", 50_000_000, "Download test size in bytes (speed.cloudflare.com/__down?bytes=...)")
	flag.DurationVar(&dlTimeout, "download-timeout", 45*time.Second, "Per-IP download test timeout")
	flag.StringVar(&outFmt, "out", "jsonl", "Output format: jsonl|csv|text")
	flag.StringVar(&outPath, "out-file", "", "Write output to file (default: stdout)")
	flag.IntVar(&splitV4, "split-step-v4", 2, "When splitting an IPv4 prefix, increase prefix bits by this step")
	flag.IntVar(&splitV6, "split-step-v6", 4, "When splitting an IPv6 prefix, increase prefix bits by this step")
	flag.IntVar(&minSplit, "min-samples-split", 20, "Minimum samples on a prefix before it can be split")
	flag.IntVar(&maxBitsV4, "max-bits-v4", 24, "Maximum IPv4 prefix bits to drill down to")
	flag.IntVar(&maxBitsV6, "max-bits-v6", 56, "Maximum IPv6 prefix bits to drill down to")
	flag.Int64Var(&seed, "seed", 0, "Random seed (0 = time-based)")
	flag.BoolVar(&verbose, "v", false, "Verbose progress to stderr")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Unify host: by default use --host for both SNI and Host header.
	// Old flags are still supported and override independently if provided.
	if sni == "" {
		sni = host
	}
	if hostHdr == "" {
		hostHdr = host
	}

	cfg := search.Config{
		Budget:          budget,
		TopN:            topN,
		Concurrency:     concur,
		Heads:           heads,
		Beam:            beam,
		SplitStepV4:     splitV4,
		SplitStepV6:     splitV6,
		MinSamplesSplit: minSplit,
		MaxBitsV4:       maxBitsV4,
		MaxBitsV6:       maxBitsV6,
		Seed:            seed,
		Verbose:         verbose,
	}

	probeCfg := probe.Config{
		Timeout:    timeout,
		SNI:        sni,
		HostHeader: hostHdr,
		Path:       path,
	}

	req := search.Request{
		CIDRs:    []string(cidrs),
		CIDRFile: cidrFile,
		Probe:    probeCfg,
	}

	res, err := search.Run(ctx, cfg, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if dlTop < 0 {
		dlTop = 0
	}
	if dlTop > 0 && dlBytes > 0 {
		if dlTop > len(res.Top) {
			dlTop = len(res.Top)
		}
		dlp := probe.NewDownloadProber(probe.DownloadConfig{
			Timeout:  dlTimeout,
			Bytes:    dlBytes,
			SNI:      "speed.cloudflare.com",
			HostName: "speed.cloudflare.com",
			Path:     "/__down",
		})
		for i := 0; i < dlTop; i++ {
			r := &res.Top[i]
			dctx, cancel := context.WithTimeout(ctx, dlTimeout)
			dr := dlp.Download(dctx, r.IP)
			cancel()
			r.DownloadOK = dr.OK
			r.DownloadBytes = dr.Bytes
			r.DownloadMS = dr.TotalMS
			r.DownloadMbps = dr.Mbps
			r.DownloadError = dr.Error
			if verbose {
				fmt.Fprintf(os.Stderr, "download: rank=%d ip=%s ok=%v mbps=%.2f ms=%d bytes=%d err=%s\n",
					i+1, r.IP.String(), dr.OK, dr.Mbps, dr.TotalMS, dr.Bytes, dr.Error)
			}
		}
	}

	var w *os.File = os.Stdout
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		defer func() {
			_ = f.Close()
		}()
		w = f
	}

	switch outFmt {
	case "jsonl":
		if err := output.WriteJSONL(w, res.Top); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "csv":
		if err := output.WriteCSV(w, res.Top); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "text":
		if err := output.WriteText(w, res.Top); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "debug":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	default:
		fmt.Fprintln(os.Stderr, "error: unknown -out:", outFmt)
		os.Exit(1)
	}
}
