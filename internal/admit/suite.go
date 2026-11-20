package admit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// SuiteVersion identifies the deterministic synthetic admission suite.
const SuiteVersion = "v1-synth-3cases"

const minCrossSentenceRepeatRunes = 6

var chineseNegationMarkers = []string{
	"不要", "请勿", "别", "切勿", "不能", "不得", "勿",
}

// CaseSpec defines one deterministic English-to-Chinese synthetic subtitle test case.
type CaseSpec struct {
	ID              string
	SystemPrompt    string
	UserPrompt      string
	ExpectedIDs     []string
	RequiredNumbers []string
	RequiredTerms   []string
	SourceTexts     map[string]string
	Validator       func(c CaseSpec, parsed map[string]string, finishReason string) (bool, string)
}

// DefaultCases returns the 3 deterministic synthetic admission cases specified in SPEC §6.2.
func DefaultCases() []CaseSpec {
	return []CaseSpec{
		{
			ID: "case1_negation_term_number",
			SystemPrompt: "You are a precise English-to-Chinese subtitle translator. " +
				"Translate each numbered English line into natural Simplified Chinese. " +
				"Strictly preserve all numbers, follow the provided glossary, keep negations accurate, and output ONLY a valid JSON object mapping each string ID to its Chinese translation without any explanation or extra text.",
			UserPrompt: "Translate the following English subtitle lines into Simplified Chinese.\n" +
				"Glossary:\n- cache -> 缓存\n\n" +
				"Return ONLY a JSON object with keys \"1\" and \"2\":\n" +
				"{\n  \"1\": \"Do not disable the cache.\",\n  \"2\": \"It stores 12 results.\"\n}",
			ExpectedIDs:     []string{"1", "2"},
			RequiredNumbers: []string{"12"},
			RequiredTerms:   []string{"缓存"},
			SourceTexts: map[string]string{
				"1": "Do not disable the cache.",
				"2": "It stores 12 results.",
			},
			Validator: validateCase1Negation,
		},
		{
			ID: "case2_split_condition_number",
			SystemPrompt: "You are a precise English-to-Chinese subtitle translator. " +
				"Translate each split English subtitle fragment into Simplified Chinese while keeping each fragment aligned 1-to-1 with its ID. " +
				"Do not repeat the entire sentence in both fragments. Preserve all numbers and output ONLY a valid JSON object mapping each string ID to its translation.",
			UserPrompt: "Translate these split English subtitle fragments into Simplified Chinese, keeping each ID separate.\n" +
				"Return ONLY a JSON object with keys \"1\" and \"2\":\n" +
				"{\n  \"1\": \"If the temperature exceeds 35 degrees,\",\n  \"2\": \"stop the pump.\"\n}",
			ExpectedIDs:     []string{"1", "2"},
			RequiredNumbers: []string{"35"},
			SourceTexts: map[string]string{
				"1": "If the temperature exceeds 35 degrees,",
				"2": "stop the pump.",
			},
			Validator: validateCase2SplitNoDup,
		},
		{
			ID: "case3_batch8_numbers_cache",
			SystemPrompt: "You are a precise English-to-Chinese subtitle translator. " +
				"Translate every English subtitle fragment into Simplified Chinese, keeping each translation aligned strictly to its ID without repeating lines. " +
				"Preserve exact numbers (do not merge or drop digits), follow the glossary, and output ONLY a valid JSON object mapping keys \"1\" through \"9\" to their Chinese translations.",
			UserPrompt: "Translate the following 9 English subtitle lines into Simplified Chinese.\n" +
				"Glossary:\n- cache -> 缓存\n\n" +
				"Return ONLY a JSON object with keys \"1\" through \"9\":\n" +
				"{\n" +
				"  \"1\": \"Initialize 3 worker threads first.\",\n" +
				"  \"2\": \"Wait 30 seconds before retrying.\",\n" +
				"  \"3\": \"Do not flush the cache yet.\",\n" +
				"  \"4\": \"It retains 12 recent entries.\",\n" +
				"  \"5\": \"Log every timeout to disk.\",\n" +
				"  \"6\": \"Check the queue depth again.\",\n" +
				"  \"7\": \"Throttle requests under heavy load.\",\n" +
				"  \"8\": \"Verify the checksum after transfer.\",\n" +
				"  \"9\": \"Report completion to the monitor.\"\n" +
				"}",
			ExpectedIDs:     []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"},
			RequiredNumbers: []string{"3", "30", "12"},
			RequiredTerms:   []string{"缓存"},
			SourceTexts: map[string]string{
				"1": "Initialize 3 worker threads first.",
				"2": "Wait 30 seconds before retrying.",
				"3": "Do not flush the cache yet.",
				"4": "It retains 12 recent entries.",
				"5": "Log every timeout to disk.",
				"6": "Check the queue depth again.",
				"7": "Throttle requests under heavy load.",
				"8": "Verify the checksum after transfer.",
				"9": "Report completion to the monitor.",
			},
			Validator: validateCase3Batch,
		},
	}
}

// ParseStrictJSONObject strips an optional markdown code fence and parses a top-level JSON
// object of string values while strictly rejecting duplicate keys and non-string values.
func ParseStrictJSONObject(raw string) (map[string]string, error) {
	cleaned := stripCodeFence(strings.TrimSpace(raw))
	if cleaned == "" {
		return nil, errors.New("empty json response")
	}

	dec := json.NewDecoder(strings.NewReader(cleaned))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("expected top-level json object")
	}

	result := make(map[string]string)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid json key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", keyTok)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate json key %q", key)
		}

		valTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid json value for key %q: %w", key, err)
		}
		val, ok := valTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected string value for key %q, got %T", key, valTok)
		}
		result[key] = val
	}

	endTok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("expected closing brace: %w", err)
	}
	if endDelim, ok := endTok.(json.Delim); !ok || endDelim != '}' {
		return nil, fmt.Errorf("expected closing brace, got %v", endTok)
	}
	if extra, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("trailing json content: %w", err)
		}
		return nil, fmt.Errorf("unexpected trailing token %v", extra)
	}
	return result, nil
}

// ExtractNumberTokens extracts exact ASCII digit runs ([0-9]+) so "30" yields "30" and not "3".
func ExtractNumberTokens(text string) map[string]bool {
	tokens := make(map[string]bool)
	start := -1
	for i := 0; i < len(text); i++ {
		if text[i] >= '0' && text[i] <= '9' {
			if start == -1 {
				start = i
			}
		} else if start != -1 {
			tokens[text[start:i]] = true
			start = -1
		}
	}
	if start != -1 {
		tokens[text[start:]] = true
	}
	return tokens
}

// ValidateCaseResponse evaluates a model response against deterministic rules for CaseSpec c.
func ValidateCaseResponse(c CaseSpec, content, finishReason string, reasoningTokens int, reasoningContent string) (bool, string) {
	if reasoningTokens != 0 {
		return false, fmt.Sprintf("reasoning_tokens must be 0, got %d", reasoningTokens)
	}
	if strings.TrimSpace(reasoningContent) != "" {
		return false, "non-empty reasoning_content is forbidden"
	}

	parsed, err := ParseStrictJSONObject(content)
	if err != nil {
		return false, fmt.Sprintf("json parse failed: %v", err)
	}

	expectedValues := make([]string, 0, len(c.ExpectedIDs))
	for _, id := range c.ExpectedIDs {
		val, ok := parsed[id]
		if !ok {
			return false, fmt.Sprintf("missing required id %q", id)
		}
		if strings.TrimSpace(val) == "" {
			return false, fmt.Sprintf("empty translation for id %q", id)
		}
		if !containsCJK(val) {
			return false, fmt.Sprintf("translation for id %q contains no CJK characters", id)
		}
		if src, hasSrc := c.SourceTexts[id]; hasSrc && isVerbatimCopy(src, val) {
			return false, fmt.Sprintf("translation for id %q is a verbatim copy of source", id)
		}
		expectedValues = append(expectedValues, val)
	}

	combined := strings.Join(expectedValues, " ")
	numTokens := ExtractNumberTokens(combined)
	for _, reqNum := range c.RequiredNumbers {
		if !numTokens[reqNum] {
			return false, fmt.Sprintf("missing exact number token %q", reqNum)
		}
	}
	for _, term := range c.RequiredTerms {
		if !strings.Contains(combined, term) {
			return false, fmt.Sprintf("missing required term %q", term)
		}
	}

	if c.Validator != nil {
		if ok, reason := c.Validator(c, parsed, finishReason); !ok {
			return false, reason
		}
	}
	return true, ""
}

func validateCase1Negation(_ CaseSpec, parsed map[string]string, _ string) (bool, string) {
	line1 := parsed["1"]
	for _, marker := range chineseNegationMarkers {
		if strings.Contains(line1, marker) {
			return true, ""
		}
	}
	return false, "case 1 id \"1\" missing Chinese negation marker"
}

func validateCase2SplitNoDup(_ CaseSpec, parsed map[string]string, _ string) (bool, string) {
	n1 := normalizeForCompare(parsed["1"])
	n2 := normalizeForCompare(parsed["2"])
	if n1 == "" || n2 == "" {
		return false, "case 2 fragment normalized to empty"
	}
	if n1 == n2 {
		return false, "case 2 split fragments are full-sentence duplicates"
	}
	return true, ""
}

func validateCase3Batch(c CaseSpec, parsed map[string]string, finishReason string) (bool, string) {
	if finishReason != "stop" {
		return false, fmt.Sprintf("finish_reason must be \"stop\", got %q", finishReason)
	}
	return hasNoCrossSentenceRepetition(c.ExpectedIDs, parsed)
}

func hasNoCrossSentenceRepetition(ids []string, parsed map[string]string) (bool, string) {
	norms := make([]string, len(ids))
	runesList := make([][]rune, len(ids))
	for i, id := range ids {
		norms[i] = normalizeForCompare(parsed[id])
		runesList[i] = []rune(norms[i])
	}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if norms[i] != "" && norms[i] == norms[j] {
				return false, fmt.Sprintf("duplicate translation between %q and %q", ids[i], ids[j])
			}
			r1, r2 := runesList[i], runesList[j]
			if len(r1) < minCrossSentenceRepeatRunes || len(r2) < minCrossSentenceRepeatRunes {
				continue
			}
			for k := 0; k <= len(r1)-minCrossSentenceRepeatRunes; k++ {
				sub := string(r1[k : k+minCrossSentenceRepeatRunes])
				if strings.Contains(norms[j], sub) {
					return false, fmt.Sprintf("long cross-sentence repetition (%q) between %q and %q", sub, ids[i], ids[j])
				}
			}
		}
	}
	return true, ""
}

func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s
	}
	inner := s[3 : len(s)-3]
	if idx := strings.IndexByte(inner, '\n'); idx >= 0 {
		firstLine := strings.TrimSpace(inner[:idx])
		if firstLine == "" || strings.EqualFold(firstLine, "json") {
			inner = inner[idx+1:]
		}
		return strings.TrimSpace(inner)
	}
	trimmed := strings.TrimSpace(inner)
	if len(trimmed) >= 4 && strings.EqualFold(trimmed[:4], "json") {
		if len(trimmed) == 4 || trimmed[4] == ' ' || trimmed[4] == '\t' || trimmed[4] == '{' {
			return strings.TrimSpace(trimmed[4:])
		}
	}
	return trimmed
}

func containsCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func isVerbatimCopy(source, translated string) bool {
	normSrc := normalizeForCompare(source)
	normTrans := normalizeForCompare(translated)
	if normSrc == "" {
		return false
	}
	if normTrans == normSrc {
		return true
	}
	return len(normSrc) >= 8 && strings.Contains(normTrans, normSrc)
}

func normalizeForCompare(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}
