package kernel

import (
	"fmt"
	"nofx/logger"
	"nofx/market"
)

// ============================================================================
// Decision Validation
// ============================================================================

func validateDecisions(decisions []Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int, btcEthPosRatio, altcoinPosRatio float64) error {
	for i := range decisions {
		if err := validateDecision(&decisions[i], accountEquity, btcEthLeverage, altcoinLeverage, btcEthPosRatio, altcoinPosRatio); err != nil {
			return fmt.Errorf("decision #%d validation failed: %w", i+1, err)
		}
	}
	return nil
}

func validateDecision(d *Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int, btcEthPosRatio, altcoinPosRatio float64) error {
	validActions := map[string]bool{
		"open_long":   true,
		"open_short":  true,
		"close_long":  true,
		"close_short": true,
		"hold":        true,
		"wait":        true,
	}

	if !validActions[d.Action] {
		return fmt.Errorf("invalid action: %s", d.Action)
	}

	if d.Action == "open_long" || d.Action == "open_short" {
		// Asset tiering for validation:
		//   - BTC/ETH crypto perps use the BTC/ETH tier (typically 5x equity).
		//   - Hyperliquid XYZ assets (US equities, commodities, forex) are
		//     also treated as the higher tier — they are not crypto altcoins
		//     and the user's quick-trade flow shows them at the higher cap,
		//     so the validator must match.
		//   - Everything else is altcoin (1x equity by default).
		maxLeverage := altcoinLeverage
		posRatio := altcoinPosRatio
		maxPositionValue := accountEquity * posRatio
		isMajor := d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" || market.IsXyzDexAsset(d.Symbol)
		if isMajor {
			maxLeverage = btcEthLeverage
			posRatio = btcEthPosRatio
			maxPositionValue = accountEquity * posRatio
		}

		if d.Leverage <= 0 {
			return fmt.Errorf("leverage must be greater than 0: %d", d.Leverage)
		}
		if d.Leverage > maxLeverage {
			logger.Infof("⚠️  [Leverage Fallback] %s leverage exceeded (%dx > %dx), auto-adjusting to limit %dx",
				d.Symbol, d.Leverage, maxLeverage, maxLeverage)
			d.Leverage = maxLeverage
		}
		if d.PositionSizeUSD <= 0 {
			return fmt.Errorf("position size must be greater than 0: %.2f", d.PositionSizeUSD)
		}

		const minPositionSizeGeneral = 12.0
		const minPositionSizeBTCETH = 60.0

		if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
			if d.PositionSizeUSD < minPositionSizeBTCETH {
				return fmt.Errorf("%s opening amount too small (%.2f USDT), must be ≥%.2f USDT", d.Symbol, d.PositionSizeUSD, minPositionSizeBTCETH)
			}
		} else {
			if d.PositionSizeUSD < minPositionSizeGeneral {
				return fmt.Errorf("opening amount too small (%.2f USDT), must be ≥%.2f USDT", d.PositionSizeUSD, minPositionSizeGeneral)
			}
		}

		tolerance := maxPositionValue * 0.01
		if d.PositionSizeUSD > maxPositionValue+tolerance {
			switch {
			case d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT":
				return fmt.Errorf("BTC/ETH single coin position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", maxPositionValue, posRatio, d.PositionSizeUSD)
			case market.IsXyzDexAsset(d.Symbol):
				return fmt.Errorf("%s position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", d.Symbol, maxPositionValue, posRatio, d.PositionSizeUSD)
			default:
				return fmt.Errorf("altcoin single coin position value cannot exceed %.0f USDT (%.1fx account equity), actual: %.0f", maxPositionValue, posRatio, d.PositionSizeUSD)
			}
		}
		if d.StopLoss <= 0 || d.TakeProfit <= 0 {
			return fmt.Errorf("stop loss and take profit must be greater than 0")
		}

		if d.Action == "open_long" {
			if d.StopLoss >= d.TakeProfit {
				return fmt.Errorf("for long positions, stop loss price must be less than take profit price")
			}
		} else {
			if d.StopLoss <= d.TakeProfit {
				return fmt.Errorf("for short positions, stop loss price must be greater than take profit price")
			}
		}

		// Risk/reward is enforced downstream in PostValidateDecisions using the
		// real current price. The previous check synthesized an entry price from
		// stop-loss/take-profit, which yielded a constant ratio and never
		// rejected anything.
	}

	return nil
}

// defaultMinRiskReward is the fallback minimum risk/reward ratio used when the
// strategy config does not specify one. Matches the historical default.
const defaultMinRiskReward = 3.0

// PostValidateDecisions applies price-aware checks that the stateless parser
// cannot: a real risk/reward gate (all strategies) and, for the GINA source in
// hard mode, the directional/entry gate plus exit enforcement. It returns the
// (possibly modified) decision slice — non-compliant opens are downgraded to
// "wait" and GINA exits are appended; closing and maintenance actions are never
// blocked.
//
// IMPORTANT: call this AFTER symbol canonicalization (the strategy-universe
// filter), so decision symbols match ctx.MarketDataMap / candidate keys. For
// non-GINA strategies, a decision whose price cannot be resolved is left
// untouched (preserving legacy behavior); only GINA hard mode fails closed,
// because its entry signal is mandatory.
func PostValidateDecisions(ctx *Context, engine *StrategyEngine, decisions []Decision) []Decision {
	if ctx == nil || engine == nil || len(decisions) == 0 {
		return decisions
	}
	cfg := engine.GetConfig()
	minRR := cfg.RiskControl.MinRiskRewardRatio
	if minRR <= 0 {
		minRR = defaultMinRiskReward
	}
	ginaHard := cfg.CoinSource.SourceType == "gina" && !cfg.CoinSource.GinaSoftMode

	// Gate rationale: non-compliant opens are downgraded to "wait" rather than
	// erroring the whole batch (as validateDecisions does for hard violations).
	// This preserves valid close/hold actions in the same cycle and lets the
	// model only ever be more conservative — it can wait, resize, or close, but
	// the GINA direction/entry rules cannot be overridden into a new open.
	var sideBySym map[string]string
	if ginaHard {
		sideBySym = make(map[string]string)
		for _, c := range ctx.CandidateCoins {
			if c.Side != "" {
				sideBySym[c.Symbol] = c.Side
			}
		}
	}

	for i := range decisions {
		d := &decisions[i]
		if d.Action != "open_long" && d.Action != "open_short" {
			continue
		}
		data := resolveMarketData(ctx, d.Symbol)
		if data == nil || data.CurrentPrice <= 0 {
			// GINA needs the price+signal; fail closed. Other strategies keep
			// their legacy behavior (no real R/R was enforced before), so leave
			// the decision untouched for downstream handling.
			if ginaHard {
				downgradeToWait(d, "no current price available for GINA gate (fail-closed)")
			}
			continue
		}

		if ok, reason := enforceRiskRewardWithPrice(d, data, minRR); !ok {
			downgradeToWait(d, reason)
			continue
		}

		if ginaHard {
			wantSide := "long"
			if d.Action == "open_short" {
				wantSide = "short"
			}
			candSide := ginaCandidateSide(sideBySym, d.Symbol)
			if candSide != wantSide {
				downgradeToWait(d, fmt.Sprintf("gina: %s not allowed for %s (candidate side=%q)", d.Action, d.Symbol, candSide))
				continue
			}
			if !ginaEntrySatisfied(wantSide, data) {
				downgradeToWait(d, fmt.Sprintf("gina: entry conditions not satisfied for %s %s", wantSide, d.Symbol))
				continue
			}
		}
	}

	if ginaHard {
		decisions = injectGinaExits(ctx, decisions)
	}
	return decisions
}

// resolveMarketData looks up market data by symbol, falling back to the
// normalized form so minor symbol variants still resolve.
func resolveMarketData(ctx *Context, symbol string) *market.Data {
	if d := ctx.MarketDataMap[symbol]; d != nil {
		return d
	}
	return ctx.MarketDataMap[market.Normalize(symbol)]
}

// ginaCandidateSide returns the allowed side for a symbol from the candidate
// map, trying the normalized form as a fallback. Empty when not a candidate.
func ginaCandidateSide(sideBySym map[string]string, symbol string) string {
	if s, ok := sideBySym[symbol]; ok {
		return s
	}
	return sideBySym[market.Normalize(symbol)]
}

// enforceRiskRewardWithPrice validates an open decision's risk/reward using the
// real current price. Fails closed when price is unavailable.
func enforceRiskRewardWithPrice(d *Decision, data *market.Data, minRR float64) (bool, string) {
	if data == nil || data.CurrentPrice <= 0 {
		return false, "no current price available for risk/reward check (fail-closed)"
	}
	if d.StopLoss <= 0 || d.TakeProfit <= 0 {
		return false, "missing stop-loss/take-profit"
	}
	entry := data.CurrentPrice
	var risk, reward float64
	if d.Action == "open_long" {
		risk = entry - d.StopLoss
		reward = d.TakeProfit - entry
	} else {
		risk = d.StopLoss - entry
		reward = entry - d.TakeProfit
	}
	if risk <= 0 {
		return false, fmt.Sprintf("stop-loss not protective at current price %.6f", entry)
	}
	if reward <= 0 {
		return false, fmt.Sprintf("take-profit beyond current price %.6f", entry)
	}
	ratio := reward / risk
	if ratio < minRR {
		return false, fmt.Sprintf("risk/reward %.2f:1 below minimum %.2f:1 (entry %.6f sl %.6f tp %.6f)",
			ratio, minRR, entry, d.StopLoss, d.TakeProfit)
	}
	return true, ""
}

// downgradeToWait turns a rejected open decision into a no-op wait, preserving an
// audit trail in the reasoning and clearing the now-irrelevant open parameters.
func downgradeToWait(d *Decision, reason string) {
	logger.Infof("⛔ Downgrading %s %s to wait: %s", d.Action, d.Symbol, reason)
	d.Action = "wait"
	if d.Reasoning != "" {
		d.Reasoning += " | "
	}
	d.Reasoning += "[gate] " + reason
	d.Leverage = 0
	d.PositionSizeUSD = 0
	d.StopLoss = 0
	d.TakeProfit = 0
}

// injectGinaExits appends close decisions for held positions whose GINA exit
// conditions are satisfied but that the model did not already close. Returns the
// (possibly extended) decision slice.
func injectGinaExits(ctx *Context, decisions []Decision) []Decision {
	closing := make(map[string]bool)
	for _, d := range decisions {
		switch d.Action {
		case "close_long":
			closing[d.Symbol+"_long"] = true
		case "close_short":
			closing[d.Symbol+"_short"] = true
		}
	}

	for _, pos := range ctx.Positions {
		if pos.Side != "long" && pos.Side != "short" {
			continue
		}
		data := resolveMarketData(ctx, pos.Symbol)
		if data == nil || !ginaExitSatisfied(pos.Side, data) {
			continue
		}
		if closing[pos.Symbol+"_"+pos.Side] {
			continue
		}
		action := "close_long"
		if pos.Side == "short" {
			action = "close_short"
		}
		logger.Infof("🚪 GINA exit triggered for %s %s — injecting %s", pos.Symbol, pos.Side, action)
		decisions = append(decisions, Decision{
			Symbol:    pos.Symbol,
			Action:    action,
			Reasoning: "[gina] exit conditions satisfied (funding reversal + middle-band momentum reversal)",
		})
	}
	return decisions
}
