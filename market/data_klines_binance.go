package market

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// getKlinesFromBinance fetches klines directly from Binance USD-M Futures
// (fapi.binance.com) — the same venue GINA executes orders on — so the data
// driving a decision and the price it fills at share one source. Unlike
// getKlinesFromCoinAnk there is NO fallback: any transport, decode, or invariant
// failure returns an error so the caller fails closed (skips the symbol) instead
// of trading on suspect data. Reuses the shared 429/418 backoff client.
func getKlinesFromBinance(symbol, interval string, limit int) ([]Kline, error) {
	if limit <= 0 {
		limit = 200
	}
	url := fmt.Sprintf("%s/fapi/v1/klines?symbol=%s&interval=%s&limit=%d", baseURL, symbol, interval, limit)
	body, err := httpGetWithBackoff(NewAPIClient().client, url)
	if err != nil {
		return nil, fmt.Errorf("binance klines %s %s: %w", symbol, interval, err)
	}
	klines, err := parseBinanceKlines(body)
	if err != nil {
		return nil, fmt.Errorf("binance klines %s %s: %w", symbol, interval, err)
	}
	return klines, nil
}

// parseBinanceKlines strictly decodes a /fapi/v1/klines array into an ascending,
// invariant-checked series. Separated from transport so it is unit-testable
// without network access. Returns an error (never a partial / silently-dropped
// series) on any malformed row so callers fail closed.
func parseBinanceKlines(body []byte) ([]Kline, error) {
	var raw []KlineResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty kline array")
	}
	klines := make([]Kline, 0, len(raw))
	for i, kr := range raw {
		k, err := parseBinanceKline(kr)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		klines = append(klines, k)
	}
	// Defensive ordering: ascending by OpenTime, strictly increasing (no dupes).
	sort.Slice(klines, func(i, j int) bool { return klines[i].OpenTime < klines[j].OpenTime })
	for i := 1; i < len(klines); i++ {
		if klines[i].OpenTime <= klines[i-1].OpenTime {
			return nil, fmt.Errorf("non-increasing openTime at row %d", i)
		}
	}
	return klines, nil
}

// parseBinanceKline decodes one /fapi/v1/klines row with checked types and OHLC
// invariants. Field layout: [openTime, open, high, low, close, volume, closeTime, …].
func parseBinanceKline(kr KlineResponse) (Kline, error) {
	var k Kline
	if len(kr) < 7 {
		return k, fmt.Errorf("short row (%d fields)", len(kr))
	}
	openTime, ok := kr[0].(float64)
	if !ok {
		return k, fmt.Errorf("openTime not numeric")
	}
	closeTime, ok := kr[6].(float64)
	if !ok {
		return k, fmt.Errorf("closeTime not numeric")
	}
	open, err := klineFloat(kr[1], "open")
	if err != nil {
		return k, err
	}
	high, err := klineFloat(kr[2], "high")
	if err != nil {
		return k, err
	}
	low, err := klineFloat(kr[3], "low")
	if err != nil {
		return k, err
	}
	closePx, err := klineFloat(kr[4], "close")
	if err != nil {
		return k, err
	}
	volume, err := klineFloat(kr[5], "volume")
	if err != nil {
		return k, err
	}

	// Fail-closed invariants: any impossible value rejects the whole series.
	for _, f := range []struct {
		name string
		v    float64
	}{{"open", open}, {"high", high}, {"low", low}, {"close", closePx}, {"volume", volume}} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			return k, fmt.Errorf("non-finite %s", f.name)
		}
	}
	if open <= 0 || high <= 0 || low <= 0 || closePx <= 0 {
		return k, fmt.Errorf("non-positive price")
	}
	if volume < 0 {
		return k, fmt.Errorf("negative volume")
	}
	if high < math.Max(open, closePx) || low > math.Min(open, closePx) || high < low {
		return k, fmt.Errorf("OHLC bracket violation")
	}
	if int64(closeTime) <= int64(openTime) {
		return k, fmt.Errorf("closeTime <= openTime")
	}

	k.OpenTime = int64(openTime)
	k.CloseTime = int64(closeTime)
	k.Open = open
	k.High = high
	k.Low = low
	k.Close = closePx
	k.Volume = volume
	// Best-effort enrichment (not used by indicators; ignore if malformed).
	if len(kr) >= 8 {
		k.QuoteVolume, _ = klineFloat(kr[7], "quoteVolume")
	}
	if len(kr) >= 9 {
		if t, ok := kr[8].(float64); ok {
			k.Trades = int(t)
		}
	}
	return k, nil
}

// klineFloat reads a Binance numeric-as-string field with a checked type.
func klineFloat(v interface{}, name string) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("%s not a string", name)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return f, nil
}
