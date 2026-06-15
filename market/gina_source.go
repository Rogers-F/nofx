package market

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"nofx/logger"
	"sort"
	"strconv"
	"sync"
	"time"
)

// This file backs the GINA coin source: a Binance 24h volume ranking plus the
// settled funding-rate change signal. All calls reuse the shared HTTP client
// (NewAPIClient, which honors hook/proxy settings), add 429/418 backoff, and
// cache process-wide so that multiple traders on the same account do not each
// hammer the weighted endpoints.

const (
	// 8h Binance funding settlement; a settled change older than this plus a
	// grace window is treated as stale.
	fundingSettleInterval = 8 * time.Hour
	fundingStaleGrace     = 2 * time.Hour
)

var (
	// Full 24h ticker snapshot is heavy (no-symbol weight 40); cache briefly and
	// share across traders.
	all24hCacheTTL = 45 * time.Second
	all24hMu       sync.Mutex
	all24hData     []Ticker24hr
	all24hAt       time.Time

	// USDT perpetual tradable set changes rarely; cache for an hour.
	usdtPerpCacheTTL = 1 * time.Hour
	usdtPerpMu       sync.Mutex
	usdtPerpSet      map[string]bool
	usdtPerpAt       time.Time

	// Settled funding-rate change cache (8h cadence, so 1h cache is ample).
	fundingChangeMap sync.Map // map[string]*FundingChangeCache
	fcCacheTTL       = 1 * time.Hour

	// Recent kline change cache keyed by "symbol|interval". Short TTL: 1h/15m
	// candle closes are periodic, so a brief cache removes redundant fetches
	// within and across decision cycles without staling the selection signal.
	recentChangeMap sync.Map // map[string]*recentChangeCache
	rcCacheTTL      = 60 * time.Second
)

type recentChangeCache struct {
	pct       float64
	updatedAt time.Time
}

// FundingChangeInfo carries the settled funding-rate change signal used by GINA.
type FundingChangeInfo struct {
	Latest          float64
	Prev            float64
	Change          float64 // Latest - Prev
	FundingTime     int64   // settlement time (ms) of the latest settled rate
	NextFundingTime int64   // approximate next settlement (ms)
	Stale           bool    // true when fewer than two records or data is too old
}

// FundingChangeCache caches a settled funding-rate change result.
type FundingChangeCache struct {
	Info      *FundingChangeInfo
	UpdatedAt time.Time
}

// GinaRankedSymbol is a 24h volume ranking entry for a USDT perpetual.
type GinaRankedSymbol struct {
	Symbol             string
	QuoteVolume        float64
	PriceChangePercent float64 // 24h percent change
}

// httpGetWithBackoff performs a GET with retries on Binance rate-limit (429) and
// IP-ban (418) responses, honoring Retry-After. It also surfaces the used-weight
// header for observability.
func httpGetWithBackoff(client *http.Client, url string) ([]byte, error) {
	const maxAttempts = 4
	var lastErr error
	backoff := 1 * time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		if w := resp.Header.Get("X-MBX-USED-WEIGHT-1M"); w != "" {
			logger.Debugf("Binance used weight (1m): %s", w)
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 418 {
			wait := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, perr := strconv.Atoi(ra); perr == nil && secs > 0 {
					wait = time.Duration(secs) * time.Second
				}
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("binance rate limited (status %d), backing off %s", resp.StatusCode, wait)
			logger.Infof("⚠️ %v", lastErr)
			time.Sleep(wait)
			backoff *= 2
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("binance request failed (status %d): %s", resp.StatusCode, string(body))
		}
		return body, nil
	}
	return nil, fmt.Errorf("binance request exhausted retries: %w", lastErr)
}

// getAll24hTickers returns a process-cached snapshot of the full 24h ticker list.
func getAll24hTickers() ([]Ticker24hr, error) {
	all24hMu.Lock()
	defer all24hMu.Unlock()

	if all24hData != nil && time.Since(all24hAt) < all24hCacheTTL {
		return all24hData, nil
	}

	url := fmt.Sprintf("%s/fapi/v1/ticker/24hr", baseURL)
	body, err := httpGetWithBackoff(NewAPIClient().client, url)
	if err != nil {
		// Serve a stale snapshot rather than nothing if we have one.
		if all24hData != nil {
			logger.Infof("⚠️ 24h ticker refresh failed, using stale snapshot: %v", err)
			return all24hData, nil
		}
		return nil, err
	}

	var tickers []Ticker24hr
	if err := json.Unmarshal(body, &tickers); err != nil {
		return nil, err
	}

	all24hData = tickers
	all24hAt = time.Now()
	return tickers, nil
}

// getUSDTPerpetualSet returns the set of tradable USDT perpetual symbols.
func getUSDTPerpetualSet() (map[string]bool, error) {
	usdtPerpMu.Lock()
	defer usdtPerpMu.Unlock()

	if usdtPerpSet != nil && time.Since(usdtPerpAt) < usdtPerpCacheTTL {
		return usdtPerpSet, nil
	}

	info, err := NewAPIClient().GetExchangeInfo()
	if err != nil {
		if usdtPerpSet != nil {
			logger.Infof("⚠️ exchangeInfo refresh failed, using stale set: %v", err)
			return usdtPerpSet, nil
		}
		return nil, err
	}

	set := make(map[string]bool, len(info.Symbols))
	for _, s := range info.Symbols {
		if s.Status == "TRADING" && s.QuoteAsset == "USDT" && s.ContractType == "PERPETUAL" {
			set[s.Symbol] = true
		}
	}

	usdtPerpSet = set
	usdtPerpAt = time.Now()
	return set, nil
}

// GetBinanceVolumeRank returns the top-N tradable USDT perpetuals by 24h quote
// volume, descending. Each entry also carries the 24h percent change.
func GetBinanceVolumeRank(topN int) ([]GinaRankedSymbol, error) {
	if topN <= 0 {
		topN = 20
	}

	tickers, err := getAll24hTickers()
	if err != nil {
		return nil, err
	}
	perpSet, err := getUSDTPerpetualSet()
	if err != nil {
		return nil, err
	}

	ranked := make([]GinaRankedSymbol, 0, len(tickers))
	for _, t := range tickers {
		if !perpSet[t.Symbol] {
			continue
		}
		qv, perr := strconv.ParseFloat(t.QuoteVolume, 64)
		if perr != nil || qv <= 0 {
			continue
		}
		pct, _ := strconv.ParseFloat(t.PriceChangePercent, 64)
		ranked = append(ranked, GinaRankedSymbol{
			Symbol:             t.Symbol,
			QuoteVolume:        qv,
			PriceChangePercent: pct,
		})
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].QuoteVolume > ranked[j].QuoteVolume
	})

	if len(ranked) > topN {
		ranked = ranked[:topN]
	}
	return ranked, nil
}

// GetRecentChangePct returns the percent change between the last two closes of
// the given interval (e.g. "1h", "15m"), using the most recent two candles.
// Used by the GINA selection to replicate its 24h/1h/15m extreme ranking.
func GetRecentChangePct(symbol, interval string) (float64, error) {
	cacheKey := symbol + "|" + interval
	if cached, ok := recentChangeMap.Load(cacheKey); ok {
		c := cached.(*recentChangeCache)
		if time.Since(c.updatedAt) < rcCacheTTL {
			return c.pct, nil
		}
	}

	url := fmt.Sprintf("%s/fapi/v1/klines?symbol=%s&interval=%s&limit=2", baseURL, symbol, interval)
	body, err := httpGetWithBackoff(NewAPIClient().client, url)
	if err != nil {
		return 0, err
	}

	var raw []KlineResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, err
	}
	if len(raw) < 2 {
		return 0, fmt.Errorf("insufficient klines for %s %s", symbol, interval)
	}

	prev, err := parseBinanceKline(raw[len(raw)-2])
	if err != nil {
		return 0, err
	}
	last, err := parseBinanceKline(raw[len(raw)-1])
	if err != nil {
		return 0, err
	}
	if prev.Close == 0 {
		return 0, fmt.Errorf("zero previous close for %s %s", symbol, interval)
	}
	pct := (last.Close - prev.Close) / prev.Close * 100
	recentChangeMap.Store(cacheKey, &recentChangeCache{pct: pct, updatedAt: time.Now()})
	return pct, nil
}

// fundingRateHistoryEntry mirrors a /fapi/v1/fundingRate record.
type fundingRateHistoryEntry struct {
	Symbol      string `json:"symbol"`
	FundingRate string `json:"fundingRate"`
	FundingTime int64  `json:"fundingTime"`
}

// GetFundingRateChange returns the change between the two most recently settled
// funding rates for a symbol. This mirrors GINA's ccxt fetch_funding_rate_history
// (limit=2) and is therefore an 8h-cadence discrete signal, not a per-minute one.
func GetFundingRateChange(symbol string) (*FundingChangeInfo, error) {
	if cached, ok := fundingChangeMap.Load(symbol); ok {
		c := cached.(*FundingChangeCache)
		if time.Since(c.UpdatedAt) < fcCacheTTL {
			return c.Info, nil
		}
	}

	url := fmt.Sprintf("%s/fapi/v1/fundingRate?symbol=%s&limit=2", baseURL, symbol)
	body, err := httpGetWithBackoff(NewAPIClient().client, url)
	if err != nil {
		return nil, err
	}

	var entries []fundingRateHistoryEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, err
	}

	// Binance returns ascending by fundingTime; sort defensively.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].FundingTime < entries[j].FundingTime
	})

	info := &FundingChangeInfo{Stale: true}
	switch len(entries) {
	case 0:
		// nothing usable
	case 1:
		latest, _ := strconv.ParseFloat(entries[0].FundingRate, 64)
		info.Latest = latest
		info.FundingTime = entries[0].FundingTime
		info.NextFundingTime = entries[0].FundingTime + fundingSettleInterval.Milliseconds()
	default:
		prev, _ := strconv.ParseFloat(entries[len(entries)-2].FundingRate, 64)
		latest, _ := strconv.ParseFloat(entries[len(entries)-1].FundingRate, 64)
		info.Prev = prev
		info.Latest = latest
		info.Change = latest - prev
		info.FundingTime = entries[len(entries)-1].FundingTime
		info.NextFundingTime = info.FundingTime + fundingSettleInterval.Milliseconds()
		// Fresh only when the latest settlement is recent enough.
		age := time.Since(time.UnixMilli(info.FundingTime))
		info.Stale = age > (fundingSettleInterval + fundingStaleGrace)
	}

	fundingChangeMap.Store(symbol, &FundingChangeCache{Info: info, UpdatedAt: time.Now()})
	return info, nil
}
