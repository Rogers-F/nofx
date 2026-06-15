package kernel

import (
	"testing"

	"nofx/market"
	"nofx/store"
)

// oneMinData builds a market.Data with a 1m timeframe whose last CLOSED candle
// (index n-2) carries the given slope/accel, plus a funding-change signal.
func oneMinData(price, slopeClosed, accelClosed, fundingPrev, fundingChange float64, stale bool) *market.Data {
	// Series of length 3: index 1 is the last closed candle, index 2 is forming.
	return &market.Data{
		Symbol:            "TESTUSDT",
		CurrentPrice:      price,
		FundingRatePrev:   fundingPrev,
		FundingRateChange: fundingChange,
		FundingStale:      stale,
		FundingTime:       1,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"1m": {
				BOLLMiddleSlope: []float64{0, slopeClosed, 999},
				BOLLMiddleAccel: []float64{0, accelClosed, 999},
			},
		},
	}
}

func TestSelectGinaCandidates_DedupCapDirection(t *testing.T) {
	declines := []ginaScored{
		{"AAAUSDT", -9}, {"BBBUSDT", -5}, {"CCCUSDT", -1},
	}
	// AAA qualifies for BOTH top-2 lists (decline -9, increase +6); its decline
	// magnitude (9) beats its increase magnitude (6), so the dedup must keep long.
	increases := []ginaScored{
		{"AAAUSDT", 6}, {"DDDUSDT", 8}, {"EEEUSDT", 1},
	}
	// priceTopN=2 per direction, cap=3 total.
	got := selectGinaCandidates(declines, increases, 2, 3, 123)

	if len(got) > 3 {
		t.Fatalf("must respect cap, got %d", len(got))
	}
	sides := map[string]string{}
	for _, c := range got {
		sides[c.Symbol] = c.Side
		if c.DataTimestamp != 123 {
			t.Fatalf("timestamp not propagated: %+v", c)
		}
		if len(c.Sources) != 1 || c.Sources[0] != "gina" {
			t.Fatalf("source tag wrong: %+v", c)
		}
	}
	// AAA qualifies for both (decline -9 vs increase +2); stronger magnitude is
	// the decline, so it must be long.
	if s, ok := sides["AAAUSDT"]; !ok || s != "long" {
		t.Fatalf("AAAUSDT should be long (stronger decline), got %q present=%v", s, ok)
	}
	// Both directions should be represented within the cap.
	var haveLong, haveShort bool
	for _, s := range sides {
		if s == "long" {
			haveLong = true
		}
		if s == "short" {
			haveShort = true
		}
	}
	if !haveLong || !haveShort {
		t.Fatalf("expected both directions within cap, sides=%v", sides)
	}
}

func TestGinaEntrySatisfied(t *testing.T) {
	// Long entry: closed slope>0, accel>0, funding change<0.
	if !ginaEntrySatisfied("long", oneMinData(100, 1.0, 1.0, 0.0001, -0.0002, false)) {
		t.Fatalf("long entry should be satisfied")
	}
	// Wrong funding sign.
	if ginaEntrySatisfied("long", oneMinData(100, 1.0, 1.0, 0.0001, +0.0002, false)) {
		t.Fatalf("long entry must fail when funding change >= 0")
	}
	// Acceleration not positive.
	if ginaEntrySatisfied("long", oneMinData(100, 1.0, 0.0, 0.0001, -0.0002, false)) {
		t.Fatalf("long entry must fail when accel <= 0")
	}
	// Stale funding fails closed.
	if ginaEntrySatisfied("long", oneMinData(100, 1.0, 1.0, 0.0001, -0.0002, true)) {
		t.Fatalf("stale funding must fail closed")
	}
	// Short entry: slope<0, accel>0, funding change>0.
	if !ginaEntrySatisfied("short", oneMinData(100, -1.0, 1.0, 0.0001, +0.0002, false)) {
		t.Fatalf("short entry should be satisfied")
	}
}

func TestGinaExitSatisfied(t *testing.T) {
	// Long exit: latest settled funding>0, change>=0, slope<0, accel>0.
	// latest = prev + change = 0.001 + 0.0005 = 0.0015 > 0.
	if !ginaExitSatisfied("long", oneMinData(100, -1.0, 1.0, 0.001, 0.0005, false)) {
		t.Fatalf("long exit should be satisfied")
	}
	// Slope wrong sign for long exit.
	if ginaExitSatisfied("long", oneMinData(100, 1.0, 1.0, 0.001, 0.0005, false)) {
		t.Fatalf("long exit must fail when slope >= 0")
	}
}

func TestEnforceRiskRewardWithPrice(t *testing.T) {
	long := &Decision{Action: "open_long", StopLoss: 99, TakeProfit: 110}
	if ok, _ := enforceRiskRewardWithPrice(long, &market.Data{CurrentPrice: 100}, 3.0); !ok {
		t.Fatalf("R/R 10:1 should pass min 3.0")
	}
	// No price -> fail closed.
	if ok, _ := enforceRiskRewardWithPrice(long, &market.Data{CurrentPrice: 0}, 3.0); ok {
		t.Fatalf("missing price must fail closed")
	}
	if ok, _ := enforceRiskRewardWithPrice(long, nil, 3.0); ok {
		t.Fatalf("nil data must fail closed")
	}
	// Stop-loss above entry for a long -> risk not protective.
	bad := &Decision{Action: "open_long", StopLoss: 101, TakeProfit: 110}
	if ok, _ := enforceRiskRewardWithPrice(bad, &market.Data{CurrentPrice: 100}, 3.0); ok {
		t.Fatalf("non-protective stop-loss must fail")
	}
	// Ratio below minimum.
	low := &Decision{Action: "open_long", StopLoss: 90, TakeProfit: 105} // risk 10, reward 5 -> 0.5
	if ok, _ := enforceRiskRewardWithPrice(low, &market.Data{CurrentPrice: 100}, 3.0); ok {
		t.Fatalf("R/R 0.5 must fail min 3.0")
	}
}

func ginaEngine(soft bool) *StrategyEngine {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CoinSource.SourceType = "gina"
	cfg.CoinSource.GinaSoftMode = soft
	cfg.RiskControl.MinRiskRewardRatio = 1.0
	return NewStrategyEngine(&cfg)
}

func TestPostValidate_GinaHardGate(t *testing.T) {
	engine := ginaEngine(false)
	data := oneMinData(100, 1.0, 1.0, 0.0001, -0.0002, false) // long entry satisfied
	ctx := &Context{
		CandidateCoins: []CandidateCoin{{Symbol: "TESTUSDT", Side: "long"}},
		MarketDataMap:  map[string]*market.Data{"TESTUSDT": data},
	}

	// Valid GINA long passes.
	fd := &FullDecision{Decisions: []Decision{
		{Symbol: "TESTUSDT", Action: "open_long", Leverage: 3, PositionSizeUSD: 50, StopLoss: 99, TakeProfit: 110},
	}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "open_long" {
		t.Fatalf("valid GINA long should pass, got %s (%s)", fd.Decisions[0].Action, fd.Decisions[0].Reasoning)
	}

	// Reverse direction is downgraded to wait.
	fd = &FullDecision{Decisions: []Decision{
		{Symbol: "TESTUSDT", Action: "open_short", Leverage: 3, PositionSizeUSD: 50, StopLoss: 110, TakeProfit: 90},
	}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "wait" {
		t.Fatalf("reverse-direction open must be downgraded to wait, got %s", fd.Decisions[0].Action)
	}

	// Symbol not in candidate pool is downgraded.
	fd = &FullDecision{Decisions: []Decision{
		{Symbol: "OTHERUSDT", Action: "open_long", Leverage: 3, PositionSizeUSD: 50, StopLoss: 99, TakeProfit: 110},
	}}
	ctx.MarketDataMap["OTHERUSDT"] = &market.Data{CurrentPrice: 100}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "wait" {
		t.Fatalf("out-of-pool open must be downgraded to wait, got %s", fd.Decisions[0].Action)
	}

	// Close is always allowed.
	fd = &FullDecision{Decisions: []Decision{{Symbol: "TESTUSDT", Action: "close_long"}}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "close_long" {
		t.Fatalf("close must never be gated, got %s", fd.Decisions[0].Action)
	}
}

func TestPostValidate_GinaEntryNotSatisfied(t *testing.T) {
	engine := ginaEngine(false)
	// Funding change positive -> long entry NOT satisfied.
	data := oneMinData(100, 1.0, 1.0, 0.0001, +0.0002, false)
	ctx := &Context{
		CandidateCoins: []CandidateCoin{{Symbol: "TESTUSDT", Side: "long"}},
		MarketDataMap:  map[string]*market.Data{"TESTUSDT": data},
	}
	fd := &FullDecision{Decisions: []Decision{
		{Symbol: "TESTUSDT", Action: "open_long", Leverage: 3, PositionSizeUSD: 50, StopLoss: 99, TakeProfit: 110},
	}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "wait" {
		t.Fatalf("entry-not-satisfied long must be downgraded to wait, got %s", fd.Decisions[0].Action)
	}
}

func TestPostValidate_SoftModeBypassesGate(t *testing.T) {
	engine := ginaEngine(true) // soft mode
	data := oneMinData(100, 1.0, 1.0, 0.0001, +0.0002, false)
	ctx := &Context{
		CandidateCoins: []CandidateCoin{{Symbol: "TESTUSDT", Side: "long"}},
		MarketDataMap:  map[string]*market.Data{"TESTUSDT": data},
	}
	// Reverse direction but soft mode: gate is bypassed, only R/R applies.
	fd := &FullDecision{Decisions: []Decision{
		{Symbol: "TESTUSDT", Action: "open_short", Leverage: 3, PositionSizeUSD: 50, StopLoss: 110, TakeProfit: 90},
	}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "open_short" {
		t.Fatalf("soft mode should bypass direction gate, got %s (%s)", fd.Decisions[0].Action, fd.Decisions[0].Reasoning)
	}
}

func nonGinaEngine() *StrategyEngine {
	cfg := store.GetDefaultStrategyConfig("en")
	cfg.CoinSource.SourceType = "static"
	cfg.RiskControl.MinRiskRewardRatio = 3.0
	return NewStrategyEngine(&cfg)
}

func TestPostValidate_NonGinaNoPricePreserved(t *testing.T) {
	engine := nonGinaEngine()
	// No market data for the symbol -> legacy behavior preserved (no downgrade),
	// so existing non-GINA symbol tolerance is not regressed.
	ctx := &Context{MarketDataMap: map[string]*market.Data{}}
	fd := &FullDecision{Decisions: []Decision{
		{Symbol: "FOOUSDT", Action: "open_long", Leverage: 3, PositionSizeUSD: 50, StopLoss: 99, TakeProfit: 110},
	}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "open_long" {
		t.Fatalf("non-GINA open with no price must be preserved, got %s (%s)", fd.Decisions[0].Action, fd.Decisions[0].Reasoning)
	}
}

func TestPostValidate_RealRRAppliesToAllStrategies(t *testing.T) {
	engine := nonGinaEngine() // minRR 3.0
	data := &market.Data{Symbol: "FOOUSDT", CurrentPrice: 100}
	ctx := &Context{MarketDataMap: map[string]*market.Data{"FOOUSDT": data}}
	// risk 10 (100->90), reward 5 (100->105) => 0.5:1 < 3.0 => downgraded.
	fd := &FullDecision{Decisions: []Decision{
		{Symbol: "FOOUSDT", Action: "open_long", Leverage: 3, PositionSizeUSD: 50, StopLoss: 90, TakeProfit: 105},
	}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	if fd.Decisions[0].Action != "wait" {
		t.Fatalf("non-GINA low R/R must be downgraded to wait, got %s", fd.Decisions[0].Action)
	}
}

func TestInjectGinaExits(t *testing.T) {
	engine := ginaEngine(false)
	// Held long whose GINA long-exit is satisfied.
	data := oneMinData(100, -1.0, 1.0, 0.001, 0.0005, false)
	ctx := &Context{
		Positions:     []PositionInfo{{Symbol: "TESTUSDT", Side: "long"}},
		MarketDataMap: map[string]*market.Data{"TESTUSDT": data},
	}
	fd := &FullDecision{Decisions: []Decision{{Symbol: "TESTUSDT", Action: "hold"}}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)

	var injected bool
	for _, d := range fd.Decisions {
		if d.Symbol == "TESTUSDT" && d.Action == "close_long" {
			injected = true
		}
	}
	if !injected {
		t.Fatalf("GINA exit should inject a close_long, decisions=%+v", fd.Decisions)
	}

	// If a close already exists, no duplicate is injected.
	fd = &FullDecision{Decisions: []Decision{{Symbol: "TESTUSDT", Action: "close_long"}}}
	fd.Decisions = PostValidateDecisions(ctx, engine, fd.Decisions)
	count := 0
	for _, d := range fd.Decisions {
		if d.Action == "close_long" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("should not duplicate existing close, got %d", count)
	}
}
