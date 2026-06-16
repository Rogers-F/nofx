package kernel

// ============================================================================
// Response Normalization Layer (additive, non-invasive)
// ============================================================================
//
// This layer translates free-form model output (single objects, custom
// schemas, alias field names, 0-1 confidences, etc.) into the standard
//
//	<reasoning>...</reasoning>
//	<decision>
//	```json
//	[ {...} ]
//	```
//	</decision>
//
// format that the existing parser (extractDecisions) already understands.
//
// It is controlled by the NOFX_NORMALIZER env switch and is OFF by default:
//   - off    : never invoked; behavior is byte-for-byte identical to before.
//   - on      : the normalized response is fed to the existing parser.
//   - shadow : the normalized response is computed and logged for comparison,
//              but the ORIGINAL response is still fed to the parser (no effect
//              on real trading — used to validate the layer before enabling).
//
// Fail-safe contract: any parse failure, missing field, ambiguous direction,
// unit ambiguity, price/direction contradiction, or out-of-pool symbol causes
// that decision to collapse to {symbol, action:"wait"}. The layer NEVER
// invents an open direction or side; the worst case is always "wait".
// ============================================================================

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const (
	normalizerOff    = "off"
	normalizerOn     = "on"
	normalizerShadow = "shadow"
)

// ginaDefaultLeverage is the leverage injected for the GINA source when the model
// omits leverage on an otherwise-valid open. Notional is independently capped at
// ≤ tier ratio × equity and SL/TP are mandatory, so leverage mainly affects the
// liquidation buffer; a small value keeps that buffer wide. The injected value is
// still per-tier clamped by validateDecision.
const ginaDefaultLeverage = 3

// reSecretKV redacts obvious credential-looking key/value pairs before logging.
// The optional quotes tolerate JSON-style `"api_key":"..."` as well as
// header/env style `api_key=...` / `authorization: ...`.
var reSecretKV = regexp.MustCompile(`(?i)(authorization|api[-_]?key|secret|token|bearer|password)["']?\s*[:=]\s*["']?[^\s,"']+`)

// reBearer redacts `Bearer <token>` (where the value is space-separated and
// would otherwise slip past reSecretKV's no-whitespace value matcher).
var reBearer = regexp.MustCompile(`(?i)bearer\s+[^\s,"']+`)

// reJSONCodeFence captures the content of a ```json ... ``` code fence. The
// mandatory whitespace after the "json" label pins the tag to exactly "json"
// (a markdown info string ends at a newline), so neighbours like ```jsonc /
// ```json5 / ```json_example can never outrank the real decision fence.
var reJSONCodeFence = regexp.MustCompile(`(?is)` + "```json" + `\s(.*?)` + "```")

// normalizerMode reads NOFX_NORMALIZER and returns a canonical mode. Any
// unrecognized / empty value maps to "off" so the safe default is preserved.
func normalizerMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NOFX_NORMALIZER"))) {
	case normalizerOn:
		return normalizerOn
	case normalizerShadow:
		return normalizerShadow
	default:
		return normalizerOff
	}
}

// redactForLog masks credential-looking substrings and caps length so raw
// model output can be logged without leaking secrets or flooding logs.
func redactForLog(s string) string {
	s = reBearer.ReplaceAllString(s, "bearer ***")
	s = reSecretKV.ReplaceAllString(s, "$1=***")
	const maxRunes = 1000
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "...(truncated)"
	}
	return s
}

// NormalizeAIResponse converts a free-form model response into the NOFX
// standard format string.
//
//	raw           : the model's raw content.
//	candidatePool : symbols allowed to be opened this cycle (exact match only).
//	currentPrice  : current price per symbol, used for price/direction checks.
//	equity        : account equity in USDT, used only to convert a
//	                percent-of-equity position size into a USD notional. Pass 0
//	                when unknown — percent-based sizing then fails safe to wait.
//	defaultLeverage : leverage applied to an otherwise-valid open when the model
//	                omits leverage. Pass 0 to keep the legacy fail-safe (missing
//	                leverage → wait); a positive value is supplied ONLY for the
//	                GINA source so non-GINA behavior is byte-for-byte unchanged.
//
// Returns the normalized standard-format string, whether it differs from the
// raw input (changed), and a short reason code for logging.
func NormalizeAIResponse(raw string, candidatePool []string, currentPrice map[string]float64, equity float64, defaultLeverage int) (normalized string, changed bool, reason string) {
	pool := make(map[string]bool, len(candidatePool))
	for _, s := range candidatePool {
		if t := strings.TrimSpace(s); t != "" {
			pool[t] = true
		}
	}

	reasoning := extractReasoningText(raw)

	frag, ok := pickJSONFragment(raw)
	if !ok {
		out := buildNormalized(reasoning, []Decision{waitDecision("ALL", reasoning, "no_json_found")})
		return out, out != raw, "no_json_found"
	}

	// Duplicate same-name JSON keys (e.g. {"action":"NO_TRADE","action":"open_long"})
	// are silently collapsed by encoding/json to the last value, hiding the
	// conflict from lowerKeys/alias checks. On a money path that ambiguity must
	// fail safe: any duplicate key anywhere in the fragment → all wait.
	if hasDuplicateJSONKeys(frag) {
		out := buildNormalized(reasoning, []Decision{waitDecision("ALL", reasoning, "duplicate_json_keys")})
		return out, true, "duplicate_json_keys"
	}

	rawDecisions, code := extractRawDecisionObjects(frag)
	if code != "" {
		out := buildNormalized(reasoning, []Decision{waitDecision("ALL", reasoning, code)})
		return out, true, code
	}
	if len(rawDecisions) == 0 {
		out := buildNormalized(reasoning, []Decision{waitDecision("ALL", reasoning, "empty_decisions")})
		return out, true, "empty_decisions"
	}

	out := make([]Decision, 0, len(rawDecisions))
	waitCount := 0
	for _, rd := range rawDecisions {
		d := normalizeOne(rd, pool, currentPrice, equity, defaultLeverage)
		if d.Action == "wait" {
			waitCount++
		}
		out = append(out, d)
	}

	normalized = buildNormalized(reasoning, out)
	return normalized, normalized != raw, fmt.Sprintf("ok out=%d wait=%d", len(out), waitCount)
}

// extractRawDecisionObjects turns a JSON fragment into a list of candidate
// decision objects. A top-level array is used as-is; a top-level object uses
// its "orders" array when present, otherwise it is treated as a single
// decision. A "watchlist" is NEVER interpreted as an order array.
func extractRawDecisionObjects(frag string) ([]map[string]interface{}, string) {
	b := []byte(frag)

	var arr []map[string]interface{}
	if err := json.Unmarshal(b, &arr); err == nil {
		return arr, ""
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(b, &obj); err == nil {
		lo, _ := lowerKeys(obj)
		if ordersRaw, present := lo["orders"]; present {
			if list, isList := ordersRaw.([]interface{}); isList {
				return toMaps(list), ""
			}
			// "orders" present but not an array: malformed envelope, fail safe.
			return nil, "orders_not_array"
		}
		return []map[string]interface{}{obj}, ""
	}

	return nil, "json_parse_error"
}

// normalizeOne maps one free-form decision object to a standard Decision,
// downgrading to "wait" whenever any open-position safety gate is not met.
func normalizeOne(rawObj map[string]interface{}, pool map[string]bool, price map[string]float64, equity float64, defaultLeverage int) Decision {
	// Flatten known wrapper objects (e.g. position_sizing{}, risk_management{})
	// up to the top level so nested decisions are reachable; this also lower-cases
	// keys and reports case/placement conflicts exactly like lowerKeys.
	m, conflicts := flattenEnvelopes(rawObj)

	symbol := firstStr(m, "symbol", "ticker")
	reasoning := firstStr(m, "reasoning", "reason", "rationale")

	// A case-variant conflict on any trading-critical field family is ambiguous
	// — never fall back to an alias or treat the field as merely missing → wait.
	if hasCriticalConflict(conflicts) {
		return waitDecision(symbol, reasoning, "field_key_conflict")
	}

	// Same for alias-family conflicts: when two aliases of the same field appear
	// with disagreeing values (e.g. symbol vs ticker, action vs decision, side
	// vs direction, USD-size aliases), priority-picking the first would be a
	// silent guess on a money path → wait instead.
	for _, family := range aliasFamilies {
		if familyValueConflict(m, family...) {
			return waitDecision(symbol, reasoning, "field_alias_conflict")
		}
	}

	dir, sidePresent := resolveSide(m)
	action := normalizeAction(firstStr(m, "action", "decision", "signal"), dir)

	confidence := 0
	if c, ok := firstFloat(m, "confidence"); ok {
		if c > 0 && c < 1 {
			c *= 100 // 0-1 float convention → 0-100
		}
		if c < 0 {
			c = 0
		}
		confidence = int(c + 0.5)
	}

	// Non-open actions (wait/hold and — in v1 — close_*) never carry order
	// parameters; collapse them all to a plain "wait".
	if action != "open_long" && action != "open_short" {
		return waitDecision(symbol, reasoning, "non_open_action")
	}

	// Open-position gates — ALL must hold, else downgrade to "wait". Numbered in
	// evaluation order.

	// ① Direction must be unambiguous. If a side-like field is present, it must
	// agree with the action's direction; a present-but-unresolved side ("flat",
	// "not long", "long/short", ...) or a contradicting side is ambiguous → wait.
	// An absent side is fine — the explicit open_* action is itself the directive.
	if sidePresent {
		want := "long"
		if action == "open_short" {
			want = "short"
		}
		if dir != want {
			return waitDecision(symbol, reasoning, "ambiguous_or_conflicting_side")
		}
	}

	// USD size only from notional-denominated fields. Bare position_size/size/
	// qty/amount/quantity AND margin-denominated fields are explicitly NOT
	// treated as USD notional. If no USD notional is given, fall back to a
	// percent-of-equity size (e.g. variant-style "position_size_pct_equity"),
	// converted with the account equity.
	posUSD, hasUSD := firstFloat(m, "position_size_usd", "position_size_usdt", "notional_usd", "notional_usdt", "suggested_notional_usdt", "notional")
	if !hasUSD {
		if pct, ok := firstFloat(m, "position_size_pct_equity", "position_size_pct", "pct_equity"); ok && equity > 0 {
			if usd := pctEquityToUSD(pct, equity); usd > 0 {
				posUSD, hasUSD = usd, true
			}
		}
	}
	// Stop-loss / take-profit, tolerating common alias names. take_profit_2 is
	// deliberately NOT an alias — when both TP1 and TP2 are given, TP1 (the
	// nearest target) is used and TP2 ignored.
	sl, hasSL := firstFloat(m, "stop_loss", "sl", "stop_price", "stoploss")
	tp, hasTP := firstFloat(m, "take_profit", "take_profit_1", "tp", "tp1", "target_price", "takeprofit")
	leverage := 0
	if l, ok := firstFloat(m, "leverage"); ok {
		leverage = int(l + 0.5)
	}
	// GINA path: the configured prompt frequently omits leverage, which would
	// otherwise fail gate ③ below. When a positive defaultLeverage is supplied
	// (GINA-only at the call site) inject it for an otherwise-valid open. This
	// defaults ONLY leverage — direction, size, SL and TP are never invented — and
	// the value is still per-tier clamped downstream by validateDecision.
	// defaultLeverage==0 preserves the legacy missing-leverage → wait behavior.
	if leverage <= 0 && defaultLeverage > 0 {
		leverage = defaultLeverage
	}

	// ② symbol must exactly match the candidate pool (no fuzzy completion).
	if symbol == "" || !pool[symbol] {
		return waitDecision(symbol, reasoning, "symbol_not_in_pool")
	}
	// ③ leverage is a required open field downstream (validateDecision errors on
	// leverage<=0, which would fail the WHOLE parse, not just this decision);
	// missing/zero leverage therefore fails the gate here → wait.
	if leverage <= 0 {
		return waitDecision(symbol, reasoning, "missing_leverage")
	}
	// ④ USD field present and positive.
	if !hasUSD || posUSD <= 0 {
		return waitDecision(symbol, reasoning, "missing_usd_size")
	}
	// ⑤ both stop-loss and take-profit present and positive.
	if !hasSL || !hasTP || sl <= 0 || tp <= 0 {
		return waitDecision(symbol, reasoning, "missing_sl_tp")
	}
	// ⑥ price/direction consistency against the current price.
	cur, priceOK := price[symbol]
	if !priceOK || cur <= 0 {
		return waitDecision(symbol, reasoning, "no_current_price")
	}
	if action == "open_long" && !(sl < cur && cur < tp) {
		return waitDecision(symbol, reasoning, "price_dir_mismatch_long")
	}
	if action == "open_short" && !(tp < cur && cur < sl) {
		return waitDecision(symbol, reasoning, "price_dir_mismatch_short")
	}

	return Decision{
		Symbol:          symbol,
		Action:          action,
		Leverage:        leverage,
		PositionSizeUSD: posUSD,
		StopLoss:        sl,
		TakeProfit:      tp,
		Confidence:      confidence,
		Reasoning:       reasoning,
	}
}

// normalizeAction maps a raw action verb (plus a resolved direction) onto the
// standard action enum. Anything not unambiguously an open/close becomes
// "wait" — including bare LONG/SHORT/BUY/SELL/HOLD/NO_TRADE/FLAT.
func normalizeAction(rawAction, dir string) string {
	a := strings.ToLower(strings.TrimSpace(rawAction))
	a = strings.NewReplacer("-", "_", " ", "_").Replace(a)
	switch a {
	case "open_long":
		return "open_long"
	case "open_short":
		return "open_short"
	case "close_long":
		return "close_long"
	case "close_short":
		return "close_short"
	case "open_position", "open", "openposition":
		if dir == "long" {
			return "open_long"
		}
		if dir == "short" {
			return "open_short"
		}
		return "wait"
	default:
		return "wait"
	}
}

// resolveSide extracts an EXPLICIT long/short direction from side-like fields.
// Matching is exact (not substring) so ambiguous or negated values such as
// "long/short", "not long", "avoid short", or "flat" resolve to dir="" while
// reporting present=true — the caller treats a present-but-unresolved side as
// ambiguous and downgrades to wait. present=false means no side field at all.
func resolveSide(m map[string]interface{}) (dir string, present bool) {
	for _, k := range []string{"side", "direction", "position_side"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		// The first side-like key that EXISTS is authoritative — its mere
		// presence makes the field "present", even if empty/null/non-string.
		if s, isStr := v.(string); isStr {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "long":
				return "long", true
			case "short":
				return "short", true
			}
		}
		return "", true // present but unresolved (empty, null, non-string, other)
	}
	return "", false
}

// waitDecision builds a fail-safe "wait" decision, preserving the symbol when
// known and tagging the reason code for log/debug.
func waitDecision(symbol, reasoning, code string) Decision {
	if strings.TrimSpace(symbol) == "" {
		symbol = "UNKNOWN"
	}
	if strings.TrimSpace(reasoning) == "" {
		reasoning = "normalized to wait"
	}
	return Decision{
		Symbol:    symbol,
		Action:    "wait",
		Reasoning: fmt.Sprintf("%s [normalizer:%s]", reasoning, code),
	}
}

// buildNormalized renders decisions into the standard NOFX format string.
func buildNormalized(reasoning string, decisions []Decision) string {
	jsonBytes, err := json.Marshal(decisions)
	if err != nil {
		jsonBytes = []byte(`[{"symbol":"ALL","action":"wait","reasoning":"normalizer marshal error"}]`)
	}
	var b strings.Builder
	b.WriteString("<reasoning>")
	b.WriteString(sanitizeReasoning(reasoning))
	b.WriteString("</reasoning>\n<decision>\n```json\n")
	b.Write(jsonBytes)
	b.WriteString("\n```\n</decision>")
	return b.String()
}

// sanitizeReasoning strips structural tokens from reasoning text so it cannot
// break the surrounding <reasoning>/<decision> framing, and caps its length.
func sanitizeReasoning(s string) string {
	s = strings.NewReplacer(
		"<reasoning>", "",
		"</reasoning>", "",
		"<decision>", "",
		"</decision>", "",
		"```", "",
	).Replace(s)
	const maxRunes = 4000
	if r := []rune(s); len(r) > maxRunes {
		s = string(r[:maxRunes])
	}
	return strings.TrimSpace(s)
}

// extractReasoningText recovers human-readable reasoning: the <reasoning> tag,
// else text before <decision>, else text before the first JSON opener.
func extractReasoningText(raw string) string {
	if m := reReasoningTag.FindStringSubmatch(raw); m != nil && len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	if idx := strings.Index(raw, "<decision>"); idx > 0 {
		return strings.TrimSpace(raw[:idx])
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] == '{' || raw[i] == '[' {
			if i > 0 {
				return strings.TrimSpace(raw[:i])
			}
			break
		}
	}
	return ""
}

// ============================================================================
// JSON scanning helpers
// ============================================================================

// pickJSONFragment selects the decision JSON fragment following the contract
// priority, and — critically for a money path — does NOT scavenge unrelated
// JSON when a higher-priority container exists:
//
//	a. inside <decision>...</decision> ONLY (no fallback to the whole text, so
//	   prose / examples / watchlists outside the tag can never become orders);
//	b. else inside the first ```json code fence that contains valid JSON (gives
//	   fenced JSON priority over arbitrary prose JSON earlier in the text);
//	c. else the first balanced, valid JSON value anywhere in the text.
func pickJSONFragment(raw string) (string, bool) {
	if m := reDecisionTag.FindStringSubmatch(raw); m != nil && len(m) > 1 {
		return findFirstJSON(m[1])
	}
	for _, fm := range reJSONCodeFence.FindAllStringSubmatch(raw, -1) {
		if len(fm) > 1 {
			if frag, ok := findFirstJSON(fm[1]); ok {
				return frag, true
			}
		}
	}
	return findFirstJSON(raw)
}

// hasDuplicateJSONKeys reports whether any object in the JSON fragment contains
// the same key more than once. encoding/json would silently keep only the last
// occurrence, so this is the only place such a conflict can be detected.
func hasDuplicateJSONKeys(fragment string) bool {
	dec := json.NewDecoder(strings.NewReader(fragment))
	type frame struct {
		object    bool
		keys      map[string]bool
		expectKey bool
	}
	var stack []*frame
	markValueConsumed := func() {
		if len(stack) > 0 {
			if top := stack[len(stack)-1]; top.object {
				top.expectKey = true
			}
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF = clean end; any other error means malformed JSON, which
			// the normal parse path turns into a fail-safe wait anyway. Either
			// way, no duplicate key was found.
			return false
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{object: true, keys: map[string]bool{}, expectKey: true})
			case '[':
				stack = append(stack, &frame{object: false})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				markValueConsumed()
			}
		default:
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			if top.object && top.expectKey {
				if s, ok := tok.(string); ok {
					if top.keys[s] {
						return true
					}
					top.keys[s] = true
				}
				top.expectKey = false
			} else if top.object {
				top.expectKey = true
			}
		}
	}
}

// findFirstJSON returns the first balanced, syntactically valid JSON value
// (object or array) found in s.
func findFirstJSON(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '{' || s[i] == '[' {
			if frag, ok := matchBalanced(s, i); ok {
				return frag, true
			}
		}
	}
	return "", false
}

// matchBalanced returns the substring of s starting at the opener index that
// is balanced and valid JSON, honoring string literals and escapes.
func matchBalanced(s string, start int) (string, bool) {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				frag := s[start : i+1]
				if json.Valid([]byte(frag)) {
					return frag, true
				}
				return "", false
			}
		}
	}
	return "", false
}

// ============================================================================
// Field accessors (case-insensitive over lower-cased keys)
// ============================================================================

// lowerKeys lower-cases keys for case-insensitive lookups, and reports which
// lower-cased keys had a case-variant collision with DIFFERENT values (e.g.
// {"side":"flat","Side":"short"}). Colliding keys are dropped from the map so a
// reader never gets a non-deterministic winner from Go's map iteration order;
// the returned conflict set lets callers refuse to act on an ambiguous field
// rather than silently falling back to an alias.
func lowerKeys(m map[string]interface{}) (map[string]interface{}, map[string]bool) {
	out := make(map[string]interface{}, len(m))
	conflicted := make(map[string]bool)
	for k, v := range m {
		lk := strings.ToLower(strings.TrimSpace(k))
		if prev, exists := out[lk]; exists {
			if !reflect.DeepEqual(prev, v) {
				conflicted[lk] = true
			}
			continue
		}
		out[lk] = v
	}
	for lk := range conflicted {
		delete(out, lk)
	}
	return out, conflicted
}

// envelopeKeys are wrapper sub-objects whose scalar entries are merged up to the
// top level so nested decisions (e.g. position_sizing{}, risk_management{}) are
// reachable by flat field extraction.
var envelopeKeys = []string{
	"position_sizing", "risk_management", "order", "trade",
	"params", "parameters", "sizing", "risk", "decision_params",
}

// flattenEnvelopes lower-cases keys (like lowerKeys) and additionally merges the
// scalar entries of known wrapper sub-objects up to the top level. Top-level
// keys are authoritative; a key that appears in more than one place (top level
// or any envelope) with DIFFERENT values is reported as a conflict and dropped,
// so the caller fails safe rather than picking a silent winner. Deeper nesting
// inside an envelope is ignored.
func flattenEnvelopes(raw map[string]interface{}) (map[string]interface{}, map[string]bool) {
	out := make(map[string]interface{}, len(raw))
	conflicted := make(map[string]bool)
	put := func(k string, v interface{}) {
		lk := strings.ToLower(strings.TrimSpace(k))
		if prev, exists := out[lk]; exists {
			if !reflect.DeepEqual(prev, v) {
				conflicted[lk] = true
			}
			return
		}
		out[lk] = v
	}
	envSet := make(map[string]bool, len(envelopeKeys))
	for _, e := range envelopeKeys {
		envSet[e] = true
	}
	// First pass: place top-level scalars (authoritative on collision) and
	// collect envelope containers by their LOWER-CASED name, so case variants
	// (e.g. "Position_Sizing") are not silently skipped.
	var envContainers []map[string]interface{}
	for k, v := range raw {
		lk := strings.ToLower(strings.TrimSpace(k))
		if envSet[lk] {
			if subMap, isMap := v.(map[string]interface{}); isMap {
				envContainers = append(envContainers, subMap)
			}
			continue // the container itself is never a usable field
		}
		put(k, v)
	}
	// Second pass: merge scalar entries from ALL envelope containers (including
	// case variants). put() flags any per-key value disagreement as a conflict,
	// so two containers that contradict each other cannot silently pick a winner.
	for _, subMap := range envContainers {
		for k, v := range subMap {
			switch v.(type) {
			case map[string]interface{}, []interface{}:
				continue // ignore deeper nesting / arrays
			}
			put(k, v)
		}
	}
	for lk := range conflicted {
		delete(out, lk)
	}
	return out, conflicted
}

// pctEquityToUSD converts a percent-of-equity position size into a USD notional.
// The value is ALWAYS read as a percentage and accepted only in the sane range
// [1, 100] (e.g. 20 -> 20% of equity). Anything outside that range — including a
// sub-1 value whose unit is ambiguous (0.5 could mean 0.5% or 50%) or an absurd
// >100 — returns 0 so the caller fails safe to wait rather than guessing the
// unit. Downstream risk control still applies the final position-size caps.
func pctEquityToUSD(pct, equity float64) float64 {
	if equity <= 0 || pct < 1 || pct > 100 {
		return 0
	}
	return pct / 100 * equity
}

// criticalConflictKeys are the lower-cased field families that drive a trading
// decision. A case-variant conflict on ANY of them makes the decision
// ambiguous on a money path, so it must collapse to wait rather than fall back
// to an alias or be treated as "missing".
var criticalConflictKeys = []string{
	"action", "decision", "signal",
	"symbol", "ticker",
	"side", "direction", "position_side",
	"position_size_usd", "position_size_usdt", "notional_usd", "notional_usdt", "suggested_notional_usdt", "notional",
	"position_size_pct_equity", "position_size_pct", "pct_equity",
	"stop_loss", "sl", "stop_price", "stoploss",
	"take_profit", "take_profit_1", "tp", "tp1", "target_price", "takeprofit",
	"leverage",
}

func hasCriticalConflict(conflicts map[string]bool) bool {
	for _, k := range criticalConflictKeys {
		if conflicts[k] {
			return true
		}
	}
	return false
}

// aliasFamilies groups interchangeable field names. If two members of a family
// are both present with disagreeing values, the decision is ambiguous → wait.
var aliasFamilies = [][]string{
	{"action", "decision", "signal"},
	{"symbol", "ticker"},
	{"side", "direction", "position_side"},
	{"position_size_usd", "position_size_usdt", "notional_usd", "notional_usdt", "suggested_notional_usdt", "notional"},
	{"position_size_pct_equity", "position_size_pct", "pct_equity"},
	{"stop_loss", "sl", "stop_price", "stoploss"},
	{"take_profit", "take_profit_1", "tp", "tp1", "target_price", "takeprofit"},
}

// familyValueConflict reports whether 2+ of the given keys are present with
// values that do not all normalize equal. Comparison is conservative (a string
// vs number, or any differing value, counts as a conflict) — safe for a money
// path where a false "wait" is preferable to a guessed open.
func familyValueConflict(m map[string]interface{}, keys ...string) bool {
	first := ""
	have := false
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		key := valueCompareKey(v)
		if !have {
			first, have = key, true
			continue
		}
		if key != first {
			return true
		}
	}
	return false
}

// valueCompareKey renders a JSON value into a normalized, type-tagged string for
// equality comparison (case-insensitive for strings, canonical for numbers).
func valueCompareKey(v interface{}) string {
	switch n := v.(type) {
	case string:
		return "s:" + strings.ToLower(strings.TrimSpace(n))
	case float64:
		return "f:" + strconv.FormatFloat(n, 'g', -1, 64)
	case json.Number:
		return "f:" + n.String()
	case bool:
		return fmt.Sprintf("b:%v", n)
	case nil:
		return "null"
	default:
		return fmt.Sprintf("o:%v", n)
	}
}

func firstStr(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func firstFloat(m map[string]interface{}, keys ...string) (float64, bool) {
	// finite rejects NaN/±Inf so a money-path number can never slip through a
	// gate (e.g. "+Inf" parsed from a string would otherwise pass numeric checks).
	finite := func(f float64) (float64, bool) {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return finite(n)
		case json.Number:
			if f, err := n.Float64(); err == nil {
				return finite(f)
			}
		case int:
			return float64(n), true
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				return finite(f)
			}
		}
	}
	return 0, false
}

func toMaps(list []interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(list))
	for _, it := range list {
		if mm, ok := it.(map[string]interface{}); ok {
			out = append(out, mm)
		}
	}
	return out
}
