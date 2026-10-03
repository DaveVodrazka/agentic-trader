package venue

import (
	"fmt"
	"strings"
	"sync"
)

// Token describes an on-chain asset.
type Token struct {
	Symbol   string
	Address  string // mint address on Solana
	Decimals uint8
}

// SolanaTokens is the default registry of well-known Solana mints.
var SolanaTokens = []Token{
	{Symbol: "SOL", Address: "So11111111111111111111111111111111111111112", Decimals: 9},
	{Symbol: "USDC", Address: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Decimals: 6},
	{Symbol: "USDT", Address: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Decimals: 6},
	{Symbol: "JUP", Address: "JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN", Decimals: 6},
	{Symbol: "BONK", Address: "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", Decimals: 5},
	{Symbol: "WIF", Address: "EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", Decimals: 6},
	{Symbol: "PUMP", Address: "pumpCmXqMfrsAkQ5r49WcJnRayYRqmXz6ae8H7H9Dfn", Decimals: 6},
}

// TokenRegistry resolves symbols to tokens. Safe for concurrent use.
type TokenRegistry struct {
	mu       sync.RWMutex
	bySymbol map[string]Token
}

// NewTokenRegistry builds a registry from the given tokens.
func NewTokenRegistry(tokens ...Token) *TokenRegistry {
	r := &TokenRegistry{bySymbol: make(map[string]Token, len(tokens))}
	for _, t := range tokens {
		r.Register(t)
	}
	return r
}

// Register adds or replaces a token. Symbols are case-insensitive.
func (r *TokenRegistry) Register(t Token) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bySymbol[strings.ToUpper(t.Symbol)] = t
}

// Lookup returns the token for symbol.
func (r *TokenRegistry) Lookup(symbol string) (Token, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.bySymbol[strings.ToUpper(symbol)]
	if !ok {
		return Token{}, fmt.Errorf("%w: %q", ErrUnknownToken, symbol)
	}
	return t, nil
}
