package venue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParseFormatUnits(t *testing.T) {
	cases := []struct {
		in   string
		dec  uint8
		want string
	}{
		{"100", 6, "100000000"},
		{"1.5", 9, "1500000000"},
		{"0.25", 6, "250000"},
	}
	for _, c := range cases {
		n, err := ParseUnits(c.in, c.dec)
		if err != nil {
			t.Fatalf("ParseUnits(%q): %v", c.in, err)
		}
		if n.String() != c.want {
			t.Errorf("ParseUnits(%q) = %s, want %s", c.in, n, c.want)
		}
		if got := FormatUnits(n, c.dec); got != c.in {
			t.Errorf("FormatUnits round trip %q -> %q", c.in, got)
		}
	}
	if n, err := ParseUnits(".5", 6); err != nil || n.String() != "500000" {
		t.Errorf("ParseUnits(\".5\") = %v, %v", n, err)
	}
	for _, bad := range []string{"", "0", "-1", "abc", "1.1234567"} {
		if _, err := ParseUnits(bad, 6); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("ParseUnits(%q) err = %v, want ErrInvalidAmount", bad, err)
		}
	}
}

func TestJupiterQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/quote" ||
			q.Get("inputMint") != "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" ||
			q.Get("outputMint") != "So11111111111111111111111111111111111111112" ||
			q.Get("amount") != "100000000" || q.Get("slippageBps") != "50" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		w.Write([]byte(`{"inAmount":"100000000","outAmount":"837083774","otherAmountThreshold":"832898356","slippageBps":50,"priceImpactPct":"0.0123","platformFee":{"amount":"1000","feeBps":20},"routePlan":[{"swapInfo":{"label":"Quantum"}}]}`))
	}))
	defer srv.Close()

	j := NewJupiter(WithJupiterBaseURL(srv.URL))
	quote, err := j.Quote(context.Background(), QuoteRequest{From: "USDC", To: "SOL", Amount: "100"})
	if err != nil {
		t.Fatal(err)
	}
	if quote.Out() != "0.837083774" || quote.MinOut() != "0.832898356" {
		t.Errorf("out=%s min=%s", quote.Out(), quote.MinOut())
	}
	if quote.PriceImpactPercent() != "1.2300" || quote.PlatformFee.String() != "1000" || quote.PlatformFeeBps != 20 {
		t.Errorf("impact=%s%% platformFee=%v bps=%d", quote.PriceImpactPercent(), quote.PlatformFee, quote.PlatformFeeBps)
	}
	if len(quote.Route) != 1 || quote.Route[0] != "Quantum" || len(quote.Raw) == 0 {
		t.Errorf("route=%v raw=%d", quote.Route, len(quote.Raw))
	}
	t.Log(quote)
}

func TestJupiterUnknownToken(t *testing.T) {
	_, err := NewJupiter().Quote(context.Background(), QuoteRequest{From: "NOPE", To: "SOL", Amount: "1"})
	if !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("err = %v, want ErrUnknownToken", err)
	}
}

func TestJupiterSameToken(t *testing.T) {
	_, err := NewJupiter().Quote(context.Background(), QuoteRequest{From: "sol", To: "SOL", Amount: "1"})
	if !errors.Is(err, ErrSameToken) {
		t.Fatalf("err = %v, want ErrSameToken", err)
	}
}

func TestJupiterAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := NewJupiter(WithJupiterBaseURL(srv.URL)).Quote(context.Background(), QuoteRequest{From: "USDC", To: "SOL", Amount: "1"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want *APIError 429", err)
	}
}

// Hits the real API; run with JUPITER_LIVE=1 go test ./internal/venue -run Live -v
func TestJupiterQuoteLive(t *testing.T) {
	if os.Getenv("JUPITER_LIVE") == "" {
		t.Skip("set JUPITER_LIVE=1")
	}
	j := NewJupiter(WithJupiterAPIKey(os.Getenv("JUPITER_API_KEY")))
	quote, err := j.Quote(context.Background(), QuoteRequest{From: "USDC", To: "SOL", Amount: "100"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(quote)
}

func TestJupiterUSDPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("ids"), "So11111111111111111111111111111111111111112") {
			t.Errorf("ids = %q", r.URL.Query().Get("ids"))
		}
		w.Write([]byte(`{"So11111111111111111111111111111111111111112":{"usdPrice":119.3},"EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm":null}`))
	}))
	defer srv.Close()
	reg := NewTokenRegistry(SolanaTokens...)
	sol, _ := reg.Lookup("SOL")
	wif, _ := reg.Lookup("WIF")
	prices, err := NewJupiter(WithJupiterPriceURL(srv.URL)).USDPrices(context.Background(), []Token{sol, wif})
	if err != nil || prices["SOL"] != 119.3 || len(prices) != 1 {
		t.Errorf("prices = %v err = %v", prices, err)
	}
}
