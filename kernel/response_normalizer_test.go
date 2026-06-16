package kernel

import (
	"strings"
	"testing"
)

// parseNormalized runs the normalized output through the REAL downstream
// parser (extractDecisions) so the tests assert both that the normalized
// string is parseable and that the resulting actions are correct.
func parseNormalized(t *testing.T, normalized string) []Decision {
	t.Helper()
	ds, err := extractDecisions(normalized)
	if err != nil {
		t.Fatalf("extractDecisions failed: %v\nnormalized:\n%s", err, normalized)
	}
	return ds
}

// Sample 1: bare ```json-style object, no <decision> tag, OPEN_POSITION with
// a non-USD size field (position_size). The USD whitelist must reject it, so
// the safe outcome is "wait" even though price/SL/TP are internally consistent.
func TestNormalize_Sample1_NonUSDSizeMustWait(t *testing.T) {
	raw := `{"decision":"OPEN_POSITION","symbol":"xyz:SNDK","side":"LONG","position_size":0.03,"stop_loss":1978.8,"take_profit":2018.0,"confidence":0.64,"reason":"momentum breakout"}`
	pool := []string{"xyz:SNDK"}
	price := map[string]float64{"xyz:SNDK": 2000.0} // between SL and TP — only the USD gate should fail

	normalized, changed, _ := NormalizeAIResponse(raw, pool, price, 1000, 0)
	if !changed {
		t.Fatalf("expected changed=true for free-form input")
	}
	ds := parseNormalized(t, normalized)
	if len(ds) != 1 {
		t.Fatalf("expected 1 decision, got %d: %+v", len(ds), ds)
	}
	if ds[0].Action != "wait" {
		t.Fatalf("expected wait (position_size is not USD), got %q", ds[0].Action)
	}
}

// Sample 2: custom NO_TRADE schema carrying a watchlist. The watchlist must
// NEVER be treated as an order array; the result is a single "wait".
func TestNormalize_Sample2_WatchlistNotOrders(t *testing.T) {
	raw := `{"decision":"NO_TRADE","side":"FLAT","watchlist":[{"symbol":"xyz:SP500","bias":"long"}]}`
	pool := []string{"xyz:SP500"}
	price := map[string]float64{"xyz:SP500": 5000.0}

	normalized, _, _ := NormalizeAIResponse(raw, pool, price, 1000, 0)
	ds := parseNormalized(t, normalized)
	if len(ds) != 1 {
		t.Fatalf("expected 1 decision, got %d: %+v", len(ds), ds)
	}
	for _, d := range ds {
		if d.Action != "wait" {
			t.Fatalf("watchlist must not become an order; got action %q for %s", d.Action, d.Symbol)
		}
	}
}

// Sample 3: action verbs NO_TRADE/HOLD/LONG/OPEN_POSITION, alias reason fields,
// 0-1 confidences, and the non-USD position_size trap. Includes the one happy
// path that must actually open, to prove the layer is not over-aggressive.
func TestNormalize_Sample3_ActionVariants(t *testing.T) {
	pool := []string{"BTCUSDT"}
	price := map[string]float64{"BTCUSDT": 100000.0}

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"hold", `{"action":"HOLD","symbol":"BTCUSDT","rationale":"chop"}`, "wait"},
		{"no_trade", `{"signal":"NO_TRADE"}`, "wait"},
		// bare LONG as the action (not open_long) must NOT open.
		{"bare_long", `{"action":"LONG","symbol":"BTCUSDT","notional_usd":200,"stop_loss":90000,"take_profit":110000,"confidence":0.8}`, "wait"},
		// open_position but size is the non-USD position_size field → wait.
		{"open_no_usd", `{"action":"open_position","side":"long","symbol":"BTCUSDT","position_size":0.01,"stop_loss":90000,"take_profit":110000,"confidence":0.8}`, "wait"},
		// fully valid open → open_long.
		{"open_ok", `{"action":"open_position","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":0.82,"reason":"trend"}`, "open_long"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalized, _, _ := NormalizeAIResponse(tc.raw, pool, price, 1000, 0)
			ds := parseNormalized(t, normalized)
			if len(ds) != 1 {
				t.Fatalf("expected 1 decision, got %d: %+v", len(ds), ds)
			}
			if ds[0].Action != tc.want {
				t.Fatalf("action = %q, want %q\nnormalized:\n%s", ds[0].Action, tc.want, normalized)
			}
			if tc.name == "open_ok" {
				if ds[0].Symbol != "BTCUSDT" {
					t.Fatalf("symbol = %q, want BTCUSDT", ds[0].Symbol)
				}
				if ds[0].PositionSizeUSD != 200 {
					t.Fatalf("position_size_usd = %v, want 200", ds[0].PositionSizeUSD)
				}
				if ds[0].Confidence != 82 {
					t.Fatalf("confidence = %d, want 82 (0.82*100)", ds[0].Confidence)
				}
				if ds[0].Leverage != 3 {
					t.Fatalf("leverage = %d, want 3", ds[0].Leverage)
				}
			}
		})
	}
}

// A long open whose SL/TP straddle the price the wrong way must be rejected.
func TestNormalize_PriceDirectionMismatch(t *testing.T) {
	// open_long but stop_loss > current > take_profit (inverted) → wait.
	raw := `{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"stop_loss":110000,"take_profit":90000,"confidence":90}`
	pool := []string{"BTCUSDT"}
	price := map[string]float64{"BTCUSDT": 100000.0}

	normalized, _, _ := NormalizeAIResponse(raw, pool, price, 1000, 0)
	ds := parseNormalized(t, normalized)
	if len(ds) != 1 || ds[0].Action != "wait" {
		t.Fatalf("inverted SL/TP must be wait, got %+v", ds)
	}
}

// A symbol outside the candidate pool must never be opened.
func TestNormalize_SymbolNotInPool(t *testing.T) {
	raw := `{"action":"open_long","symbol":"DOGEUSDT","notional_usd":200,"stop_loss":0.1,"take_profit":0.3,"confidence":90}`
	pool := []string{"BTCUSDT"}
	price := map[string]float64{"DOGEUSDT": 0.2}

	normalized, _, _ := NormalizeAIResponse(raw, pool, price, 1000, 0)
	ds := parseNormalized(t, normalized)
	if len(ds) != 1 || ds[0].Action != "wait" {
		t.Fatalf("out-of-pool symbol must be wait, got %+v", ds)
	}
}

// Extraction paths: <decision>-wrapped fenced array, and a top-level "orders"
// envelope. Both must yield the correct actions.
func TestNormalize_ExtractionPaths(t *testing.T) {
	pool := []string{"BTCUSDT"}
	price := map[string]float64{"BTCUSDT": 100000.0}

	t.Run("decision_tag_fenced_array", func(t *testing.T) {
		raw := "thinking...\n<decision>\n```json\n[{\"action\":\"open_long\",\"symbol\":\"BTCUSDT\",\"notional_usd\":200,\"leverage\":3,\"stop_loss\":90000,\"take_profit\":110000,\"confidence\":80}]\n```\n</decision>"
		ds := parseNormalized(t, mustNormalize(t, raw, pool, price))
		if len(ds) != 1 || ds[0].Action != "open_long" {
			t.Fatalf("expected open_long, got %+v", ds)
		}
	})

	t.Run("orders_envelope", func(t *testing.T) {
		raw := `{"orders":[{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80}]}`
		ds := parseNormalized(t, mustNormalize(t, raw, pool, price))
		if len(ds) != 1 || ds[0].Action != "open_long" {
			t.Fatalf("expected open_long from orders envelope, got %+v", ds)
		}
	})
}

// No JSON anywhere → overall fail-safe wait.
func TestNormalize_NoJSON_FailSafeWait(t *testing.T) {
	raw := "I considered the market but decided to provide only prose, no JSON."
	normalized, _, reason := NormalizeAIResponse(raw, []string{"BTCUSDT"}, map[string]float64{"BTCUSDT": 100000}, 1000, 0)
	if reason != "no_json_found" {
		t.Fatalf("reason = %q, want no_json_found", reason)
	}
	ds := parseNormalized(t, normalized)
	if len(ds) != 1 || ds[0].Action != "wait" {
		t.Fatalf("no-JSON must be wait, got %+v", ds)
	}
}

func mustNormalize(t *testing.T, raw string, pool []string, price map[string]float64) string {
	t.Helper()
	n, _, _ := NormalizeAIResponse(raw, pool, price, 1000, 0)
	return n
}

// ---------------------------------------------------------------------------
// Hardening cases from acceptance review (extraction priority, ambiguous
// direction, action/side conflict, duplicate-key conflict, missing leverage).
// ---------------------------------------------------------------------------

var hardenPool = []string{"BTCUSDT"}
var hardenPrice = map[string]float64{"BTCUSDT": 100000.0}

func assertSingleAction(t *testing.T, raw, want string) {
	t.Helper()
	ds := parseNormalized(t, mustNormalize(t, raw, hardenPool, hardenPrice))
	if len(ds) != 1 {
		t.Fatalf("expected 1 decision, got %d: %+v", len(ds), ds)
	}
	if ds[0].Action != want {
		t.Fatalf("action = %q, want %q (raw: %s)", ds[0].Action, want, raw)
	}
}

// A <decision> tag with no valid JSON inside must NOT scavenge JSON from the
// surrounding text (a real open array sits after the empty tag here).
func TestNormalize_DecisionTagEmpty_NoScavenge(t *testing.T) {
	raw := `<decision>no json here</decision>` + "\n" +
		`[{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80}]`
	assertSingleAction(t, raw, "wait")
}

// Fenced ```json decision block must win over a prose/example JSON that
// appears earlier in the text.
func TestNormalize_FencedBlockBeatsProseJSON(t *testing.T) {
	raw := `For example {"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"leverage":5,"stop_loss":90000,"take_profit":110000} would be aggressive.` +
		"\n\n```json\n[{\"action\":\"NO_TRADE\"}]\n```"
	// The fenced block is NO_TRADE → wait; the earlier prose open must be ignored.
	assertSingleAction(t, raw, "wait")
}

// Ambiguous / negated side values must resolve to wait, never a direction.
func TestNormalize_AmbiguousSideMustWait(t *testing.T) {
	for _, side := range []string{"not long", "long/short", "avoid short", "flat", ""} {
		raw := `{"action":"open_position","side":"` + side +
			`","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
		assertSingleAction(t, raw, "wait")
	}
}

// An explicit open_long action contradicted by side:short is ambiguous → wait.
func TestNormalize_ActionSideConflictMustWait(t *testing.T) {
	raw := `{"action":"open_long","side":"short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
	assertSingleAction(t, raw, "wait")
}

// An explicit open_long with a side field that is PRESENT but unresolved
// ("flat"/"not long") is ambiguous → wait (the action alone is not enough when
// a side field exists and disagrees/garbles).
func TestNormalize_ExplicitOpenWithAmbiguousSidePresentMustWait(t *testing.T) {
	for _, side := range []string{"flat", "not long", "long/short"} {
		raw := `{"action":"open_long","side":"` + side +
			`","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
		assertSingleAction(t, raw, "wait")
	}
}

// An explicit open_long with NO side field still opens (the action is the
// directive) — proves the present-vs-absent distinction is correct.
func TestNormalize_ExplicitOpenNoSideOpens(t *testing.T) {
	raw := `{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80}`
	assertSingleAction(t, raw, "open_long")
}

// A side field that exists but is empty / null / non-string is PRESENT yet
// unresolved → ambiguous → wait (key existence counts, not just a usable value).
func TestNormalize_SidePresentButEmptyMustWait(t *testing.T) {
	for _, sideJSON := range []string{`"side":""`, `"side":null`, `"side":123`} {
		raw := `{"action":"open_long",` + sideJSON +
			`,"symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
		assertSingleAction(t, raw, "wait")
	}
}

// A ```jsonc / second-```json edge: only a strict ```json fence is honored, and
// the first json fence that actually contains JSON wins.
func TestNormalize_StrictJSONFenceSelection(t *testing.T) {
	// ```jsonc holds an open; the real ```json holds NO_TRADE → must pick json.
	t.Run("jsonc_before_json", func(t *testing.T) {
		raw := "```jsonc\n[{\"action\":\"open_long\",\"symbol\":\"BTCUSDT\",\"notional_usd\":200,\"leverage\":5,\"stop_loss\":90000,\"take_profit\":110000}]\n```\n" +
			"```json\n[{\"action\":\"NO_TRADE\"}]\n```"
		assertSingleAction(t, raw, "wait")
	})
	// First ```json fence has no JSON; the second ```json fence does.
	t.Run("first_json_fence_empty", func(t *testing.T) {
		raw := "```json\nno json here\n```\n" +
			"```json\n[{\"action\":\"open_long\",\"symbol\":\"BTCUSDT\",\"notional_usd\":200,\"leverage\":3,\"stop_loss\":90000,\"take_profit\":110000,\"confidence\":80}]\n```"
		assertSingleAction(t, raw, "open_long")
	})
}

// A valid order alongside a malformed (non-object) element in an orders array:
// the malformed element is ignored and must never produce a spurious open; the
// valid sibling still opens.
func TestNormalize_OrdersMixedElements(t *testing.T) {
	raw := `{"orders":[{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80},"garbage"]}`
	ds := parseNormalized(t, mustNormalize(t, raw, hardenPool, hardenPrice))
	if len(ds) != 1 || ds[0].Action != "open_long" {
		t.Fatalf("expected only the valid order to open, got %+v", ds)
	}
}

// Duplicate case-variant keys with conflicting values must be deterministic:
// the field reads as missing → wait (never a random winner).
func TestNormalize_DuplicateKeyConflictMustWait(t *testing.T) {
	raw := `{"action":"NO_TRADE","Action":"open_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
	assertSingleAction(t, raw, "wait")
}

// A conflicting side family (side:"flat" vs Side:"short") must not be reduced
// to "missing" and let an explicit open_long through — the conflict itself
// forces wait.
func TestNormalize_SideConflictBeatsExplicitOpen(t *testing.T) {
	raw := `{"action":"open_long","side":"flat","Side":"short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
	assertSingleAction(t, raw, "wait")
}

// A conflicting action family must not fall back to a same-meaning alias.
func TestNormalize_ActionConflictBeatsAlias(t *testing.T) {
	// action/Action conflict, with a "decision" alias that would otherwise open.
	raw := `{"action":"NO_TRADE","Action":"hold","decision":"open_long","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
	assertSingleAction(t, raw, "wait")
}

// A conflicting symbol family must not fall back to ticker.
func TestNormalize_SymbolConflictBeatsTicker(t *testing.T) {
	raw := `{"action":"open_long","symbol":"BTCUSDT","Symbol":"ETHUSDT","ticker":"BTCUSDT","side":"long","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
	assertSingleAction(t, raw, "wait")
}

// ---------------------------------------------------------------------------
// variant-style schema: decision/take_profit_1/position_size_pct_equity and nested
// position_sizing{}/risk_management{} envelopes.
// ---------------------------------------------------------------------------

// Flat variant schema: decision verb, take_profit_1 (with an ignored take_profit_2),
// and percent-of-equity sizing converted with equity → open_long.
func TestNormalize_VariantFlatSchemaOpens(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":2,"stop_loss":90000,"take_profit_1":110000,"take_profit_2":115000,"position_size_pct_equity":20,"confidence":0.62}`
	ds := parseNormalized(t, mustNormalize(t, raw, hardenPool, hardenPrice))
	if len(ds) != 1 || ds[0].Action != "open_long" {
		t.Fatalf("expected open_long from variant flat schema, got %+v", ds)
	}
	if ds[0].TakeProfit != 110000 {
		t.Fatalf("expected take_profit_1 used as take_profit, got %v", ds[0].TakeProfit)
	}
	if ds[0].PositionSizeUSD != 200 { // 20% of equity 1000
		t.Fatalf("expected 200 USD from 20%% of 1000 equity, got %v", ds[0].PositionSizeUSD)
	}
}

// Nested variant schema: leverage/notional inside position_sizing{}, sl/tp inside
// risk_management{} → flattened → open_long.
func TestNormalize_VariantNestedEnvelopeOpens(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","confidence":0.68,` +
		`"position_sizing":{"suggested_notional_usdt":200,"leverage":3},` +
		`"risk_management":{"stop_loss":90000,"take_profit_1":110000}}`
	ds := parseNormalized(t, mustNormalize(t, raw, hardenPool, hardenPrice))
	if len(ds) != 1 || ds[0].Action != "open_long" {
		t.Fatalf("expected open_long from nested envelope, got %+v", ds)
	}
	if ds[0].PositionSizeUSD != 200 || ds[0].Leverage != 3 {
		t.Fatalf("nested notional/leverage not extracted, got %+v", ds[0])
	}
}

// Percent-of-equity sizing with NO equity available cannot be converted → wait.
func TestNormalize_VariantPctSizeNoEquityMustWait(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":2,"stop_loss":90000,"take_profit_1":110000,"position_size_pct_equity":20}`
	n, _, _ := NormalizeAIResponse(raw, hardenPool, hardenPrice, 0, 0)
	ds := parseNormalized(t, n)
	if len(ds) != 1 || ds[0].Action != "wait" {
		t.Fatalf("pct size without equity must wait, got %+v", ds)
	}
}

// A margin-denominated field must NEVER be treated as a USD notional size →
// without a real notional/pct the open fails safe to wait.
func TestNormalize_VariantMarginNotTreatedAsSize(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":3,"stop_loss":90000,"take_profit":110000,"suggested_margin_usdt":50}`
	assertSingleAction(t, raw, "wait")
}

// A leverage that disagrees between the top level and a nested envelope is
// ambiguous → wait (the flatten conflict must not silently pick one).
func TestNormalize_VariantNestedLeverageConflictMustWait(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":5,"stop_loss":90000,"take_profit":110000,"notional_usd":200,"position_sizing":{"leverage":3}}`
	assertSingleAction(t, raw, "wait")
}

// Percent sizing outside the sane [1,100] range is unit-ambiguous (0.5 could be
// 0.5% or 50%) or absurd (>100) → not converted → wait.
func TestNormalize_VariantPctSizeOutOfRangeMustWait(t *testing.T) {
	for _, pct := range []string{"0.5", "0", "150"} {
		raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":2,"stop_loss":90000,"take_profit_1":110000,"position_size_pct_equity":` + pct + `}`
		ds := parseNormalized(t, mustNormalize(t, raw, hardenPool, hardenPrice))
		if len(ds) != 1 || ds[0].Action != "wait" {
			t.Fatalf("pct=%s out of range must wait, got %+v", pct, ds)
		}
	}
}

// NaN / Inf numeric strings must never pass a money-path numeric gate → wait.
func TestNormalize_NaNInfMustWait(t *testing.T) {
	raws := []string{
		`{"action":"open_long","symbol":"BTCUSDT","leverage":3,"notional_usd":200,"stop_loss":"NaN","take_profit":110000}`,
		`{"action":"open_long","symbol":"BTCUSDT","leverage":3,"notional_usd":"+Inf","stop_loss":90000,"take_profit":110000}`,
		`{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":2,"stop_loss":90000,"take_profit_1":110000,"position_size_pct_equity":"+Inf"}`,
	}
	for _, raw := range raws {
		assertSingleAction(t, raw, "wait")
	}
}

// Two case-variant envelope containers that disagree must not silently pick one
// → the per-key conflict forces wait.
func TestNormalize_VariantEnvelopeCaseConflictMustWait(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","stop_loss":90000,"take_profit":110000,` +
		`"position_sizing":{"leverage":3,"notional_usd":200},` +
		`"Position_Sizing":{"leverage":5,"notional_usd":200}}`
	assertSingleAction(t, raw, "wait")
}

// Alias-family conflicts (two different alias keys of the same field with
// disagreeing values) must wait, not priority-pick the first alias.
func TestNormalize_AliasFamilyConflictsMustWait(t *testing.T) {
	cases := map[string]string{
		"action_vs_decision": `{"action":"open_long","decision":"NO_TRADE","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`,
		"side_vs_direction":  `{"action":"open_long","side":"long","direction":"short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`,
		"symbol_vs_ticker":   `{"action":"open_long","side":"long","symbol":"BTCUSDT","ticker":"ETHUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`,
		"usd_alias_conflict": `{"action":"open_long","side":"long","symbol":"BTCUSDT","notional_usd":200,"notional_usdt":300,"leverage":3,"stop_loss":90000,"take_profit":110000}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			assertSingleAction(t, raw, "wait")
		})
	}
}

// Agreeing aliases (same field repeated with the same value) must NOT trip the
// conflict guard — a valid open still opens.
func TestNormalize_AgreeingAliasesStillOpen(t *testing.T) {
	raw := `{"action":"open_long","signal":"open_long","symbol":"BTCUSDT","ticker":"BTCUSDT","side":"long","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80}`
	assertSingleAction(t, raw, "open_long")
}

// Duplicate same-name JSON keys are collapsed by encoding/json to the last
// value, hiding the conflict; such a fragment must fail safe to wait.
func TestNormalize_DuplicateJSONKeyMustWait(t *testing.T) {
	// {"action":"NO_TRADE","action":"open_long", ...valid open...}
	raw := `{"action":"NO_TRADE","action":"open_long","symbol":"BTCUSDT","side":"long","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`
	assertSingleAction(t, raw, "wait")
}

// Duplicate key inside an array element must also fail safe.
func TestNormalize_DuplicateJSONKeyInArrayMustWait(t *testing.T) {
	raw := `[{"action":"open_long","symbol":"BTCUSDT","symbol":"ETHUSDT","side":"long","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}]`
	assertSingleAction(t, raw, "wait")
}

// Missing leverage on an open must downgrade to wait (it would otherwise hard-
// error the whole downstream parse).
func TestNormalize_MissingLeverageMustWait(t *testing.T) {
	raw := `{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"stop_loss":90000,"take_profit":110000,"confidence":80}`
	assertSingleAction(t, raw, "wait")
}

// Missing current price for the symbol must downgrade to wait.
func TestNormalize_MissingCurrentPriceMustWait(t *testing.T) {
	raw := `{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80}`
	ds := parseNormalized(t, mustNormalize(t, raw, hardenPool, map[string]float64{})) // empty price map
	if len(ds) != 1 || ds[0].Action != "wait" {
		t.Fatalf("missing price must be wait, got %+v", ds)
	}
}

// close_long/close_short pass through (no order params, not pool-gated) so the
// model can actually exit a held position; a symbol-less close fails safe to wait.
func TestNormalize_ClosePassesThrough(t *testing.T) {
	assertSingleAction(t, `{"action":"close_long","symbol":"BTCUSDT"}`, "close_long")
	assertSingleAction(t, `{"action":"close_short","symbol":"BTCUSDT"}`, "close_short")
	// Not gated by the candidate pool — a held position may be on a symbol GINA
	// did not select this cycle.
	assertSingleAction(t, `{"action":"close_long","symbol":"DOGEUSDT"}`, "close_long")
	// A symbol-less close is ambiguous → wait.
	assertSingleAction(t, `{"action":"close_long"}`, "wait")
	// A present side must agree with the close direction (same rule as opens): a
	// contradicting or unresolved side could close the wrong leg → wait.
	assertSingleAction(t, `{"action":"close_long","symbol":"BTCUSDT","side":"short"}`, "wait")
	assertSingleAction(t, `{"action":"close_short","symbol":"BTCUSDT","side":"long"}`, "wait")
	assertSingleAction(t, `{"action":"close_long","symbol":"BTCUSDT","side":"flat"}`, "wait")
	// A matching side passes.
	assertSingleAction(t, `{"action":"close_long","symbol":"BTCUSDT","side":"long"}`, "close_long")
}

// A fully valid short opens; an inverted SL/TP short is rejected.
func TestNormalize_OpenShort(t *testing.T) {
	ok := `{"action":"open_short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":110000,"take_profit":90000,"confidence":80}`
	assertSingleAction(t, ok, "open_short")

	inverted := `{"action":"open_short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80}`
	assertSingleAction(t, inverted, "wait")
}

// "orders" present but not an array is a malformed envelope → wait.
func TestNormalize_OrdersNotArrayMustWait(t *testing.T) {
	assertSingleAction(t, `{"orders":"BTCUSDT"}`, "wait")
}

// normalizerMode parses the switch safely: empty/unknown → off.
func TestNormalizerMode(t *testing.T) {
	cases := map[string]string{
		"":        normalizerOff,
		"off":     normalizerOff,
		"garbage": normalizerOff,
		"on":      normalizerOn,
		"ON":      normalizerOn,
		" shadow": normalizerShadow,
	}
	for env, want := range cases {
		t.Setenv("NOFX_NORMALIZER", env)
		if got := normalizerMode(); got != want {
			t.Fatalf("NOFX_NORMALIZER=%q → mode %q, want %q", env, got, want)
		}
	}
}

// redactForLog masks credential-looking values in both JSON and header forms.
func TestRedactForLog(t *testing.T) {
	for _, in := range []string{
		`{"api_key":"sk-secret-123"}`,
		`Authorization: Bearer sk-secret-123`,
		`token=sk-secret-123`,
	} {
		if got := redactForLog(in); strings.Contains(got, "sk-secret-123") {
			t.Fatalf("redactForLog failed to mask secret in %q → %q", in, got)
		}
	}
}

// The GINA schema commonly omits leverage; with a positive defaultLeverage the
// otherwise-valid open is injected with that leverage and opens, instead of the
// missing-leverage fail-safe wait. This is the Change-A core fix.
func TestNormalize_MissingLeverageInjectsDefaultForGina(t *testing.T) {
	raw := `{"decision":"OPEN_POSITION","side":"LONG","symbol":"XLMUSDT","suggested_notional_usdt":40,"entry_price":0.5,"stop_loss":0.45,"take_profit":0.65}`
	pool := []string{"XLMUSDT"}
	price := map[string]float64{"XLMUSDT": 0.5} // between SL and TP
	n, _, _ := NormalizeAIResponse(raw, pool, price, 95, 3)
	ds := parseNormalized(t, n)
	if len(ds) != 1 || ds[0].Action != "open_long" {
		t.Fatalf("missing-leverage GINA open must inject and open_long, got %+v", ds)
	}
	if ds[0].Leverage != 3 {
		t.Fatalf("expected injected leverage 3, got %v", ds[0].Leverage)
	}
	if ds[0].PositionSizeUSD != 40 {
		t.Fatalf("expected notional 40 from suggested_notional_usdt, got %v", ds[0].PositionSizeUSD)
	}
}

// With defaultLeverage 0 (non-GINA) a missing leverage still fails safe to wait —
// non-GINA behavior is byte-for-byte unchanged.
func TestNormalize_MissingLeverageNoDefaultWaits(t *testing.T) {
	raw := `{"decision":"OPEN_POSITION","side":"LONG","symbol":"XLMUSDT","suggested_notional_usdt":40,"entry_price":0.5,"stop_loss":0.45,"take_profit":0.65}`
	pool := []string{"XLMUSDT"}
	price := map[string]float64{"XLMUSDT": 0.5}
	n, _, _ := NormalizeAIResponse(raw, pool, price, 95, 0)
	ds := parseNormalized(t, n)
	if len(ds) != 1 || ds[0].Action != "wait" {
		t.Fatalf("missing-leverage with no default must wait, got %+v", ds)
	}
}

// position_size_usdt is recognized as a USD notional alias (new), so an otherwise
// valid open sizes from it.
func TestNormalize_PositionSizeUsdtAlias(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":3,"stop_loss":90000,"take_profit":110000,"position_size_usdt":250}`
	ds := parseNormalized(t, mustNormalize(t, raw, hardenPool, hardenPrice))
	if len(ds) != 1 || ds[0].Action != "open_long" {
		t.Fatalf("position_size_usdt open expected, got %+v", ds)
	}
	if ds[0].PositionSizeUSD != 250 {
		t.Fatalf("expected 250 from position_size_usdt, got %v", ds[0].PositionSizeUSD)
	}
}

// position_size_usdt disagreeing with position_size_usd is an alias-family conflict
// → wait, never a silent pick.
func TestNormalize_PositionSizeUsdtConflictWaits(t *testing.T) {
	raw := `{"decision":"OPEN_LONG","symbol":"BTCUSDT","leverage":3,"stop_loss":90000,"take_profit":110000,"position_size_usd":200,"position_size_usdt":250}`
	assertSingleAction(t, raw, "wait")
}

// ---------------------------------------------------------------------------
// Open-action synonyms. Audit of all decision_records.raw_response showed the
// model (gpt-5.5) frequently emits enter_long/enter_short/enter/enter_trade as
// the verb; the normalizer must recognize them as open intent while still
// crossing every existing open safety gate. Bare direction words and close/exit
// flavored verbs are deliberately NOT mapped.
// ---------------------------------------------------------------------------

// enter_long/enter_short map directly to the standard open actions; enter and
// enter_trade resolve via an explicit side; a missing or conflicting side fails
// safe to wait.
func TestNormalize_EnterSynonyms(t *testing.T) {
	assertSingleAction(t, `{"action":"enter_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000,"confidence":80}`, "open_long")
	assertSingleAction(t, `{"action":"enter_short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":110000,"take_profit":90000,"confidence":80}`, "open_short")
	assertSingleAction(t, `{"action":"enter","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`, "open_long")
	assertSingleAction(t, `{"action":"enter_trade","side":"short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":110000,"take_profit":90000}`, "open_short")
	// enter / enter_trade with NO resolvable direction must wait.
	assertSingleAction(t, `{"action":"enter","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`, "wait")
	assertSingleAction(t, `{"action":"enter_trade","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`, "wait")
	// enter_long contradicted by side:short is ambiguous → wait (side gate holds).
	assertSingleAction(t, `{"action":"enter_long","side":"short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`, "wait")
}

// Bare direction words (and bare buy/sell) are NOT open directives even with a
// consistent side — they remain ambiguous → wait, guarding against opening on a
// mere opinion word.
func TestNormalize_BareDirectionWordsStayWait(t *testing.T) {
	for _, raw := range []string{
		`{"action":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`,
		`{"action":"long","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`,
		`{"action":"short","side":"short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":110000,"take_profit":90000}`,
		`{"action":"buy","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`,
		`{"action":"sell","side":"short","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":110000,"take_profit":90000}`,
	} {
		assertSingleAction(t, raw, "wait")
	}
}

// Only close_long/close_short pass through; exit/take_profit flavored verbs must
// never be invented into a close → they collapse to wait.
func TestNormalize_NoCloseExitSynonyms(t *testing.T) {
	for _, raw := range []string{
		`{"action":"exit_long","symbol":"BTCUSDT"}`,
		`{"action":"take_profit","symbol":"BTCUSDT"}`,
		`{"action":"exit","side":"long","symbol":"BTCUSDT"}`,
	} {
		assertSingleAction(t, raw, "wait")
	}
}

// enter_position was deliberately excluded (audit showed 0 occurrences); it must
// still collapse to wait even with a clear side, locking the exclusion.
func TestNormalize_EnterPositionExcludedStaysWait(t *testing.T) {
	assertSingleAction(t, `{"action":"enter_position","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`, "wait")
}

// An enter_long verb conflicting with a same-family alias (decision/signal) is an
// alias-family conflict → wait, never a silent priority pick.
func TestNormalize_EnterSynonymAliasConflictWaits(t *testing.T) {
	assertSingleAction(t, `{"action":"enter_long","decision":"no_trade","side":"long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`, "wait")
}

// ENTER_LONG on the GINA path (missing leverage + positive defaultLeverage)
// injects the default leverage and opens — the real gpt-5.5 case.
func TestNormalize_EnterLongGinaLeverageInjection(t *testing.T) {
	raw := `{"decision":"ENTER_LONG","symbol":"XLMUSDT","suggested_notional_usdt":40,"stop_loss":0.45,"take_profit":0.65}`
	pool := []string{"XLMUSDT"}
	price := map[string]float64{"XLMUSDT": 0.5}
	n, _, _ := NormalizeAIResponse(raw, pool, price, 95, 3)
	ds := parseNormalized(t, n)
	if len(ds) != 1 || ds[0].Action != "open_long" {
		t.Fatalf("ENTER_LONG GINA open must inject leverage and open_long, got %+v", ds)
	}
	if ds[0].Leverage != 3 {
		t.Fatalf("expected injected leverage 3, got %v", ds[0].Leverage)
	}
}

// BLAST-RADIUS LOCK (do not delete — this is intended, not a bug): the normalizer
// is global, so a NON-GINA caller (defaultLeverage=0) emitting enter_long still
// fails safe to wait when leverage is missing, but WILL open_long when every open
// field is present. This is the documented behavior change from the pre-synonym
// wait. If it must change, gate the enter_* synonyms by source explicitly.
func TestNormalize_EnterLongNonGinaBlastRadius(t *testing.T) {
	// Non-GINA (assertSingleAction uses defaultLeverage=0): missing leverage still
	// fails safe to wait — the pre-synonym safety is unchanged.
	assertSingleAction(t, `{"action":"enter_long","symbol":"BTCUSDT","notional_usd":200,"stop_loss":90000,"take_profit":110000}`, "wait")
	// Every open field present → open_long. This is the documented, intended
	// behavior change from the pre-synonym wait, NOT a bug.
	assertSingleAction(t, `{"action":"enter_long","symbol":"BTCUSDT","notional_usd":200,"leverage":3,"stop_loss":90000,"take_profit":110000}`, "open_long")
}
