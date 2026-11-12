package eval

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// answers maps item id → what a model would answer; the item set is matched by prompt.
func fakeAsk(answers map[string]string) Ask {
	byPrompt := map[string]string{}
	for _, it := range Items() {
		byPrompt[it.Prompt] = answers[it.ID]
	}
	return func(_ context.Context, target, prompt string) (string, error) {
		a, ok := answers["__err__"+target]
		if ok {
			return "", errors.New(a)
		}
		return byPrompt[prompt], nil
	}
}

func TestSuiteIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	cats := map[string]int{}
	for _, it := range Items() {
		if it.ID == "" || it.Prompt == "" || len(it.Checks) == 0 {
			t.Errorf("incomplete item %+v", it.ID)
		}
		if seen[it.ID] {
			t.Errorf("duplicate id %s", it.ID)
		}
		seen[it.ID] = true
		cats[it.Category]++
	}
	for _, c := range []string{CatFormat, CatReasoning, CatLanguage, CatKnowledge} {
		if cats[c] < 4 {
			t.Errorf("category %s has only %d items", c, cats[c])
		}
	}
}

// A model giving the ideal answers must score 100%, so a failing check is the model's fault, not the suite's.
var perfect = map[string]string{
	"bullets3": "- purr\n- nap\n- hunt", "lowercase": "the capital of france is paris.",
	"json": "```json\n{\"name\": \"Ada\", \"age\": 36}\n```", "banana2": "I like banana smoothies with banana bread!",
	"under15": "Air scatters blue light more than red.", "twoparas": "The sea is vast.\n***\nIt is also deep.",
	"csv5": "apple, pear, plum, fig, kiwi", "multi": "autumn leaves fall slowly and the air turns cool that is all",
	"speed": "150", "pencils": "28 TL", "mult": "391", "sally": "1", "seq": "32", "larger": "9.9",
	"tr_capital": "Türkiye'nin başkenti Ankara'dır ve bu şehir için önemli bir merkezdir.",
	"tr_intro":   "Dil modeli, metin üretmek için eğitilmiş bir yapay zekâ sistemidir ve bu sayede sorulara cevap verir.",
	"say_tr":     "Günaydın", "en_only": "An API lets programs talk to each other. It defines requests and responses.",
	"year": "1998", "translate": "Hello world", "pyprint": "10", "gold": "Au", "reverse": "321cba", "unknown": "unknown",
}

func TestPerfectAnswersScoreOneHundredPercent(t *testing.T) {
	r := Run(context.Background(), fakeAsk(perfect), "m", Items(), nil)
	for _, it := range r.Items {
		if !it.Pass {
			t.Errorf("%s failed with %q (%d/%d checks)", it.ID, it.Answer, it.Passed, it.Total)
		}
	}
	if r.Score() != 1 || r.CheckScore() != 1 {
		t.Fatalf("score %.2f checks %.2f", r.Score(), r.CheckScore())
	}
}

// "Ankara." is a perfectly good answer to the Turkish capital question; the suite must not reject short answers.
func TestShortCorrectAnswersAreNotRejected(t *testing.T) {
	r := Run(context.Background(), fakeAsk(map[string]string{"tr_capital": "Ankara."}), "m", Items(), nil)
	for _, it := range r.Items {
		if it.ID == "tr_capital" && !it.Pass {
			t.Fatalf("tr_capital failed on %q", it.Answer)
		}
	}
}

func TestWrongAnswersAreCaught(t *testing.T) {
	bad := map[string]string{
		"bullets3":  "Here are three:\n- a\n- b\n- c", // extra text
		"lowercase": "The capital is Paris.", "json": "name: Ada", "banana2": "banana banana banana!",
		"under15": "The sky is blue because of a phenomenon called Rayleigh scattering of sunlight in the atmosphere of Earth.",
		"speed":   "about 140", "sally": "2", "larger": "9.11",
		"tr_capital": "首都是安卡拉 Ankara", // right city, drifted into Chinese
		"say_tr":     "Good morning", "gold": "Gold", "unknown": "The capital is Zorbville.",
	}
	r := Run(context.Background(), fakeAsk(bad), "m", Items(), nil)
	failed := map[string]bool{}
	for _, it := range r.Items {
		if !it.Pass {
			failed[it.ID] = true
		}
	}
	for id := range bad {
		if !failed[id] {
			t.Errorf("%s should have failed", id)
		}
	}
	if r.Score() > 0.3 {
		t.Errorf("score %.2f is too high for these answers", r.Score())
	}
}

func TestErrorsCountAsFailedAndScoresAreSorted(t *testing.T) {
	ask := func(ctx context.Context, target, prompt string) (string, error) {
		if target == "dead" {
			return "", errors.New("no answer: the model used all its tokens on reasoning")
		}
		return fakeAsk(perfect)(ctx, target, prompt)
	}
	rs := RunAll(context.Background(), ask, []string{"dead", "good"}, Items(), 2, nil)
	if rs[0].Target != "good" || rs[1].Target != "dead" || rs[1].Score() != 0 {
		t.Fatalf("order/scores: %v %.2f / %v %.2f", rs[0].Target, rs[0].Score(), rs[1].Target, rs[1].Score())
	}
	if !strings.Contains(rs[1].Items[0].Err, "reasoning") {
		t.Fatal("the error must be kept")
	}
}

func TestSaveMergesAndLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eval.json")
	a := Run(context.Background(), fakeAsk(perfect), "a", Items(), nil)
	b := Run(context.Background(), fakeAsk(map[string]string{}), "b", Items(), nil)
	if err := Save(path, []Result{a}); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, []Result{b}); err != nil {
		t.Fatal(err)
	}
	m := Load(path)
	if len(m) != 2 || m["a"].Score != 1 || m["b"].Score >= 0.2 || m["a"].ByCat[CatReasoning] != 1 {
		t.Fatalf("loaded %+v", m)
	}
}
