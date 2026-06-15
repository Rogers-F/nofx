package kernel

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"nofx/logger"
	"nofx/market"
	"nofx/store"
)

// ginaFetchConcurrency bounds how many symbols' 1h/15m changes are fetched in
// parallel during selection, balancing latency against exchange API burst.
const ginaFetchConcurrency = 6

// ============================================================================
// GINA coin source and rule gate
//
// GINA selects coins by Binance 24h quote-volume rank, then ranks the survivors
// by extreme price move across 24h/1h/15m: the biggest decliners are oversold
// (long candidates) and the biggest gainers are overbought (short candidates).
// Entry/exit then key off the 1m Bollinger middle-band slope/acceleration and
// the settled funding-rate change. This mirrors the original GINA strategy.
//
// In hard mode (GinaSoftMode = false, the default) the direction and entry
// conditions are enforced in code so the model can only be more conservative
// (wait), size, set SL/TP, or close earlier. In soft mode the same signals are
// advisory context only.
// ============================================================================

const ginaTimeframe = "1m"

// getGinaCoins builds the GINA candidate list. The combined long+short list is
// hard capped at store.MaxCandidateCoins.
func (e *StrategyEngine) getGinaCoins(volumeTopN, priceTopN int) ([]CandidateCoin, error) {
	if volumeTopN <= 0 {
		volumeTopN = 20
	}
	if priceTopN <= 0 {
		priceTopN = 5
	}

	ranked, err := market.GetBinanceVolumeRank(volumeTopN)
	if err != nil {
		return nil, fmt.Errorf("gina: failed to rank by 24h volume: %w", err)
	}
	if len(ranked) == 0 {
		return nil, nil
	}

	now := time.Now().UnixMilli()

	// Fetch each symbol's 1h/15m change with bounded concurrency. 24h change
	// comes from the ticker; 1h/15m from the last two candles. Replicates GINA's
	// extreme-across-timeframes ranking. Results are positional so ordering is
	// deterministic regardless of completion order.
	declines := make([]ginaScored, len(ranked))
	increases := make([]ginaScored, len(ranked))
	sem := make(chan struct{}, ginaFetchConcurrency)
	var wg sync.WaitGroup
	for i, r := range ranked {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r market.GinaRankedSymbol) {
			defer wg.Done()
			defer func() { <-sem }()
			changes := []float64{r.PriceChangePercent}
			if ch1h, e1 := market.GetRecentChangePct(r.Symbol, "1h"); e1 == nil {
				changes = append(changes, ch1h)
			}
			if ch15m, e15 := market.GetRecentChangePct(r.Symbol, "15m"); e15 == nil {
				changes = append(changes, ch15m)
			}
			minCh, maxCh := changes[0], changes[0]
			for _, c := range changes[1:] {
				if c < minCh {
					minCh = c
				}
				if c > maxCh {
					maxCh = c
				}
			}
			declines[i] = ginaScored{r.Symbol, minCh}  // most negative -> oversold
			increases[i] = ginaScored{r.Symbol, maxCh} // most positive -> overbought
		}(i, r)
	}
	wg.Wait()

	candidates := selectGinaCandidates(declines, increases, priceTopN, store.MaxCandidateCoins, now)
	logger.Infof("🧭 GINA candidates: %d (volumeTopN=%d priceTopN=%d, cap=%d)", len(candidates), volumeTopN, priceTopN, store.MaxCandidateCoins)
	return candidates, nil
}

// ginaScored is a symbol with its selection change metric (percent).
type ginaScored struct {
	symbol string
	change float64
}

// selectGinaCandidates turns oversold (decline) and overbought (increase)
// rankings into a directional candidate list. Decliners become long candidates,
// gainers become short candidates. A symbol qualifying for both keeps the side
// with the stronger magnitude (ties prefer long, matching GINA's long-first
// check). The combined list is capped at maxCap and interleaved so both
// directions are represented. This is the pure core of getGinaCoins.
func selectGinaCandidates(declines, increases []ginaScored, priceTopN, maxCap int, now int64) []CandidateCoin {
	if priceTopN <= 0 {
		priceTopN = 5
	}
	if maxCap <= 0 {
		maxCap = store.MaxCandidateCoins
	}

	sort.Slice(declines, func(i, j int) bool { return declines[i].change < declines[j].change })
	sort.Slice(increases, func(i, j int) bool { return increases[i].change > increases[j].change })

	if len(declines) > priceTopN {
		declines = declines[:priceTopN]
	}
	if len(increases) > priceTopN {
		increases = increases[:priceTopN]
	}

	type cand struct {
		side   string
		mag    float64
		reason string
	}
	bySym := make(map[string]*cand)
	addOrReplace := func(symbol, side string, change float64) {
		mag := change
		if mag < 0 {
			mag = -mag
		}
		reason := fmt.Sprintf("gina %s: extreme move %.2f%%", side, change)
		if existing, ok := bySym[symbol]; ok {
			if mag > existing.mag {
				existing.side = side
				existing.mag = mag
				existing.reason = reason
			}
			return
		}
		bySym[symbol] = &cand{side: side, mag: mag, reason: reason}
	}

	for _, d := range declines {
		addOrReplace(d.symbol, "long", d.change)
	}
	for _, in := range increases {
		addOrReplace(in.symbol, "short", in.change)
	}

	candidates := make([]CandidateCoin, 0, maxCap)
	emitted := make(map[string]bool)
	emit := func(symbol string) {
		if emitted[symbol] || len(candidates) >= maxCap {
			return
		}
		c := bySym[symbol]
		emitted[symbol] = true
		candidates = append(candidates, CandidateCoin{
			Symbol:        market.Normalize(symbol),
			Sources:       []string{"gina"},
			Side:          c.side,
			Reason:        c.reason,
			DataTimestamp: now,
		})
	}

	// Interleave decline/increase order to keep both directions within the cap.
	for i := 0; i < priceTopN && len(candidates) < maxCap; i++ {
		if i < len(declines) {
			emit(declines[i].symbol)
		}
		if i < len(increases) {
			emit(increases[i].symbol)
		}
	}
	return candidates
}

// ============================================================================
// GINA signal evaluation (1m Bollinger middle momentum + settled funding change)
// ============================================================================

// ginaClosedSlopeAccel returns the 1m Bollinger middle-band slope and
// acceleration computed on the last CLOSED candle (the latest candle is dropped
// because it may still be forming). ok is false when 1m data is unavailable.
func ginaClosedSlopeAccel(data *market.Data) (slope, accel float64, ok bool) {
	if data == nil || data.TimeframeData == nil {
		return 0, 0, false
	}
	tf := data.TimeframeData[ginaTimeframe]
	if tf == nil {
		return 0, 0, false
	}
	n := len(tf.BOLLMiddleSlope)
	if n < 2 {
		return 0, 0, false
	}
	idx := n - 2 // last closed candle
	slope = tf.BOLLMiddleSlope[idx]
	if idx >= 0 && idx < len(tf.BOLLMiddleAccel) {
		accel = tf.BOLLMiddleAccel[idx]
	}
	return slope, accel, true
}

// ginaLatestSettledFunding returns the latest settled funding rate, derived from
// the previous settled rate plus the settled change.
func ginaLatestSettledFunding(data *market.Data) float64 {
	return data.FundingRatePrev + data.FundingRateChange
}

// ginaSignalReady reports whether the GINA momentum signal is usable: 1m data
// is present, funding is not stale, and acceleration is positive (GINA requires
// accelerating momentum for both entries and exits). Returns the closed-candle
// slope when ready.
func ginaSignalReady(data *market.Data) (slope float64, ok bool) {
	if data == nil || data.FundingStale {
		return 0, false
	}
	s, accel, have := ginaClosedSlopeAccel(data)
	if !have || accel <= 0 {
		return 0, false
	}
	return s, true
}

// ginaEntrySatisfied reports whether GINA's entry conditions hold for the given
// direction ("long"/"short"). Mirrors strategy.py check_*_entry_conditions
// (active logic): 1m middle slope sign + acceleration > 0 + funding-change sign.
// Stale funding data fails closed (entry not allowed).
func ginaEntrySatisfied(side string, data *market.Data) bool {
	slope, ok := ginaSignalReady(data)
	if !ok {
		return false
	}
	switch side {
	case "long":
		return slope > 0 && data.FundingRateChange < 0
	case "short":
		return slope < 0 && data.FundingRateChange > 0
	default:
		return false
	}
}

// ginaExitSatisfied reports whether GINA's exit conditions hold for an open
// position of the given side. Mirrors strategy.py check_*_exit_conditions
// (active logic): funding reversal plus middle-band momentum reversal.
func ginaExitSatisfied(side string, data *market.Data) bool {
	slope, ok := ginaSignalReady(data)
	if !ok {
		return false
	}
	latest := ginaLatestSettledFunding(data)
	switch side {
	case "long":
		return latest > 0 && data.FundingRateChange >= 0 && slope < 0
	case "short":
		return latest < 0 && data.FundingRateChange <= 0 && slope > 0
	default:
		return false
	}
}
