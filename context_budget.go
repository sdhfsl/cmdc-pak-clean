package main

import "strings"

// Context-budget guards. The upstream enforces `input + completion <= context
// window` as a hard limit, so a conversation whose history has grown close to
// the window fails outright once the fixed completion budget no longer fits —
// and every retry of the same conversation keeps failing. These helpers
// estimate the input size and shrink the completion budget to the room that is
// actually left, so long conversations degrade gracefully instead of dying.
const (
	// contextSafetyMargin absorbs model-side framing tokens (chat template,
	// per-message role markers) that a byte estimate cannot see.
	contextSafetyMargin = 6144
	// minCompletionBudget is the floor still sent when the history fills the
	// window: a tiny budget can finish a short turn, while sending nothing is
	// a guaranteed failure.
	minCompletionBudget = 1024
)

// Per-unit token weights in hundredths of a token, tuned against live
// upstream measurements (2026-10: CJK 3.33 bytes/token, English prose 6.7,
// code 3.3, base64 1.44, punctuation 1.41, digits 3.0).
const (
	cjkCentiPerRune   = 95 // CJK runes: 0.95 token each (measured 0.90)
	wordCentiPerChar  = 20 // short alnum runs: word-like, 0.20 token/char
	denseCentiPerChar = 62 // long alnum runs (ids, base64, hashes)
	spaceCentiPerChar = 8  // spaces and newlines merge cheaply
	otherCentiPerChar = 72 // punctuation, symbols: measured 0.71
	otherWideCenti    = 50 // non-CJK non-ASCII runes (Cyrillic, emoji)
	wordRunMax        = 12 // runs longer than this switch to the dense rate
)

// contextFit is the outcome of fitting the completion budget to a request.
type contextFit struct {
	Limit     int  // model context window (0 when unknown)
	Estimated int  // estimated input tokens incl. safety margin
	Fitted    int  // completion budget to send
	Shrunk    bool // Fitted < requested maxTokens
	Over      bool // history alone (nearly) exhausts the window
}

// contextWindowOf returns the model's context window from the live catalog.
// Zero means unknown (no fitting is attempted).
func contextWindowOf(model string) int {
	catalog, _ := snapshotCatalog()
	for _, m := range catalog {
		if m.ID == model && m.ContextWindow > 0 {
			return m.ContextWindow
		}
	}
	return 0
}

// fitContextBudget caps the completion budget so the estimated request stays
// inside the model's context window. When the model is unknown the request is
// left untouched.
func fitContextBudget(model string, maxTokens int, vals ...any) contextFit {
	limit := contextWindowOf(model)
	if limit <= 0 || maxTokens <= 0 {
		return contextFit{Limit: limit, Fitted: maxTokens}
	}
	est := estimateInputTokens(vals...) + contextSafetyMargin
	room := limit - est
	if room >= maxTokens {
		return contextFit{Limit: limit, Estimated: est, Fitted: maxTokens}
	}
	fitted := room
	if fitted < minCompletionBudget {
		fitted = minCompletionBudget
	}
	return contextFit{Limit: limit, Estimated: est, Fitted: fitted,
		Shrunk: true, Over: est+minCompletionBudget > limit}
}

// estimateInputTokens approximates the upstream token count of a request
// payload by walking its strings and weights by character class, also
// accounting for the JSON syntax added at marshal time (quotes, braces,
// separators). Base64 image data URLs are skipped: the upstream prices them
// by pixels, not bytes.
func estimateInputTokens(vals ...any) int {
	acc := &tokenAcc{}
	for _, v := range vals {
		acc.add(v)
	}
	return acc.centi / 100
}

type tokenAcc struct {
	centi int // hundredths of a token
}

func (a *tokenAcc) add(v any) {
	switch t := v.(type) {
	case string:
		a.addString(t)
		a.centi += 3 * otherCentiPerChar // enclosing quotes + separator
	case []any:
		a.centi += 2 * otherCentiPerChar // brackets
		for _, e := range t {
			a.centi += otherCentiPerChar // separator
			a.add(e)
		}
	case map[string]any:
		a.centi += 2 * otherCentiPerChar // braces
		for k, e := range t {
			a.centi += 4 * otherCentiPerChar // key quotes, colon, separator
			a.addString(k)
			a.add(e)
		}
	default:
		a.centi += 4 * otherCentiPerChar // number/bool/null literal
	}
}

func (a *tokenAcc) addString(s string) {
	if len(s) > 512 && strings.HasPrefix(s, "data:") {
		return
	}
	run := 0
	flush := func() {
		if run == 0 {
			return
		}
		per := wordCentiPerChar
		if run > wordRunMax {
			per = denseCentiPerChar
		}
		a.centi += run * per
		run = 0
	}
	for _, r := range s {
		switch {
		case isCJKRune(r):
			flush()
			a.centi += cjkCentiPerRune
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'):
			run++
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
			a.centi += spaceCentiPerChar
		case r >= 0x80:
			flush()
			a.centi += otherWideCenti
		default:
			flush()
			a.centi += otherCentiPerChar
		}
	}
	flush()
}

// isCJKRune reports whether the rune belongs to the CJK blocks whose density
// was measured at roughly one token per character.
func isCJKRune(r rune) bool {
	return r >= 0x2E80 && r <= 0x9FFF || // radicals, kana, CJK unified
		r >= 0xF900 && r <= 0xFAFF || // compatibility ideographs
		r >= 0x20000 && r <= 0x3FFFF // extensions B+
}
