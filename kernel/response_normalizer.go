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
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	normalizerOff    = "off"
	normalizerOn     = "on"
	normalizerShadow = "shadow"
)

// reSecretKV redacts obvious credential-looking key/value pairs before logging.
var reSecretKV = regexp.MustCompile(`(?i)(authorization|api[-_]?key|secret|token|bearer|password)\s*[:=]\s*[^\s,"']+`)

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
//
// Returns the normalized standard-format string, whether it differs from the
// raw input (changed), and a short reason code for logging.
func NormalizeAIResponse(raw string, candidatePool []string, currentPrice map[string]float64) (normalized string, changed bool, reason string) {
	pool := make(map[string]bool, len(candidatePool))
	for _, s := range candidatePool {
		if t := strings.TrimSpace(s); t != "" {
			pool[t] = true
		}
	}

	reasoning := extractReasoningText(raw)

	// Decision JSON extraction priority:
	//   a. inside <decision>...</decision>
	//   b/c. first balanced, valid JSON value anywhere (covers ```json fences
	//        and bare objects/arrays — the scanner ignores the surrounding
	//        backticks and prose).
	region := raw
	if m := reDecisionTag.FindStringSubmatch(raw); m != nil && len(m) > 1 {
		region = m[1]
	}
	frag, ok := findFirstJSON(region)
	if !ok && region != raw {
		frag, ok = findFirstJSON(raw)
	}
	if !ok {
		out := buildNormalized(reasoning, []Decision{waitDecision("ALL", reasoning, "no_json_found")})
		return out, out != raw, "no_json_found"
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
		d := normalizeOne(rd, pool, currentPrice)
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
		lo := lowerKeys(obj)
		if ordersRaw, present := lo["orders"]; present {
			if list, isList := ordersRaw.([]interface{}); isList {
				return toMaps(list), ""
			}
		}
		return []map[string]interface{}{obj}, ""
	}

	return nil, "json_parse_error"
}

// normalizeOne maps one free-form decision object to a standard Decision,
// downgrading to "wait" whenever any open-position safety gate is not met.
func normalizeOne(rawObj map[string]interface{}, pool map[string]bool, price map[string]float64) Decision {
	m := lowerKeys(rawObj)

	symbol := firstStr(m, "symbol", "ticker")
	reasoning := firstStr(m, "reasoning", "reason", "rationale")
	dir := resolveDir(m)
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

	// Open-position gates — ALL must hold, else downgrade to "wait".
	// ② USD size only from the whitelist (position_size/size/qty/amount/quantity
	//    are explicitly NOT treated as USD).
	posUSD, hasUSD := firstFloat(m, "position_size_usd", "notional_usd", "notional_usdt")
	sl, hasSL := firstFloat(m, "stop_loss")
	tp, hasTP := firstFloat(m, "take_profit")
	leverage := 0
	if l, ok := firstFloat(m, "leverage"); ok {
		leverage = int(l + 0.5)
	}

	// ⑤ symbol must exactly match the candidate pool (no fuzzy completion).
	if symbol == "" || !pool[symbol] {
		return waitDecision(symbol, reasoning, "symbol_not_in_pool")
	}
	// ② USD field present and positive.
	if !hasUSD || posUSD <= 0 {
		return waitDecision(symbol, reasoning, "missing_usd_size")
	}
	// ③ both stop-loss and take-profit present and positive.
	if !hasSL || !hasTP || sl <= 0 || tp <= 0 {
		return waitDecision(symbol, reasoning, "missing_sl_tp")
	}
	// ④ price/direction consistency against the current price.
	cur, ok := price[symbol]
	if !ok || cur <= 0 {
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
	a = strings.ReplaceAll(a, "-", "_")
	a = strings.ReplaceAll(a, " ", "_")
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

// resolveDir extracts an explicit long/short direction from side-like fields.
func resolveDir(m map[string]interface{}) string {
	s := strings.ToLower(firstStr(m, "side", "direction", "position_side"))
	switch {
	case strings.Contains(s, "long"):
		return "long"
	case strings.Contains(s, "short"):
		return "short"
	default:
		return ""
	}
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

func lowerKeys(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out
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
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return n, true
		case json.Number:
			if f, err := n.Float64(); err == nil {
				return f, true
			}
		case int:
			return float64(n), true
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				return f, true
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
