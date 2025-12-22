package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	mrand "math/rand"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/Leo-Mu/montecarlo-ip-searcher/internal/cidr"
	"github.com/Leo-Mu/montecarlo-ip-searcher/internal/probe"
)

type Config struct {
	Budget          int
	TopN            int
	Concurrency     int
	Heads           int
	Beam            int
	SplitStepV4     int
	SplitStepV6     int
	MinSamplesSplit int
	MaxBitsV4       int
	MaxBitsV6       int
	Seed            int64
	Verbose         bool
}

type Request struct {
	CIDRs    []string
	CIDRFile string
	Probe    probe.Config
}

type TopResult struct {
	IP     netip.Addr   `json:"ip"`
	Prefix netip.Prefix `json:"prefix"`
	OK     bool         `json:"ok"`
	Status int          `json:"status"`
	Error  string       `json:"error,omitempty"`

	ConnectMS int64             `json:"connect_ms"`
	TLSMS     int64             `json:"tls_ms"`
	TTFBMS    int64             `json:"ttfb_ms"`
	TotalMS   int64             `json:"total_ms"`
	ScoreMS   float64           `json:"score_ms"`
	Trace     map[string]string `json:"trace,omitempty"`

	DownloadOK    bool    `json:"download_ok"`
	DownloadBytes int64   `json:"download_bytes"`
	DownloadMS    int64   `json:"download_ms"`
	DownloadMbps  float64 `json:"download_mbps"`
	DownloadError string  `json:"download_error,omitempty"`

	PrefixSamples int `json:"prefix_samples"`
	PrefixOK      int `json:"prefix_ok"`
	PrefixFail    int `json:"prefix_fail"`
}

type Response struct {
	Top []TopResult `json:"top"`
}

type arm struct {
	Pfx netip.Prefix

	// counts
	Samples int
	OK      int
	Fail    int

	// OK totals stats
	MeanOKTotal float64
	M2OKTotal   float64

	Split bool
}

func (a *arm) scoreMS(timeout time.Duration) float64 {
	// Smaller is better.
	failRate := 0.0
	if a.Samples > 0 {
		failRate = float64(a.Fail) / float64(a.Samples)
	}
	mean := a.MeanOKTotal
	if a.OK == 0 {
		mean = float64(timeout.Milliseconds()) * 2
	}
	return mean + failRate*float64(timeout.Milliseconds())
}

func (a *arm) ucbValue(timeout time.Duration, totalSamples int, c float64) float64 {
	// Convert minimization score into maximization reward.
	reward := -a.scoreMS(timeout)
	if a.Samples == 0 {
		return math.Inf(1)
	}
	return reward + c*math.Sqrt(math.Log(float64(totalSamples+1))/float64(a.Samples))
}

type probeTask struct {
	head int
	pfx  netip.Prefix
	ip   netip.Addr
}

type probeDone struct {
	head int
	pfx  netip.Prefix
	ip   netip.Addr
	res  probe.Result
}

func Run(ctx context.Context, cfg Config, req Request) (Response, error) {
	if cfg.Budget <= 0 {
		return Response{}, fmt.Errorf("budget must be > 0")
	}
	if cfg.TopN <= 0 {
		cfg.TopN = 20
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 200
	}
	if cfg.Heads <= 0 {
		cfg.Heads = 4
	}
	if cfg.Beam <= 0 {
		cfg.Beam = 32
	}
	if cfg.SplitStepV4 <= 0 {
		cfg.SplitStepV4 = 2
	}
	if cfg.SplitStepV6 <= 0 {
		cfg.SplitStepV6 = 4
	}
	if cfg.MinSamplesSplit <= 0 {
		cfg.MinSamplesSplit = 20
	}
	if cfg.MaxBitsV4 <= 0 {
		cfg.MaxBitsV4 = 24
	}
	if cfg.MaxBitsV6 <= 0 {
		cfg.MaxBitsV6 = 56
	}
	if cfg.Seed == 0 {
		cfg.Seed = time.Now().UnixNano()
	}

	prefixes, err := loadPrefixes(req)
	if err != nil {
		return Response{}, err
	}
	if len(prefixes) == 0 {
		return Response{}, errors.New("no cidr provided (use --cidr or --cidr-file)")
	}

	armMu := &sync.Mutex{}
	arms := make(map[string]*arm, len(prefixes))
	for _, p := range prefixes {
		arms[p.String()] = &arm{Pfx: p}
	}

	// Per-head RNG for diversification.
	rngs := make([]*mrand.Rand, cfg.Heads)
	for i := 0; i < cfg.Heads; i++ {
		rngs[i] = mrand.New(mrand.NewSource(cfg.Seed + int64(i*9973)))
	}

	seenIPs := &sync.Map{} // ip.String() -> struct{}

	tasks := make(chan probeTask, cfg.Concurrency*2)
	done := make(chan probeDone, cfg.Concurrency*2)

	// workers
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prober := probe.NewProber(req.Probe)
			for t := range tasks {
				pctx, cancel := context.WithTimeout(ctx, req.Probe.Timeout)
				r := prober.ProbeHTTPTrace(pctx, t.ip)
				cancel()
				done <- probeDone{head: t.head, pfx: t.pfx, ip: t.ip, res: r}
			}
		}()
	}

	// schedule loop
	top := newTopN(cfg.TopN)
	start := time.Now()
	totalSubmitted := 0
	totalCompleted := 0
	ucbC := 2.0

	// per-head beam cache
	beams := make([][]netip.Prefix, cfg.Heads)
	refreshBeams := func() {
		armMu.Lock()
		defer armMu.Unlock()

		list := make([]*arm, 0, len(arms))
		for _, a := range arms {
			list = append(list, a)
		}
		// Sort by (score + jitter) so each head diverges.
		for h := 0; h < cfg.Heads; h++ {
			r := rngs[h]
			sort.Slice(list, func(i, j int) bool {
				ai := list[i].scoreMS(req.Probe.Timeout) * (1 + 0.02*r.NormFloat64())
				aj := list[j].scoreMS(req.Probe.Timeout) * (1 + 0.02*r.NormFloat64())
				return ai < aj
			})
			k := cfg.Beam
			if k > len(list) {
				k = len(list)
			}
			b := make([]netip.Prefix, 0, k)
			for i := 0; i < k; i++ {
				b = append(b, list[i].Pfx)
			}
			beams[h] = b
		}
	}

	refreshBeams()

	// Helper: choose next arm for a head using UCB over its beam.
	chooseArm := func(head int, totalSamples int) netip.Prefix {
		armMu.Lock()
		defer armMu.Unlock()
		cands := beams[head]
		if len(cands) == 0 {
			// fallback: pick any
			for _, a := range arms {
				return a.Pfx
			}
			return netip.Prefix{}
		}
		best := cands[0]
		bestV := math.Inf(-1)
		r := rngs[head]
		for _, p := range cands {
			a := arms[p.String()]
			if a == nil {
				continue
			}
			v := a.ucbValue(req.Probe.Timeout, totalSamples, ucbC)
			// small jitter to avoid head synchronization
			v += 0.05 * r.NormFloat64()
			if v > bestV {
				bestV = v
				best = p
			}
		}
		return best
	}

	// initial fill
	for totalSubmitted < cfg.Budget && totalSubmitted < cfg.Concurrency*2 {
		h := totalSubmitted % cfg.Heads
		p := chooseArm(h, totalCompleted)
		ip := cidr.RandomAddr(p, rngs[h])
		if _, loaded := seenIPs.LoadOrStore(ip.String(), struct{}{}); loaded {
			continue
		}
		tasks <- probeTask{head: h, pfx: p, ip: ip}
		totalSubmitted++
	}

	lastLog := time.Now()
	lastRefresh := time.Now()

	for totalCompleted < cfg.Budget {
		select {
		case <-ctx.Done():
			close(tasks)
			wg.Wait()
			close(done)
			return Response{Top: top.Snapshot()}, ctx.Err()
		case d := <-done:
			totalCompleted++
			updateArm(armMu, arms, d.pfx, d.res, req.Probe.Timeout)

			aCounts := func() (samples, okN, failN int) {
				armMu.Lock()
				defer armMu.Unlock()
				if a := arms[d.pfx.String()]; a != nil {
					return a.Samples, a.OK, a.Fail
				}
				return 0, 0, 0
			}
			samples, okN, failN := aCounts()

			// update top
			score := float64(d.res.TotalMS)
			if !d.res.OK {
				score = float64(req.Probe.Timeout.Milliseconds()) * 2
			}
			top.Consider(TopResult{
				IP:            d.ip,
				Prefix:        d.pfx,
				OK:            d.res.OK,
				Status:        d.res.Status,
				Error:         d.res.Error,
				ConnectMS:     d.res.ConnectMS,
				TLSMS:         d.res.TLSMS,
				TTFBMS:        d.res.TTFBMS,
				TotalMS:       d.res.TotalMS,
				ScoreMS:       score,
				Trace:         d.res.Trace,
				PrefixSamples: samples,
				PrefixOK:      okN,
				PrefixFail:    failN,
			})

			// split decisions + beam refresh
			if time.Since(lastRefresh) > 800*time.Millisecond {
				trySplitTop(cfg, req, armMu, arms)
				refreshBeams()
				lastRefresh = time.Now()
			}

			// submit next
			if totalSubmitted < cfg.Budget {
				h := totalSubmitted % cfg.Heads
				p := chooseArm(h, totalCompleted)
				ip := cidr.RandomAddr(p, rngs[h])
				if _, loaded := seenIPs.LoadOrStore(ip.String(), struct{}{}); loaded {
					break
				}
				tasks <- probeTask{head: h, pfx: p, ip: ip}
				totalSubmitted++
			}

			if cfg.Verbose && time.Since(lastLog) > 1*time.Second {
				best := top.Best()
				elapsed := time.Since(start).Truncate(100 * time.Millisecond)
				fmt.Fprintf(os.Stderr, "progress: %d/%d done, best=%.1fms ip=%s prefix=%s elapsed=%s\n",
					totalCompleted, cfg.Budget, best.ScoreMS, best.IP.String(), best.Prefix.String(), elapsed)
				lastLog = time.Now()
			}
		}
	}

	close(tasks)
	wg.Wait()
	close(done)

	return Response{Top: top.Snapshot()}, nil
}

func loadPrefixes(req Request) ([]netip.Prefix, error) {
	var pfxs []netip.Prefix
	if len(req.CIDRs) > 0 {
		ps, err := cidr.ParseCIDRs(req.CIDRs)
		if err != nil {
			return nil, err
		}
		pfxs = append(pfxs, ps...)
	}
	if req.CIDRFile != "" {
		ps, err := cidr.ReadCIDRsFromFile(req.CIDRFile)
		if err != nil {
			return nil, err
		}
		pfxs = append(pfxs, ps...)
	}
	// Dedup
	m := make(map[string]netip.Prefix, len(pfxs))
	for _, p := range pfxs {
		m[p.String()] = p.Masked()
	}
	out := make([]netip.Prefix, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func updateArm(mu *sync.Mutex, arms map[string]*arm, pfx netip.Prefix, r probe.Result, timeout time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	a := arms[pfx.String()]
	if a == nil {
		a = &arm{Pfx: pfx}
		arms[pfx.String()] = a
	}
	a.Samples++
	if r.OK {
		a.OK++
		// Welford on TotalMS
		x := float64(r.TotalMS)
		delta := x - a.MeanOKTotal
		a.MeanOKTotal += delta / float64(a.OK)
		a.M2OKTotal += delta * (x - a.MeanOKTotal)
	} else {
		a.Fail++
		_ = timeout
	}
}

func trySplitTop(cfg Config, req Request, mu *sync.Mutex, arms map[string]*arm) {
	mu.Lock()
	defer mu.Unlock()

	type cand struct {
		a     *arm
		score float64
	}
	var cands []cand
	for _, a := range arms {
		if a.Split {
			continue
		}
		if a.Samples < cfg.MinSamplesSplit {
			continue
		}
		bits := a.Pfx.Bits()
		if a.Pfx.Addr().Is4() && bits >= cfg.MaxBitsV4 {
			continue
		}
		if a.Pfx.Addr().Is6() && bits >= cfg.MaxBitsV6 {
			continue
		}
		cands = append(cands, cand{a: a, score: a.scoreMS(req.Probe.Timeout)})
	}
	if len(cands) == 0 {
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
	limit := cfg.Heads
	if limit < 1 {
		limit = 1
	}
	if limit > len(cands) {
		limit = len(cands)
	}
	for i := 0; i < limit; i++ {
		a := cands[i].a
		step := cfg.SplitStepV6
		if a.Pfx.Addr().Is4() {
			step = cfg.SplitStepV4
		}
		children, err := cidr.SplitPrefix(a.Pfx, step)
		if err != nil || len(children) == 0 {
			a.Split = true
			continue
		}
		for _, ch := range children {
			if _, ok := arms[ch.String()]; !ok {
				arms[ch.String()] = &arm{Pfx: ch}
			}
		}
		a.Split = true
	}
}
