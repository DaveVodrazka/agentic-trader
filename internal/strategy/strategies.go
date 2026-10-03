package strategy

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"agentic-trader/internal/venue"
)

var registry = []Spec{
	{
		Name:     "cash",
		Summary:  "Hold everything in USDC (plus the SOL fee reserve). Defensive: crashes, no edge.",
		Defaults: func() any { return &struct{}{} },
		build:    func(any, *venue.TokenRegistry) (Strategy, error) { return cash{}, nil },
	},
	{
		Name:     "hold",
		Summary:  "Buy a fixed basket once and never trade again. Benchmark; strong bull markets.",
		Defaults: func() any { return &holdParams{Weights: map[string]float64{"SOL": 1}} },
		build: func(p any, t *venue.TokenRegistry) (Strategy, error) {
			hp := p.(*holdParams)
			return hold{hp.Weights}, checkWeights(hp.Weights, t)
		},
	},
	{
		Name:     "rebalance",
		Summary:  "Keep fixed weights; trade back to target when a weight drifts more than band. Sideways, volatile markets.",
		Defaults: func() any { return &rebalanceParams{Weights: map[string]float64{"SOL": 0.5}, Band: 0.05} },
		build: func(p any, t *venue.TokenRegistry) (Strategy, error) {
			rp := p.(*rebalanceParams)
			if err := checkWeights(rp.Weights, t); err != nil {
				return nil, err
			}
			return rebalance{*rp}, checkRange("band", rp.Band, 0.01, 0.5)
		},
	},
	{
		Name: "trend",
		Summary: "Per token: hold its weight while price > slow MA and fast MA > slow MA, else cash. " +
			"Trending markets; whipsaws in ranges.",
		Defaults: func() any {
			return &trendParams{Weights: map[string]float64{"SOL": 0.6}, Fast: 20, Slow: 50, Interval: "1h"}
		},
		build: func(p any, t *venue.TokenRegistry) (Strategy, error) {
			tp := p.(*trendParams)
			if err := checkWeights(tp.Weights, t); err != nil {
				return nil, err
			}
			if err := checkInterval(tp.Interval); err != nil {
				return nil, err
			}
			if tp.Fast < 2 || tp.Slow > 500 || tp.Fast >= tp.Slow {
				return nil, fmt.Errorf("need 2 <= fast < slow <= 500")
			}
			return trend{*tp}, nil
		},
	},
	{
		Name: "momentum",
		Summary: "Every rebalance_hours, hold the top n tokens by lookback_days return (only if positive), " +
			"equal weight capped at max_weight. Markets with clear leaders.",
		Defaults: func() any {
			return &momentumParams{Universe: []string{"SOL", "JUP", "WIF", "BONK", "PUMP"}, LookbackDays: 7, N: 2,
				RebalanceHours: 24, MaxWeight: 0.4}
		},
		build: func(p any, t *venue.TokenRegistry) (Strategy, error) {
			mp := p.(*momentumParams)
			if err := checkTokens(mp.Universe, t); err != nil {
				return nil, err
			}
			if mp.N < 1 || mp.N > len(mp.Universe) {
				return nil, fmt.Errorf("n must be between 1 and the universe size")
			}
			if mp.LookbackDays < 1 || mp.LookbackDays > 90 {
				return nil, fmt.Errorf("lookback_days must be between 1 and 90")
			}
			if mp.RebalanceHours < 1 || mp.RebalanceHours > 24*30 {
				return nil, fmt.Errorf("rebalance_hours must be between 1 and 720")
			}
			return momentum{*mp}, checkRange("max_weight", mp.MaxWeight, 0.05, 1)
		},
	},
	{
		Name: "voltarget",
		Summary: "Hold token at weight = target_vol / realized volatility, capped at max_weight. " +
			"Steady risk in any market: more exposure when calm, less when wild.",
		Defaults: func() any {
			return &volParams{Token: "SOL", TargetVol: 0.4, WindowDays: 14, MaxWeight: 0.8, Band: 0.05}
		},
		build: func(p any, t *venue.TokenRegistry) (Strategy, error) {
			vp := p.(*volParams)
			if err := checkTokens([]string{vp.Token}, t); err != nil {
				return nil, err
			}
			if vp.WindowDays < 5 || vp.WindowDays > 120 {
				return nil, fmt.Errorf("window_days must be between 5 and 120")
			}
			for _, err := range []error{
				checkRange("target_vol", vp.TargetVol, 0.05, 3),
				checkRange("max_weight", vp.MaxWeight, 0.05, 1),
				checkRange("band", vp.Band, 0.01, 0.5),
			} {
				if err != nil {
					return nil, err
				}
			}
			return voltarget{*vp}, nil
		},
	},
}

// ---- cash ------------------------------------------------------------------

type cash struct{}

func (cash) Decide(Input) (Decision, error) {
	return Decision{Weights: map[string]float64{}, Note: "all cash"}, nil
}

// ---- hold ------------------------------------------------------------------

type holdParams struct {
	Weights map[string]float64 `json:"weights"`
}

type hold struct{ weights map[string]float64 }

func (h hold) Decide(in Input) (Decision, error) {
	if in.Memory["bought"] != "" {
		return Decision{Hold: true, Note: "holding since " + in.Memory["bought"]}, nil
	}
	in.Memory["bought"] = in.Now.UTC().Format(time.RFC3339)
	return Decision{Weights: h.weights, Note: "initial buy " + fmtWeights(h.weights)}, nil
}

// ---- rebalance -------------------------------------------------------------

type rebalanceParams struct {
	Weights map[string]float64 `json:"weights"`
	Band    float64            `json:"band"`
}

type rebalance struct{ p rebalanceParams }

func (r rebalance) Decide(Input) (Decision, error) {
	return Decision{Weights: r.p.Weights, Band: r.p.Band, Note: "target " + fmtWeights(r.p.Weights)}, nil
}

// ---- trend -----------------------------------------------------------------

type trendParams struct {
	Weights  map[string]float64 `json:"weights"`
	Fast     int                `json:"fast"`
	Slow     int                `json:"slow"`
	Interval string             `json:"interval"`
}

type trend struct{ p trendParams }

func (t trend) Decide(in Input) (Decision, error) {
	w := map[string]float64{}
	var notes []string
	for _, sym := range sortedKeys(t.p.Weights) {
		closes := in.Data.Closes(sym, t.p.Interval, t.p.Slow)
		if len(closes) < t.p.Slow {
			notes = append(notes, fmt.Sprintf("%s: %d/%d bars, cash", sym, len(closes), t.p.Slow))
			continue
		}
		last, fast, slow := closes[len(closes)-1], sma(closes, t.p.Fast), sma(closes, t.p.Slow)
		if last > slow && fast > slow {
			w[sym] = t.p.Weights[sym]
			notes = append(notes, fmt.Sprintf("%s up (%.4g > MA%d %.4g)", sym, last, t.p.Slow, slow))
		} else {
			notes = append(notes, fmt.Sprintf("%s down (%.4g, MA%d %.4g, MA%d %.4g)", sym, last, t.p.Fast, fast, t.p.Slow, slow))
		}
	}
	return Decision{Weights: w, Band: 0.05, Note: strings.Join(notes, "; ")}, nil
}

// ---- momentum --------------------------------------------------------------

type momentumParams struct {
	Universe       []string `json:"universe"`
	LookbackDays   int      `json:"lookback_days"`
	N              int      `json:"n"`
	RebalanceHours int      `json:"rebalance_hours"`
	MaxWeight      float64  `json:"max_weight"`
}

type momentum struct{ p momentumParams }

func (m momentum) Decide(in Input) (Decision, error) {
	if last, err := time.Parse(time.RFC3339, in.Memory["last_rebalance"]); err == nil &&
		in.Now.Sub(last) < time.Duration(m.p.RebalanceHours)*time.Hour {
		return Decision{Hold: true, Note: "next rebalance " + last.Add(time.Duration(m.p.RebalanceHours)*time.Hour).Format(time.RFC3339)}, nil
	}
	type ranked struct {
		sym string
		ret float64
	}
	var rs []ranked
	var missing []string
	for _, sym := range m.p.Universe {
		closes := in.Data.Closes(sym, "1d", m.p.LookbackDays+1)
		if len(closes) < m.p.LookbackDays+1 || closes[0] <= 0 {
			missing = append(missing, sym)
			continue
		}
		rs = append(rs, ranked{sym, closes[len(closes)-1]/closes[0] - 1})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].ret > rs[j].ret })

	w := map[string]float64{}
	each := math.Min(m.p.MaxWeight, 1/float64(m.p.N))
	var notes []string
	for i, r := range rs {
		pick := i < m.p.N && r.ret > 0
		if pick {
			w[r.sym] = each
		}
		notes = append(notes, fmt.Sprintf("%s %+.1f%%%s", r.sym, r.ret*100, map[bool]string{true: "*", false: ""}[pick]))
	}
	if len(missing) > 0 {
		notes = append(notes, "no data: "+strings.Join(missing, ","))
	}
	in.Memory["last_rebalance"] = in.Now.UTC().Format(time.RFC3339)
	return Decision{Weights: w, Note: fmt.Sprintf("%dd returns: %s", m.p.LookbackDays, strings.Join(notes, ", "))}, nil
}

// ---- voltarget -------------------------------------------------------------

type volParams struct {
	Token      string  `json:"token"`
	TargetVol  float64 `json:"target_vol"`
	WindowDays int     `json:"window_days"`
	MaxWeight  float64 `json:"max_weight"`
	Band       float64 `json:"band"`
}

type voltarget struct{ p volParams }

func (v voltarget) Decide(in Input) (Decision, error) {
	closes := in.Data.Closes(v.p.Token, "1d", v.p.WindowDays+1)
	if len(closes) < v.p.WindowDays+1 {
		return Decision{Weights: map[string]float64{}, Note: fmt.Sprintf("%s: %d/%d daily bars, cash", v.p.Token, len(closes), v.p.WindowDays+1)}, nil
	}
	vol := annualizedVol(closes, 24*time.Hour)
	if math.IsNaN(vol) || vol <= 0 {
		return Decision{Weights: map[string]float64{}, Note: "volatility unavailable, cash"}, nil
	}
	w := math.Min(v.p.MaxWeight, v.p.TargetVol/vol)
	return Decision{
		Weights: map[string]float64{v.p.Token: w},
		Band:    v.p.Band,
		Note:    fmt.Sprintf("%s vol %.0f%% -> weight %.0f%%", v.p.Token, vol*100, w*100),
	}, nil
}

// ---- helpers -----------------------------------------------------------------

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func fmtWeights(w map[string]float64) string {
	if len(w) == 0 {
		return "all cash"
	}
	parts := make([]string, 0, len(w))
	for _, k := range sortedKeys(w) {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", k, w[k]*100))
	}
	return strings.Join(parts, ", ")
}
