package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/trading"
	"agentic-trader/internal/venue"
)

// MinHold is how long a strategy must run before it can be replaced (unless
// forced or halted), to stop strategy churn.
const MinHold = 24 * time.Hour

// ErrTooSoon means the live strategy has not run for MinHold yet.
var ErrTooSoon = errors.New("live strategy has not run long enough to be replaced")

// History windows loaded for live decisions.
var historyWindow = map[string]time.Duration{
	"1h": 600 * time.Hour,
	"1d": 150 * 24 * time.Hour,
}

// PriceSource returns USD prices for tokens.
type PriceSource interface {
	USDPrices(ctx context.Context, tokens []venue.Token) (map[string]float64, error)
}

// Activate validates params and makes strategy name the live one. Unless
// force is set, a healthy live strategy must have run for MinHold first.
func Activate(ctx context.Context, st *store.Store, tokens *venue.TokenRegistry, name string, params json.RawMessage,
	reason, setBy, runID string, now time.Time, force bool) (store.Activation, error) {
	if strings.TrimSpace(reason) == "" {
		return store.Activation{}, errors.New("a reason is required")
	}
	spec, err := Lookup(name)
	if err != nil {
		return store.Activation{}, err
	}
	_, canon, err := spec.New(params, tokens)
	if err != nil {
		return store.Activation{}, err
	}
	live, ok, err := st.LiveStrategy(ctx)
	if err != nil {
		return store.Activation{}, err
	}
	if ok && live.Status == store.StatusActive && !force {
		if ran := now.Sub(live.StartedAt); ran < MinHold {
			return store.Activation{}, fmt.Errorf("%w: %s #%d started %s ago; can switch in %s",
				ErrTooSoon, live.Strategy, live.ID, ran.Round(time.Minute), (MinHold - ran).Round(time.Minute))
		}
	}
	a := store.Activation{Strategy: name, Params: string(canon), Reason: strings.TrimSpace(reason), SetBy: setBy,
		RunID: runID, StartedAt: now}
	if err := st.StartStrategy(ctx, &a); err != nil {
		return store.Activation{}, err
	}
	return a, nil
}

// Runner executes the live strategy, one tick at a time.
type Runner struct {
	Store    *store.Store
	Tokens   *venue.TokenRegistry
	Universe []venue.Token // tokens priced and tradable (non-USDC)
	Prices   PriceSource
	Venue    venue.Venue
	Executor trading.Executor
	Rules    Rules
	Now      func() time.Time
}

// Action is an order and its outcome.
type Action struct {
	Order
	Amount  string `json:"amount,omitempty"` // amount of From sent
	TradeID string `json:"trade_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// TickResult reports what a tick did.
type TickResult struct {
	At         time.Time
	Prices     map[string]float64
	Activation *store.Activation // nil if no strategy is live
	Portfolio  Portfolio
	Plan       Plan
	Actions    []Action
	Skipped    string // why no decision was made, if any
}

// Tick records prices into candles, then (if a strategy is active) decides
// and executes. A failed order is recorded and does not stop the others.
func (r *Runner) Tick(ctx context.Context) (*TickResult, error) {
	now := r.Now()
	res := &TickResult{At: now}

	prices, err := r.Prices.USDPrices(ctx, r.Universe)
	if err != nil {
		return nil, fmt.Errorf("prices: %w", err)
	}
	res.Prices = prices
	if err := r.Store.AddPriceTicks(ctx, now, prices, Intervals, "jupiter"); err != nil {
		return nil, err
	}

	pf, err := r.portfolio(ctx, prices)
	if err != nil {
		return nil, err
	}
	res.Portfolio = pf

	act, ok, err := r.Store.LiveStrategy(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		res.Skipped = "no strategy set"
		return res, nil
	}
	res.Activation = &act
	if act.Status == store.StatusHalted {
		res.Skipped = "strategy halted: " + act.HaltReason
		return res, nil
	}

	spec, err := Lookup(act.Strategy)
	if err != nil {
		return nil, err
	}
	strat, _, err := spec.New(json.RawMessage(act.Params), r.Tokens)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal([]byte(act.State), &state); err != nil {
		return nil, fmt.Errorf("activation %d state: %w", act.ID, err)
	}
	data, err := r.history(ctx, now)
	if err != nil {
		return nil, err
	}

	plan, err := Step(strat, now, data, pf, &state, r.Rules)
	if err != nil {
		return nil, err
	}
	res.Plan = plan
	for _, o := range plan.Orders {
		a := r.execute(ctx, act, o, pf, plan.Note(), &state)
		res.Actions = append(res.Actions, a)
	}

	stateJSON, _ := json.Marshal(state)
	if err := r.Store.SaveStrategyState(ctx, act.ID, string(stateJSON), plan.Halt); err != nil {
		return nil, err
	}
	targets, _ := json.Marshal(plan.Targets)
	actions, _ := json.Marshal(res.Actions)
	if res.Actions == nil {
		actions = []byte("[]")
	}
	err = r.Store.RecordTick(ctx, store.Tick{ActivationID: act.ID, At: now, ValueUSDC: fmt.Sprintf("%.6f", pf.Total),
		Targets: string(targets), Actions: string(actions), Note: plan.Note()})
	return res, err
}

// portfolio values the wallet at the given prices (USDC at 1).
func (r *Runner) portfolio(ctx context.Context, prices map[string]float64) (Portfolio, error) {
	bal, err := r.Store.Balances(ctx)
	if err != nil {
		return Portfolio{}, err
	}
	pf := Portfolio{Values: map[string]float64{}, Units: map[string]float64{}, Prices: map[string]float64{Cash: 1}}
	for sym, p := range prices {
		pf.Prices[sym] = p
	}
	for sym, n := range bal {
		tok, err := r.Tokens.Lookup(sym)
		if err != nil {
			return pf, err
		}
		u, _ := strconv.ParseFloat(venue.FormatUnits(n, tok.Decimals), 64)
		pf.Units[sym] = u
		pf.Values[sym] = u * pf.Prices[sym] // unpriced tokens count as 0
		pf.Total += pf.Values[sym]
	}
	return pf, nil
}

// history loads the candles strategies may need into memory.
func (r *Runner) history(ctx context.Context, now time.Time) (*MemData, error) {
	var all []store.Candle
	for _, tok := range r.Universe {
		for iv, window := range historyWindow {
			cs, err := r.Store.Candles(ctx, tok.Symbol, iv, now.Add(-window), now.Add(time.Second))
			if err != nil {
				return nil, err
			}
			all = append(all, cs...)
		}
	}
	d := NewMemData(all)
	d.Now = now
	return d, nil
}

// execute quotes and executes one order, updating entry prices.
func (r *Runner) execute(ctx context.Context, act store.Activation, o Order, pf Portfolio, note string, state *State) Action {
	a := Action{Order: o}
	sym := o.Token()
	tok, err := r.Tokens.Lookup(sym)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	usdc, _ := r.Tokens.Lookup(Cash)
	bal, err := r.Store.Balances(ctx)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	have := func(t venue.Token) *big.Int {
		if n, ok := bal[t.Symbol]; ok {
			return n
		}
		return new(big.Int)
	}

	var from, to venue.Token
	var amount *big.Int
	if o.From == Cash {
		from, to = usdc, tok
		amount = minInt(toUnits(o.ValueUSD, usdc.Decimals), have(usdc))
	} else {
		from, to = tok, usdc
		amount = have(tok)
		if !o.All {
			amount = minInt(toUnits(o.ValueUSD/pf.Prices[sym], tok.Decimals), amount)
		}
	}
	if amount.Sign() <= 0 {
		a.Error = "nothing to trade"
		return a
	}
	a.Amount = venue.FormatUnits(amount, from.Decimals)

	q, err := r.Venue.Quote(ctx, venue.QuoteRequest{From: from.Symbol, To: to.Symbol, Amount: a.Amount})
	if err != nil {
		a.Error = "quote: " + err.Error()
		return a
	}
	reason := fmt.Sprintf("strategy %s #%d: %s", act.Strategy, act.ID, note)
	if len(reason) > 500 {
		reason = reason[:500] + "…"
	}
	fill, err := r.Executor.Execute(ctx, trading.Order{Quote: q, Reason: reason, ActivationID: act.ID})
	if err != nil {
		a.Error = err.Error()
		return a
	}
	a.TradeID = fill.ID

	// Update the entry price from the actual fill.
	after, err := r.Store.Balances(ctx)
	if err == nil {
		newUnits := 0.0
		if n, ok := after[sym]; ok {
			newUnits, _ = strconv.ParseFloat(venue.FormatUnits(n, tok.Decimals), 64)
		}
		if o.From == Cash {
			got, _ := strconv.ParseFloat(venue.FormatUnits(fill.OutAmount, tok.Decimals), 64)
			spent, _ := strconv.ParseFloat(venue.FormatUnits(fill.InAmount, usdc.Decimals), 64)
			state.RecordFill(sym, got, spent, newUnits)
		} else {
			sold, _ := strconv.ParseFloat(venue.FormatUnits(fill.InAmount, tok.Decimals), 64)
			state.RecordFill(sym, -sold, 0, newUnits)
		}
	}
	return a
}

// toUnits converts a float amount to smallest units, rounding down.
func toUnits(x float64, decimals uint8) *big.Int {
	if x <= 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return new(big.Int)
	}
	f := new(big.Float).SetFloat64(x)
	f.Mul(f, new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	n, _ := f.Int(nil)
	return n
}

func minInt(a, b *big.Int) *big.Int {
	if a.Cmp(b) < 0 {
		return a
	}
	return b
}
