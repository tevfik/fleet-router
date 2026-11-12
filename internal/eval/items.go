package eval

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// A Check is one machine-verifiable requirement on an answer, in the spirit of IFEval: no judge model is
// involved (small local judges are not reliable enough), every check is a deterministic function.
type Check struct {
	Name string
	OK   func(answer string) bool
}

// Item is one prompt with the checks that all have to hold for it to count as passed
// (IFEval's "prompt-level strict accuracy"; the share of single checks passed is reported as well).
type Item struct {
	ID       string
	Category string // format · reasoning · language · knowledge
	Prompt   string
	Checks   []Check
}

const (
	CatFormat    = "format"
	CatReasoning = "reasoning"
	CatLanguage  = "language"
	CatKnowledge = "knowledge"
)

// ---- helpers the checks are built from ----

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

func words(s string) int { return len(strings.Fields(s)) }

var numRe = regexp.MustCompile(`-?\d+(?:[.,]\d+)?`)

// lastNumber is the last number in the answer ("so it is 150 km" → 150), tolerant of "150 km" and "= 150".
func lastNumber(s string) (float64, bool) {
	all := numRe.FindAllString(s, -1)
	if len(all) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.Replace(all[len(all)-1], ",", ".", 1), 64)
	return v, err == nil
}

func isNumber(want float64) func(string) bool {
	return func(a string) bool { v, ok := lastNumber(a); return ok && v == want }
}

func contains(sub string) func(string) bool {
	return func(a string) bool { return strings.Contains(strings.ToLower(a), strings.ToLower(sub)) }
}

func exactly(want string) func(string) bool {
	return func(a string) bool {
		a = strings.Trim(strings.TrimSpace(a), "`*\"'.")
		return strings.EqualFold(a, want)
	}
}

// stripFence removes a ```json … ``` wrapper (the loose variant of IFEval: models like to add one).
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// hasScript reports whether s contains letters of a script that does not belong in a Turkish/English answer
// (CJK, Cyrillic, Arabic, Hangul…): the failure seen when a multilingual model drifts into Chinese.
func hasForeignScript(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Cyrillic, unicode.Arabic, unicode.Hebrew, unicode.Thai) {
			return true
		}
	}
	return false
}

var turkishMarkers = []string{" bir ", " ve ", " bu ", " için ", " ile ", "dır", "dir", "tür", "ı", "ş", "ğ"}

func looksTurkish(s string) bool {
	s = " " + strings.ToLower(s) + " "
	n := 0
	for _, m := range turkishMarkers {
		if strings.Contains(s, m) {
			n++
		}
	}
	return n >= 2
}

func noForeign(a string) bool { return !hasForeignScript(a) }

// Items is the fixed suite. Changing it changes the scores' meaning, so bump SuiteVersion when you do.
const SuiteVersion = 2

func Items() []Item {
	return []Item{
		// ---- format: verifiable instructions ----
		{ID: "bullets3", Category: CatFormat, Prompt: "Write exactly 3 bullet points about cats. Each bullet must start with '- '. Write nothing else.",
			Checks: []Check{{"3 bullets, nothing else", func(a string) bool {
				l := lines(a)
				if len(l) != 3 {
					return false
				}
				for _, x := range l {
					if !strings.HasPrefix(x, "- ") {
						return false
					}
				}
				return true
			}}}},
		{ID: "lowercase", Category: CatFormat, Prompt: "In ALL LOWERCASE letters, with no capital letter anywhere, answer in one sentence: what is the capital of France?",
			Checks: []Check{{"no uppercase", func(a string) bool { return a == strings.ToLower(a) && strings.TrimSpace(a) != "" }}, {"says paris", contains("paris")}}},
		{ID: "json", Category: CatFormat, Prompt: "Reply with valid JSON only, nothing else: an object with the keys \"name\" (a string) and \"age\" (a number) for an invented person.",
			Checks: []Check{{"valid JSON with name/age", func(a string) bool {
				var v map[string]any
				if json.Unmarshal([]byte(stripFence(a)), &v) != nil {
					return false
				}
				_, n := v["name"].(string)
				_, g := v["age"].(float64)
				return n && g
			}}}},
		{ID: "banana2", Category: CatFormat, Prompt: "Write one sentence that contains the word \"banana\" exactly twice and ends with an exclamation mark.",
			Checks: []Check{{"banana twice", func(a string) bool { return strings.Count(strings.ToLower(a), "banana") == 2 }},
				{"ends with !", func(a string) bool { return strings.HasSuffix(strings.TrimSpace(a), "!") }}}},
		{ID: "under15", Category: CatFormat, Prompt: "In fewer than 15 words, explain why the sky is blue.",
			Checks: []Check{{"under 15 words", func(a string) bool { return words(a) > 0 && words(a) < 15 }}}},
		{ID: "twoparas", Category: CatFormat, Prompt: "Write two short paragraphs about the sea. Separate them with a line that contains only ***.",
			Checks: []Check{{"two paragraphs split by ***", func(a string) bool {
				parts := strings.Split(a, "***")
				return len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && strings.TrimSpace(parts[1]) != ""
			}}}},
		{ID: "csv5", Category: CatFormat, Prompt: "List exactly 5 fruits as comma-separated values on a single line. Output nothing else.",
			Checks: []Check{{"5 comma-separated items on one line", func(a string) bool {
				l := lines(a)
				return len(l) == 1 && len(strings.Split(l[0], ",")) == 5
			}}}},
		{ID: "multi", Category: CatFormat, Prompt: "Write a short note about autumn. It must be all lowercase, contain no commas, and end with the exact words: that is all",
			Checks: []Check{{"lowercase", func(a string) bool { return a == strings.ToLower(a) }},
				{"no commas", func(a string) bool { return !strings.Contains(a, ",") }},
				{"ends with 'that is all'", func(a string) bool {
					return strings.HasSuffix(strings.ToLower(strings.Trim(strings.TrimSpace(a), ".!")), "that is all")
				}}}},

		// ---- reasoning: one numeric answer ----
		{ID: "speed", Category: CatReasoning, Prompt: "A train travels at 60 km/h for 2.5 hours. How many kilometres does it travel? Answer with the number only.", Checks: []Check{{"150", isNumber(150)}}},
		{ID: "pencils", Category: CatReasoning, Prompt: "3 pencils cost 12 TL. How much do 7 pencils cost? Answer with the number only.", Checks: []Check{{"28", isNumber(28)}}},
		{ID: "mult", Category: CatReasoning, Prompt: "What is 17 times 23? Answer with the number only.", Checks: []Check{{"391", isNumber(391)}}},
		{ID: "sally", Category: CatReasoning, Prompt: "Sally has 3 brothers. Each of her brothers has 2 sisters. How many sisters does Sally have? Answer with the number only.", Checks: []Check{{"1", isNumber(1)}}},
		{ID: "seq", Category: CatReasoning, Prompt: "What number comes next: 2, 4, 8, 16, ? Answer with the number only.", Checks: []Check{{"32", isNumber(32)}}},
		{ID: "larger", Category: CatReasoning, Prompt: "Which number is larger, 9.11 or 9.9? Answer with that number only.", Checks: []Check{{"9.9", isNumber(9.9)}}},

		// ---- language: stays in the language asked, no drifting into another script ----
		{ID: "tr_capital", Category: CatLanguage, Prompt: "Türkiye'nin başkenti neresidir? Tek cümleyle cevap ver.",
			Checks: []Check{{"mentions Ankara", contains("ankara")}, {"no foreign script", noForeign}}}, // a one-word answer ("Ankara.") shows no language: only the script is checked
		{ID: "tr_intro", Category: CatLanguage, Prompt: "Bana iki cümleyle bir dil modelinin ne olduğunu açıkla.",
			Checks: []Check{{"no foreign script", noForeign}, {"answers in Turkish", func(a string) bool { return looksTurkish(a) }}}},
		{ID: "say_tr", Category: CatLanguage, Prompt: "Say \"good morning\" in Turkish. Reply with the Turkish phrase only.", Checks: []Check{{"günaydın", contains("günaydın")}}},
		{ID: "en_only", Category: CatLanguage, Prompt: "Explain in two sentences what an API is. Answer in English only.",
			Checks: []Check{{"no foreign script", noForeign}, {"two sentences or fewer", func(a string) bool { return strings.Count(a, ". ")+1 <= 2 }}}},

		// ---- knowledge / extraction ----
		{ID: "year", Category: CatKnowledge, Prompt: "Extract the year from this text and reply with the year only: \"The company was founded in 1998 in Ankara.\"", Checks: []Check{{"1998", isNumber(1998)}}},
		{ID: "translate", Category: CatKnowledge, Prompt: "Translate to English and reply with the translation only: \"Merhaba dünya\"", Checks: []Check{{"hello world", exactly("hello world")}}},
		{ID: "pyprint", Category: CatKnowledge, Prompt: "What does this Python code print? Reply with the output only.\nprint(sum(range(5)))", Checks: []Check{{"10", isNumber(10)}}},
		{ID: "gold", Category: CatKnowledge, Prompt: "What is the chemical symbol of gold? Reply with the symbol only.", Checks: []Check{{"Au", exactly("au")}}},
		{ID: "reverse", Category: CatKnowledge, Prompt: "Reverse the string \"abc123\". Reply with the reversed string only.", Checks: []Check{{"321cba", exactly("321cba")}}},
		{ID: "unknown", Category: CatKnowledge, Prompt: "What is the capital of the fictional country Zorbania? If it does not exist or you do not know, reply with the single word: unknown",
			Checks: []Check{{"admits it does not know", func(a string) bool {
				l := strings.ToLower(a)
				return strings.Contains(l, "unknown") || strings.Contains(l, "bilmiyorum")
			}}}},
	}
}
