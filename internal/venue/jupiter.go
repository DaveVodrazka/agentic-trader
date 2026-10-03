package venue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Jupiter defaults.
const (
	// DefaultJupiterBaseURL is Jupiter's Swap API v1 endpoint.
	DefaultJupiterBaseURL = "https://api.jup.ag/swap/v1"
	// DefaultJupiterSlippageBps is 0.5%.
	DefaultJupiterSlippageBps = 50
)

// DefaultJupiterPriceURL is Jupiter's Price API v3 endpoint.
const DefaultJupiterPriceURL = "https://api.jup.ag/price/v3"

// Jupiter is a Venue backed by the Jupiter aggregator on Solana.
type Jupiter struct {
	baseURL     string
	priceURL    string
	apiKey      string
	slippageBps uint16
	http        *http.Client
	tokens      *TokenRegistry
}

var _ Venue = (*Jupiter)(nil)

// JupiterOption configures a Jupiter venue.
type JupiterOption func(*Jupiter)

// WithJupiterBaseURL overrides the API base URL (e.g. for tests or lite-api).
func WithJupiterBaseURL(u string) JupiterOption { return func(j *Jupiter) { j.baseURL = u } }

// WithJupiterPriceURL overrides the Price API URL (e.g. for tests).
func WithJupiterPriceURL(u string) JupiterOption { return func(j *Jupiter) { j.priceURL = u } }

// WithJupiterAPIKey sets the x-api-key header (from portal.jup.ag).
func WithJupiterAPIKey(k string) JupiterOption { return func(j *Jupiter) { j.apiKey = k } }

// WithJupiterSlippageBps sets the default slippage for quotes.
func WithJupiterSlippageBps(bps uint16) JupiterOption {
	return func(j *Jupiter) { j.slippageBps = bps }
}

// WithJupiterHTTPClient sets the HTTP client used for API calls.
func WithJupiterHTTPClient(c *http.Client) JupiterOption { return func(j *Jupiter) { j.http = c } }

// WithJupiterTokens replaces the token registry.
func WithJupiterTokens(r *TokenRegistry) JupiterOption { return func(j *Jupiter) { j.tokens = r } }

// NewJupiter returns a Jupiter venue with sensible defaults.
func NewJupiter(opts ...JupiterOption) *Jupiter {
	j := &Jupiter{
		baseURL:     DefaultJupiterBaseURL,
		priceURL:    DefaultJupiterPriceURL,
		slippageBps: DefaultJupiterSlippageBps,
		http:        &http.Client{Timeout: 10 * time.Second},
		tokens:      NewTokenRegistry(SolanaTokens...),
	}
	for _, opt := range opts {
		opt(j)
	}
	return j
}

// Name implements Venue.
func (j *Jupiter) Name() string { return "jupiter" }

// Tokens exposes the registry so callers can register additional mints.
func (j *Jupiter) Tokens() *TokenRegistry { return j.tokens }

// jupiterQuote mirrors the fields of /quote we use.
type jupiterQuote struct {
	InAmount             string `json:"inAmount"`
	OutAmount            string `json:"outAmount"`
	OtherAmountThreshold string `json:"otherAmountThreshold"`
	SlippageBps          uint16 `json:"slippageBps"`
	// PriceImpactPct is misnamed upstream: it is a fraction (0.01 = 1%).
	PriceImpactPct string `json:"priceImpactPct"`
	PlatformFee    *struct {
		Amount string `json:"amount"`
		FeeBps uint16 `json:"feeBps"`
	} `json:"platformFee"`
	RoutePlan []struct {
		SwapInfo struct {
			Label string `json:"label"`
		} `json:"swapInfo"`
	} `json:"routePlan"`
}

// Quote implements Venue. It prices an exact-in swap of req.Amount of req.From into req.To.
func (j *Jupiter) Quote(ctx context.Context, req QuoteRequest) (*Quote, error) {
	from, err := j.tokens.Lookup(req.From)
	if err != nil {
		return nil, err
	}
	to, err := j.tokens.Lookup(req.To)
	if err != nil {
		return nil, err
	}
	if from.Address == to.Address {
		return nil, fmt.Errorf("%w: %s", ErrSameToken, from.Symbol)
	}
	amount, err := ParseUnits(req.Amount, from.Decimals)
	if err != nil {
		return nil, err
	}
	slippage := req.SlippageBps
	if slippage == 0 {
		slippage = j.slippageBps
	}

	q := url.Values{}
	q.Set("inputMint", from.Address)
	q.Set("outputMint", to.Address)
	q.Set("amount", amount.String())
	q.Set("slippageBps", strconv.Itoa(int(slippage)))
	q.Set("swapMode", "ExactIn")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, j.baseURL+"/quote?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	if j.apiKey != "" {
		httpReq.Header.Set("x-api-key", j.apiKey)
	}

	resp, err := j.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("jupiter quote: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("jupiter quote: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Venue: j.Name(), StatusCode: resp.StatusCode, Body: string(body)}
	}

	var jq jupiterQuote
	if err := json.Unmarshal(body, &jq); err != nil {
		return nil, fmt.Errorf("jupiter quote: decode: %w", err)
	}
	in, ok1 := new(big.Int).SetString(jq.InAmount, 10)
	out, ok2 := new(big.Int).SetString(jq.OutAmount, 10)
	minOut, ok3 := new(big.Int).SetString(jq.OtherAmountThreshold, 10)
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("jupiter quote: malformed amounts in response: %s", body)
	}

	impact, ok := new(big.Rat).SetString(jq.PriceImpactPct)
	if !ok {
		impact = new(big.Rat)
	}
	var platformFee *big.Int
	var platformFeeBps uint16
	if jq.PlatformFee != nil {
		if n, ok := new(big.Int).SetString(jq.PlatformFee.Amount, 10); ok && n.Sign() > 0 {
			platformFee, platformFeeBps = n, jq.PlatformFee.FeeBps
		}
	}

	route := make([]string, 0, len(jq.RoutePlan))
	for _, hop := range jq.RoutePlan {
		route = append(route, hop.SwapInfo.Label)
	}

	return &Quote{
		Venue:          j.Name(),
		From:           from,
		To:             to,
		InAmount:       in,
		OutAmount:      out,
		MinOutAmount:   minOut,
		SlippageBps:    jq.SlippageBps,
		PriceImpact:    impact,
		PlatformFee:    platformFee,
		PlatformFeeBps: platformFeeBps,
		Route:          route,
		FetchedAt:      time.Now(),
		Raw:            json.RawMessage(body),
	}, nil
}

// USDPrices returns the USD price per unit of each token, in one request.
// Tokens Jupiter has no price for are omitted.
func (j *Jupiter) USDPrices(ctx context.Context, tokens []Token) (map[string]float64, error) {
	ids := make([]string, 0, len(tokens))
	bySymbol := make(map[string]string, len(tokens))
	for _, t := range tokens {
		ids = append(ids, t.Address)
		bySymbol[t.Address] = t.Symbol
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.priceURL+"?ids="+url.QueryEscape(strings.Join(ids, ",")), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if j.apiKey != "" {
		req.Header.Set("x-api-key", j.apiKey)
	}
	resp, err := j.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jupiter prices: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("jupiter prices: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Venue: j.Name(), StatusCode: resp.StatusCode, Body: string(body)}
	}
	var raw map[string]*struct {
		USDPrice float64 `json:"usdPrice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("jupiter prices: decode: %w", err)
	}
	out := make(map[string]float64, len(raw))
	for mint, p := range raw {
		if sym, ok := bySymbol[mint]; ok && p != nil && p.USDPrice > 0 {
			out[sym] = p.USDPrice
		}
	}
	return out, nil
}
