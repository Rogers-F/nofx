package kernel

import "testing"

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

	normalized, changed, _ := NormalizeAIResponse(raw, pool, price)
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

	normalized, _, _ := NormalizeAIResponse(raw, pool, price)
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
			normalized, _, _ := NormalizeAIResponse(tc.raw, pool, price)
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

	normalized, _, _ := NormalizeAIResponse(raw, pool, price)
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

	normalized, _, _ := NormalizeAIResponse(raw, pool, price)
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
		raw := "thinking...\n<decision>\n```json\n[{\"action\":\"open_long\",\"symbol\":\"BTCUSDT\",\"notional_usd\":200,\"stop_loss\":90000,\"take_profit\":110000,\"confidence\":80}]\n```\n</decision>"
		ds := parseNormalized(t, mustNormalize(t, raw, pool, price))
		if len(ds) != 1 || ds[0].Action != "open_long" {
			t.Fatalf("expected open_long, got %+v", ds)
		}
	})

	t.Run("orders_envelope", func(t *testing.T) {
		raw := `{"orders":[{"action":"open_long","symbol":"BTCUSDT","notional_usd":200,"stop_loss":90000,"take_profit":110000,"confidence":80}]}`
		ds := parseNormalized(t, mustNormalize(t, raw, pool, price))
		if len(ds) != 1 || ds[0].Action != "open_long" {
			t.Fatalf("expected open_long from orders envelope, got %+v", ds)
		}
	})
}

// No JSON anywhere → overall fail-safe wait.
func TestNormalize_NoJSON_FailSafeWait(t *testing.T) {
	raw := "I considered the market but decided to provide only prose, no JSON."
	normalized, _, reason := NormalizeAIResponse(raw, []string{"BTCUSDT"}, map[string]float64{"BTCUSDT": 100000})
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
	n, _, _ := NormalizeAIResponse(raw, pool, price)
	return n
}
