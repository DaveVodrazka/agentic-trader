package pnl

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"agentic-trader/internal/store"
	"agentic-trader/internal/venue"
)

// priceVenue sells tokens into USDC at fixed prices.
type priceVenue struct {
	tokens *venue.TokenRegistry
	prices map[string]string // symbol -> USDC per unit
}

func (priceVenue) Name() string { return "fixed" }

func (p priceVenue) Quote(_ context.Context, r venue.QuoteRequest) (*venue.Quote, error) {
	price, ok := p.prices[r.From]
	if !ok {
		return nil, errors.New("no route")
	}
	from, _ := p.tokens.Lookup(r.From)
	to, _ := p.tokens.Lookup(r.To)
	amt, _ := new(big.Rat).SetString(r.Amount)
	px, _ := new(big.Rat).SetString(price)
	out := new(big.Rat).Mul(amt, px)
	out.Mul(out, new(big.Rat).SetInt(pow10(to.Decimals)))
	n := new(big.Int).Quo(out.Num(), out.Denom())
	in, _ := venue.ParseUnits(r.Amount, from.Decimals)
	return &venue.Quote{From: from, To: to, InAmount: in, OutAmount: n}, nil
}

func units(s string, d uint8) *big.Int {
	n, _ := venue.ParseUnits(s, d)
	return n
}

func TestComputeAndSnapshot(t *testing.T) {
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	v := priceVenue{tokens, map[string]string{"SOL": "120", "WIF": "0.25"}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deposits := []store.Deposit{{At: start, Symbol: "USDC", Amount: units("1000", 6), ValueUSDC: "1000"}}
	balances := map[string]*big.Int{"USDC": units("500", 6), "SOL": units("4.5", 9), "WIF": units("400", 6), "PUMP": units("10", 6)}
	trades := []store.Trade{
		{From: "USDC", To: "SOL", InAmount: units("400", 6), OutAmount: units("3.35", 9), At: start.Add(time.Hour)},
		{From: "USDC", To: "WIF", InAmount: units("100", 6), OutAmount: units("400", 6), At: start.Add(2 * time.Hour),
			Fees: &store.TradeFees{NetworkLamports: 105000, NetworkUSDC: "0.0126", ImpactUSDC: "0.5"}},
	}

	// Half a year later: 500 + 540 + 100 = 1140, PUMP unpriced.
	val := Value(context.Background(), v, tokens, balances, start.Add(year/2))
	r := Compute(deposits, val, trades, tokens)

	if r.Total.FloatString(2) != "1140.00" || r.PnL.FloatString(2) != "140.00" {
		t.Errorf("value=%s pnl=%s", r.Total.FloatString(2), r.PnL.FloatString(2))
	}
	if math.Abs(r.ReturnPct-14) > 1e-9 || math.Abs(r.APR-28) > 1e-9 || math.Abs(r.APY-(1.14*1.14-1)*100) > 1e-9 {
		t.Errorf("return=%v apr=%v apy=%v", r.ReturnPct, r.APR, r.APY)
	}
	if r.Unvalued != 1 || r.Holdings[0].Symbol != "SOL" || len(r.Holdings) != 4 {
		t.Errorf("unvalued=%d holdings=%+v", r.Unvalued, r.Holdings)
	}
	if math.Abs(r.CashPct-500.0/1140) > 1e-9 {
		t.Errorf("cash=%v", r.CashPct)
	}
	if c := r.Costs; c.Tracked != 1 || c.Untracked != 1 || c.Total().FloatString(4) != "0.5126" || c.NetworkSOL.FloatString(6) != "0.000105" {
		t.Errorf("costs = %+v total=%s", c, c.Total().FloatString(4))
	}
	if r.GrossPnL.FloatString(4) != "140.5126" {
		t.Errorf("gross = %s", r.GrossPnL.FloatString(4))
	}
	if r.Trades.Count != 2 || r.Trades.VolumeUSDC.FloatString(0) != "500" || r.Trades.ByPair["USDC->SOL"] != 1 {
		t.Errorf("trades=%+v", r.Trades)
	}

	r.Runs = 7
	snap := r.Snapshot(store.KindRun, "run-1")
	if snap.Complete || snap.ValueUSDC != "1140.000000" || snap.PnLUSDC != "140.000000" || snap.ReturnPct != "14.0000" ||
		snap.APRPct != "28.0000" || snap.CostsUSDC != "0.512600" || snap.TradeCount != 2 || snap.RunCount != 7 || len(snap.Holdings) != 4 {
		t.Errorf("snapshot = %+v", snap)
	}
	sol := snap.Holdings[0]
	if sol.Symbol != "SOL" || sol.PriceUSDC != "120.000000000000" || sol.Amount.String() != "4500000000" {
		t.Errorf("SOL holding = %+v", sol)
	}
	if pump := snap.Holdings[3]; pump.Symbol != "PUMP" || pump.PriceUSDC != "" || pump.Error != "no route" {
		t.Errorf("PUMP holding = %+v", pump)
	}
}

func TestInitialSnapshot(t *testing.T) {
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	start := time.Date(2026, 10, 3, 10, 46, 10, 0, time.UTC)
	snap := InitialSnapshot([]store.Deposit{
		{At: start, Symbol: "USDC", Amount: units("1000", 6), ValueUSDC: "1000"},
		{At: start, Symbol: "SOL", Amount: units("0.05", 9), ValueUSDC: "6"},
	}, tokens)
	if snap.Kind != store.KindInitial || !snap.TakenAt.Equal(start) || snap.ValueUSDC != "1006.000000" || snap.PnLUSDC != "0" || snap.APRPct != "" {
		t.Errorf("snapshot = %+v", snap)
	}
	if h := snap.Holdings[1]; h.Symbol != "SOL" || h.PriceUSDC != "120.000000000000" {
		t.Errorf("SOL = %+v", h)
	}
	if snap.Holdings[0].PriceUSDC != "1.000000000000" || snap.CashPct != "99.4036" {
		t.Errorf("USDC = %+v cash=%s", snap.Holdings[0], snap.CashPct)
	}
}
