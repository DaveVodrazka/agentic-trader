package pnl

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"agentic-trader/internal/trading"
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

func TestCompute(t *testing.T) {
	tokens := venue.NewTokenRegistry(venue.SolanaTokens...)
	v := priceVenue{tokens, map[string]string{"SOL": "120", "WIF": "0.25"}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	initial := &InitialWallet{StartedAt: start, ValueUSDC: "1000", Balances: map[string]string{"USDC": "1000"}}
	balances := map[string]string{"USDC": "500", "SOL": "4.5", "WIF": "400", "BONK": "0", "PUMP": "10"}
	trades := []trading.TradeRecord{
		{From: "USDC", To: "SOL", In: "400", Out: "3.35", At: start.Add(time.Hour)},
		{From: "USDC", To: "WIF", In: "100", Out: "400", At: start.Add(2 * time.Hour),
			Fees: &trading.FeeRecord{NetworkSOL: "0.000105", NetworkUSDC: "0.0126", ImpactUSDC: "0.5"}},
	}

	// Half a year later: 500 + 540 + 100 = 1140, PUMP unpriced.
	r, err := Compute(context.Background(), v, initial, balances, trades, start.Add(year/2))
	if err != nil {
		t.Fatal(err)
	}
	if r.CurrentValue.FloatString(2) != "1140.00" || r.PnL.FloatString(2) != "140.00" {
		t.Errorf("value=%s pnl=%s", r.CurrentValue.FloatString(2), r.PnL.FloatString(2))
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
}
