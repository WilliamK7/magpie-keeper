package catalog

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"magpie-keeper/internal/domain"
)

func boolPtr(b bool) *bool        { return &b }
func floatPtr(f float64) *float64 { return &f }

func TestDefaultRegistryAndLookup(t *testing.T) {
	reg := NewDefaultRegistry(nil)
	want := []string{AdapterOpenRouter, AdapterRegistry, AdapterVercel, AdapterVLLMSR}
	if got := reg.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for _, name := range want {
		a, ok := reg.Get(name)
		if !ok || a.Name() != name {
			t.Fatalf("Get(%q) = (%v, %v)", name, a, ok)
		}
	}
	if _, err := reg.Discover(context.Background(), domain.Provider{ID: "p1"}, domain.ProviderRule{Adapter: "missing"}, nil); err == nil {
		t.Fatal("expected error for unknown adapter")
	}
}

func TestMatchGlobsAndFilterRule(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"*:free", "deepseek/deepseek-r1:free", true},
		{"*:free", "deepseek/deepseek-r1", false},
		{"openai/*", "openai/gpt-6-luna", true},
		{"openai/*", "google/gemini-3.8-flash", false},
		{"qwen?.?-*", "qwen3.8-max", true},
		{"qwen?.?-*", "qwen30.8-max", false},
		{"exact-model", "exact-model", true},
		{"exact-model", "exact-model-2", false},
	}
	for _, tc := range cases {
		if got := MatchGlob(tc.pattern, tc.value); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}

	rule := domain.ProviderRule{
		Include: []string{"openai/*", "*:free"},
		Exclude: []string{"openai/*-preview*", "legacy/*"},
	}
	if !FilterRule("openai/gpt-6-luna", rule) {
		t.Fatal("expected openai/gpt-6-luna to pass FilterRule")
	}
	if FilterRule("openai/gpt-6-preview-1", rule) {
		t.Fatal("expected exclude pattern to reject preview model")
	}
	if FilterRule("anthropic/claude-4", rule) {
		t.Fatal("expected non-included model to fail FilterRule")
	}
	if FilterRule("", rule) {
		t.Fatal("expected empty model to fail FilterRule")
	}
}

func TestHasBillingAndDeriveSupplierSlug(t *testing.T) {
	if !HasBilling(domain.ProviderRule{Adapter: AdapterOpenRouter}, domain.BillingFree) {
		t.Fatal("openrouter should default to free billing")
	}
	if HasBilling(domain.ProviderRule{Adapter: AdapterOpenRouter}, domain.BillingCommercial) {
		t.Fatal("openrouter should not default to commercial billing")
	}
	if !HasBilling(domain.ProviderRule{Adapter: AdapterVercel}, domain.BillingCommercial) {
		t.Fatal("vercel should default to supporting commercial billing")
	}

	tests := []struct {
		providerID string
		rule       domain.ProviderRule
		billing    string
		want       string
	}{
		{"transcribe-openrouter-free", domain.ProviderRule{Adapter: AdapterOpenRouter, Billing: []string{"free"}}, domain.BillingFree, "openrouter-free"},
		{"transcribe-bailian", domain.ProviderRule{Adapter: AdapterRegistry, Billing: []string{"commercial"}}, domain.BillingCommercial, "bailian"},
		{"vercel", domain.ProviderRule{Adapter: AdapterVercel, Billing: []string{"free", "commercial"}}, domain.BillingFree, "vercel-free"},
		{"vercel", domain.ProviderRule{Adapter: AdapterVercel, Billing: []string{"free", "commercial"}}, domain.BillingCommercial, "vercel-paid"},
		{"vllm-sr", domain.ProviderRule{Adapter: AdapterVLLMSR, Billing: []string{"free"}}, domain.BillingFree, "vllm-sr-free"},
		{"qiniu-paid", domain.ProviderRule{Adapter: AdapterRegistry, Billing: []string{"commercial"}}, domain.BillingCommercial, "qiniu-paid"},
		{"custom-id", domain.ProviderRule{SupplierSlug: "my-slug", Billing: []string{"commercial"}}, domain.BillingCommercial, "my-slug"},
	}
	for _, tc := range tests {
		if got := DeriveSupplierSlug(tc.providerID, tc.rule, tc.billing); got != tc.want {
			t.Errorf("DeriveSupplierSlug(%q, %+v, %q) = %q, want %q", tc.providerID, tc.rule, tc.billing, got, tc.want)
		}
	}
}

func TestEvaluatePricePolicy(t *testing.T) {
	mixedRule := domain.ProviderRule{
		Billing:         []string{domain.BillingFree, domain.BillingCommercial},
		PriceExceptions: []string{"openai/gpt-6-luna"},
	}

	if b, ok, pe := EvaluatePricePolicy(0, 0, mixedRule, "m-free"); b != domain.BillingFree || !ok || pe {
		t.Fatalf("zero price = (%q, %v, %v), want (free, true, false)", b, ok, pe)
	}
	if b, ok, pe := EvaluatePricePolicy(2.0, 10.0, mixedRule, "m-edge"); b != domain.BillingCommercial || !ok || pe {
		t.Fatalf("edge price = (%q, %v, %v), want (commercial, true, false)", b, ok, pe)
	}
	if b, ok, pe := EvaluatePricePolicy(2.01, 5.0, mixedRule, "m-expensive"); b != domain.BillingCommercial || ok || pe {
		t.Fatalf("over input ceiling = (%q, %v, %v), want (commercial, false, false)", b, ok, pe)
	}
	if b, ok, pe := EvaluatePricePolicy(15.0, 60.0, mixedRule, "openai/gpt-6-luna"); b != domain.BillingCommercial || !ok || !pe {
		t.Fatalf("price exception = (%q, %v, %v), want (commercial, true, true)", b, ok, pe)
	}
	if _, ok, _ := EvaluatePricePolicy(-1, -1, mixedRule, "auto"); ok {
		t.Fatal("negative price must be rejected when not in price_exceptions")
	}
	if _, ok, _ := EvaluatePricePolicy(math.NaN(), 1.0, mixedRule, "nan"); ok {
		t.Fatal("NaN price must be rejected")
	}

	customCeiling := domain.ProviderRule{
		Billing:                []string{domain.BillingCommercial},
		MaxInputUSDPerMillion:  0.5,
		MaxOutputUSDPerMillion: 1.5,
	}
	if _, ok, _ := EvaluatePricePolicy(0, 0, customCeiling, "free-model"); ok {
		t.Fatal("free model must be rejected when billing only enables commercial")
	}
	if _, ok, _ := EvaluatePricePolicy(0.6, 1.0, customCeiling, "m1"); ok {
		t.Fatal("custom input ceiling should reject 0.6 > 0.5")
	}
	if _, ok, _ := EvaluatePricePolicy(0.4, 1.5, customCeiling, "m2"); !ok {
		t.Fatal("custom ceiling should allow (0.4, 1.5)")
	}
}

func TestRankAndCap(t *testing.T) {
	rule := domain.ProviderRule{
		Preferred: []string{"paid-pref-1", "free-pref-2"},
		MaxCandidates: map[string]int{
			domain.BillingFree:       2,
			domain.BillingCommercial: 2,
		},
	}
	input := []domain.Candidate{
		{Model: "paid-z", Member: "p/paid-z", Billing: domain.BillingCommercial},
		{Model: "free-b", Member: "p/free-b", Billing: domain.BillingFree},
		{Model: "free-pref-2", Member: "p/free-pref-2", Billing: domain.BillingFree, Preferred: true},
		{Model: "paid-a", Member: "p/paid-a", Billing: domain.BillingCommercial},
		{Model: "free-a", Member: "p/free-a", Billing: domain.BillingFree},
		{Model: "paid-pref-1", Member: "p/paid-pref-1", Billing: domain.BillingCommercial, Preferred: true},
	}
	origFirst := input[0].Model
	got := RankAndCap(input, rule)
	if input[0].Model != origFirst {
		t.Fatal("RankAndCap must not mutate input slice")
	}

	wantOrder := []string{"paid-pref-1", "free-pref-2", "free-a", "paid-a"}
	if len(got) != len(wantOrder) {
		t.Fatalf("len(got) = %d (%v), want %d (%v)", len(got), got, len(wantOrder), wantOrder)
	}
	for i, m := range wantOrder {
		if got[i].Model != m {
			t.Fatalf("got[%d].Model = %q, want %q", i, got[i].Model, m)
		}
	}
}

func TestRegistryAdapter(t *testing.T) {
	adapter := NewRegistryAdapter()
	ctx := context.Background()

	// Unavailable provider yields no candidates.
	if cands, err := adapter.Discover(ctx, domain.Provider{ID: "p", Off: true, Models: []string{"m1"}}, domain.ProviderRule{Adapter: AdapterRegistry}, nil); err != nil || len(cands) != 0 {
		t.Fatalf("expected empty for off provider, got %v, err=%v", cands, err)
	}
	if cands, err := adapter.Discover(ctx, domain.Provider{ID: "p", SignedIn: boolPtr(false), Models: []string{"m1"}}, domain.ProviderRule{Adapter: AdapterRegistry}, nil); err != nil || len(cands) != 0 {
		t.Fatalf("expected empty for signedOut provider, got %v, err=%v", cands, err)
	}

	provider := domain.Provider{
		ID:        "transcribe-bailian",
		ChatURL:   "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
		APIKey:    "sk-test",
		Headers:   map[string]string{"X-Test": "1"},
		Models:    []string{"qwen3.8-max", "qwen3.8-flash", "disabled-m", "decider-m", "vision-m", "expensive-m", "qwen-free:free"},
		ModelsOff: []string{"disabled-m"},
		ModelMeta: map[string]domain.ModelMeta{
			"decider-m":   {Decides: true, Type: "language", OutputModalities: []string{"text"}},
			"vision-m":    {Type: "image", OutputModalities: []string{"image"}},
			"expensive-m": {Type: "language", OutputModalities: []string{"text"}, InputPrice: floatPtr(5.0), OutputPrice: floatPtr(20.0)},
			"qwen3.8-max": {Type: "language", OutputModalities: []string{"text"}, InputPrice: floatPtr(4.0), OutputPrice: floatPtr(12.0)},
		},
	}
	rule := domain.ProviderRule{
		Adapter:         AdapterRegistry,
		Preferred:       []string{"qwen3.8-max", "qwen3.8-flash"},
		PriceExceptions: []string{"qwen3.8-max"},
		Billing:         []string{domain.BillingCommercial},
		MaxCandidates:   map[string]int{domain.BillingCommercial: 3},
		Roles:           []string{domain.RoleTranscribe, domain.RoleReview},
	}
	served := map[string]bool{
		"transcribe-bailian/qwen3.8-max":         true,
		"transcribe-bailian/deepseek-v4.1-flash": true,
	}

	cands, err := adapter.Discover(ctx, provider, rule, served)
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	wantModels := []string{"qwen3.8-max", "qwen3.8-flash", "deepseek-v4.1-flash"}
	if len(cands) != len(wantModels) {
		t.Fatalf("got %d candidates %+v, want %v", len(cands), cands, wantModels)
	}
	for i, wm := range wantModels {
		if cands[i].Model != wm {
			t.Fatalf("cands[%d].Model = %q, want %q", i, cands[i].Model, wm)
		}
	}

	if cands[0].NeedsExpose {
		t.Fatal("qwen3.8-max is served, NeedsExpose must be false")
	}
	if !cands[0].PriceException || !cands[0].Preferred {
		t.Fatalf("expected qwen3.8-max Preferred=true, PriceException=true, got %+v", cands[0])
	}
	if cands[0].SupplierSlug != "bailian" {
		t.Fatalf("SupplierSlug = %q, want bailian", cands[0].SupplierSlug)
	}
	if !cands[1].NeedsExpose {
		t.Fatal("qwen3.8-flash is not served, NeedsExpose must be true")
	}
	wantHash := domain.EndpointKeyHash(provider.ChatURL, provider.APIKey, provider.Headers)
	if cands[0].EndpointKeyHash != wantHash {
		t.Fatalf("EndpointKeyHash = %q, want %q", cands[0].EndpointKeyHash, wantHash)
	}
	if !reflect.DeepEqual(cands[0].AllowedRoles, rule.Roles) {
		t.Fatalf("AllowedRoles = %v, want %v", cands[0].AllowedRoles, rule.Roles)
	}

	// Free registry provider check.
	freeProvider := domain.Provider{ID: "zen", Free: true, Models: []string{"m-a", "m-b"}}
	freeRule := domain.ProviderRule{Adapter: AdapterRegistry, Billing: []string{domain.BillingFree}}
	freeCands, err := adapter.Discover(ctx, freeProvider, freeRule, nil)
	if err != nil || len(freeCands) != 2 {
		t.Fatalf("free registry discover = (%v, %v)", freeCands, err)
	}
	if freeCands[0].Billing != domain.BillingFree || freeCands[0].SupplierSlug != "zen-free" {
		t.Fatalf("unexpected free candidate metadata: %+v", freeCands[0])
	}
}

func TestOpenRouterAdapter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "qwen/qwen-2.5-72b:free", "pricing": {"prompt": "0", "completion": "0.0"}, "architecture": {"output_modalities": ["text"]}},
				{"id": "meta-llama/llama-3.3:free", "pricing": {"prompt": "0.00000", "completion": 0}, "architecture": {"output_modalities": ["text"]}},
				{"id": "fake/non-zero:free", "pricing": {"prompt": "0.000001", "completion": "0"}},
				{"id": "zero-no-suffix/model", "pricing": {"prompt": "0", "completion": "0"}},
				{"id": "img/flux:free", "pricing": {"prompt": "0", "completion": "0"}, "architecture": {"output_modalities": ["image"]}},
				{"id": "disabled/model:free", "pricing": {"prompt": "0", "completion": "0"}},
				{"id": "deepseek/deepseek-chat", "pricing": {"prompt": "0.0000003", "completion": "0.0000012"}},
				{"id": "anthropic/claude-opus", "pricing": {"prompt": "0.000015", "completion": "0.000075"}},
				{"id": "openrouter/auto", "pricing": {"prompt": "-1", "completion": "-1"}}
			]
		}`))
	}))
	defer srv.Close()

	adapter := NewOpenRouterAdapter(srv.Client())
	provider := domain.Provider{
		ID:        "transcribe-openrouter-free",
		ChatURL:   "https://openrouter.ai/api/v1/chat/completions",
		APIKey:    "sk-or",
		ModelsOff: []string{"disabled/model:free"},
	}
	served := map[string]bool{
		"transcribe-openrouter-free/qwen/qwen-2.5-72b:free": true,
	}

	// Free-only mode with *:free glob.
	freeRule := domain.ProviderRule{
		Adapter:       AdapterOpenRouter,
		EndpointURL:   srv.URL,
		Include:       []string{"*:free"},
		Billing:       []string{domain.BillingFree},
		MaxCandidates: map[string]int{domain.BillingFree: 5},
	}
	cands, err := adapter.Discover(context.Background(), provider, freeRule, served)
	if err != nil {
		t.Fatalf("Discover free openrouter error: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates (%+v), want 2", len(cands), cands)
	}
	if cands[0].Model != "meta-llama/llama-3.3:free" || !cands[0].NeedsExpose {
		t.Fatalf("unexpected cands[0]: %+v", cands[0])
	}
	if cands[1].Model != "qwen/qwen-2.5-72b:free" || cands[1].NeedsExpose {
		t.Fatalf("unexpected cands[1]: %+v", cands[1])
	}
	if cands[0].SupplierSlug != "openrouter-free" {
		t.Fatalf("SupplierSlug = %q, want openrouter-free", cands[0].SupplierSlug)
	}

	// Mixed free + commercial with price_exceptions.
	mixedRule := domain.ProviderRule{
		Adapter:         AdapterOpenRouter,
		EndpointURL:     srv.URL,
		Billing:         []string{domain.BillingFree, domain.BillingCommercial},
		PriceExceptions: []string{"anthropic/claude-opus"},
	}
	mixedCands, err := adapter.Discover(context.Background(), provider, mixedRule, served)
	if err != nil {
		t.Fatalf("Discover mixed openrouter error: %v", err)
	}
	gotModels := make([]string, 0, len(mixedCands))
	for _, c := range mixedCands {
		gotModels = append(gotModels, c.Model)
	}
	// Free first (2 models), then commercial sorted lexicographically:
	// anthropic/claude-opus (exception), deepseek/deepseek-chat ($0.3/$1.2), fake/non-zero:free ($1/$0), zero-no-suffix/model ($0/$0 treated commercial).
	wantMixed := []string{
		"meta-llama/llama-3.3:free",
		"qwen/qwen-2.5-72b:free",
		"anthropic/claude-opus",
		"deepseek/deepseek-chat",
		"fake/non-zero:free",
		"zero-no-suffix/model",
	}
	if !reflect.DeepEqual(gotModels, wantMixed) {
		t.Fatalf("mixed openrouter models = %v, want %v", gotModels, wantMixed)
	}

	// HTTP failure test.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream error", http.StatusBadGateway)
	}))
	defer errSrv.Close()
	if _, err := adapter.Discover(context.Background(), provider, domain.ProviderRule{Adapter: AdapterOpenRouter, EndpointURL: errSrv.URL}, nil); err == nil {
		t.Fatal("expected HTTP 502 error")
	}
}

func TestVercelAdapter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "openai/gpt-6-luna", "type": "language", "pricing": {"input": "0.000015", "output": "0.00006"}},
				{"id": "google/gemini-3.8-flash", "type": "language", "pricing": {"input": "0.000002", "output": "0.00001"}},
				{"id": "cheap/per-million", "type": "language", "pricing": {"input": 1.5, "output": 8.0}},
				{"id": "too-pricey/model", "type": "language", "pricing": {"input": "0.000003", "output": "0.000012"}},
				{"id": "free/vercel-m", "type": "language", "pricing": {"input": "0", "output": "0"}},
				{"id": "free/fallback-m", "type": "language", "pricing": {"prompt": 0, "completion": "0.0"}},
				{"id": "embed/model", "type": "embedding", "pricing": {"input": "0", "output": "0"}},
				{"id": "remote-only/not-local", "type": "language", "pricing": {"input": "0", "output": "0"}}
			]
		}`))
	}))
	defer srv.Close()

	adapter := NewVercelAdapter(srv.Client())
	provider := domain.Provider{
		ID:      "vercel",
		ChatURL: "https://ai-gateway.vercel.sh/v1/chat/completions",
		APIKey:  "vck-key",
		Models: []string{
			"openai/gpt-6-luna",
			"google/gemini-3.8-flash",
			"cheap/per-million",
			"too-pricey/model",
			"free/vercel-m",
			"embed/model",
		},
	}
	served := map[string]bool{
		"vercel/free/fallback-m":         true,
		"vercel/google/gemini-3.8-flash": true,
	}
	rule := domain.ProviderRule{
		Adapter:                AdapterVercel,
		EndpointURL:            srv.URL,
		Preferred:              []string{"openai/gpt-6-luna", "google/gemini-3.8-flash"},
		PriceExceptions:        []string{"openai/gpt-6-luna"},
		MaxInputUSDPerMillion:  2.0,
		MaxOutputUSDPerMillion: 10.0,
		Billing:                []string{domain.BillingFree, domain.BillingCommercial},
		MaxCandidates:          map[string]int{domain.BillingFree: 2, domain.BillingCommercial: 3},
	}

	cands, err := adapter.Discover(context.Background(), provider, rule, served)
	if err != nil {
		t.Fatalf("Discover vercel error: %v", err)
	}

	// Expected order:
	// 1. openai/gpt-6-luna (preferred 0, commercial via price_exception, $15/$60)
	// 2. google/gemini-3.8-flash (preferred 1, commercial at exact $2/$10 boundary, NeedsExpose=false)
	// 3. free/fallback-m (free, served -> NeedsExpose=false)
	// 4. free/vercel-m (free, NeedsExpose=true)
	// 5. cheap/per-million (commercial $1.5/$8)
	// Excluded: too-pricey/model, embed/model, remote-only/not-local.
	want := []struct {
		model   string
		billing string
		slug    string
		expose  bool
	}{
		{"openai/gpt-6-luna", domain.BillingCommercial, "vercel-paid", true},
		{"google/gemini-3.8-flash", domain.BillingCommercial, "vercel-paid", false},
		{"free/fallback-m", domain.BillingFree, "vercel-free", false},
		{"free/vercel-m", domain.BillingFree, "vercel-free", true},
		{"cheap/per-million", domain.BillingCommercial, "vercel-paid", true},
	}
	if len(cands) != len(want) {
		t.Fatalf("got %d candidates (%+v), want %d", len(cands), cands, len(want))
	}
	for i, w := range want {
		c := cands[i]
		if c.Model != w.model || c.Billing != w.billing || c.SupplierSlug != w.slug || c.NeedsExpose != w.expose {
			t.Fatalf("cands[%d] = %+v, want %+v", i, c, w)
		}
	}
	if cands[0].InputUSDPerMillion != 15.0 || cands[0].OutputUSDPerMillion != 60.0 || !cands[0].PriceException {
		t.Fatalf("unexpected pricing on gpt-6-luna: %+v", cands[0])
	}
	if cands[1].InputUSDPerMillion != 2.0 || cands[1].OutputUSDPerMillion != 10.0 {
		t.Fatalf("boundary conversion error on gemini-3.8-flash: %+v", cands[1])
	}
}

func TestVLLMSRAdapter(t *testing.T) {
	body := `{"routes_to": ["qwen-plus", "deepseek-v3"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	adapter := NewVLLMSRAdapter(srv.Client())
	provider := domain.Provider{
		ID:      "vllm-sr",
		ChatURL: "https://app.vllm-sr.ai/pt/v1/chat/completions",
	}
	// Even if rule lists both roles, vllm-sr MUST restrict AllowedRoles strictly to transcribe.
	rule := domain.ProviderRule{
		Adapter:     AdapterVLLMSR,
		EndpointURL: srv.URL,
		Roles:       []string{domain.RoleTranscribe, domain.RoleReview},
	}

	cands, err := adapter.Discover(context.Background(), provider, rule, nil)
	if err != nil {
		t.Fatalf("Discover vllm-sr error: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Model != "vllm-sr/auto" || c.Member != "vllm-sr/vllm-sr/auto" {
		t.Fatalf("unexpected model/member: %+v", c)
	}
	if c.Billing != domain.BillingFree || !c.NeedsExpose {
		t.Fatalf("unexpected billing/expose: %+v", c)
	}
	if !reflect.DeepEqual(c.AllowedRoles, []string{domain.RoleTranscribe}) {
		t.Fatalf("AllowedRoles = %v, want [transcribe]", c.AllowedRoles)
	}
	if !strings.HasSuffix(c.SupplierSlug, "-free") {
		t.Fatalf("unexpected SupplierSlug %q", c.SupplierSlug)
	}

	// Served member sets NeedsExpose=false; nested data.routes_to also works.
	body = `{"data": {"routes_to": [{"model": "qwen-max"}]}}`
	cands, err = adapter.Discover(context.Background(), provider, rule, map[string]bool{"vllm-sr/vllm-sr/auto": true})
	if err != nil || len(cands) != 1 || cands[0].NeedsExpose {
		t.Fatalf("expected served candidate with NeedsExpose=false, got (%+v, %v)", cands, err)
	}

	// Review-only rule excludes vllm-sr.
	reviewRule := domain.ProviderRule{Adapter: AdapterVLLMSR, EndpointURL: srv.URL, Roles: []string{domain.RoleReview}}
	if cands, err := adapter.Discover(context.Background(), provider, reviewRule, nil); err != nil || len(cands) != 0 {
		t.Fatalf("expected empty for review-only rule, got (%+v, %v)", cands, err)
	}

	// Empty routes_to yields 0 candidates.
	body = `{"routes_to": []}`
	if cands, err := adapter.Discover(context.Background(), provider, rule, nil); err != nil || len(cands) != 0 {
		t.Fatalf("expected empty for empty routes_to, got (%+v, %v)", cands, err)
	}
}

func TestEdgeCasesAndContextCancellation(t *testing.T) {
	reg := NewDefaultRegistry(nil)

	// Cancelled context returns context.Canceled across all adapters.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, name := range reg.Names() {
		_, err := reg.Discover(cancelledCtx, domain.Provider{ID: "p", Models: []string{"m1"}}, domain.ProviderRule{Adapter: name}, nil)
		if err == nil {
			t.Fatalf("expected context error for adapter %q", name)
		}
	}

	// Registry.Discover dispatches properly for valid adapter.
	cands, err := reg.Discover(context.Background(), domain.Provider{
		ID:     "mixed",
		Models: []string{"zero-meta", "partial-meta"},
		ModelMeta: map[string]domain.ModelMeta{
			"zero-meta":    {InputPrice: floatPtr(0), OutputPrice: floatPtr(0)},
			"partial-meta": {InputPrice: floatPtr(1.5)},
		},
	}, domain.ProviderRule{
		Adapter: AdapterRegistry,
		Billing: []string{domain.BillingFree, domain.BillingCommercial},
	}, nil)
	if err != nil {
		t.Fatalf("Registry.Discover error: %v", err)
	}
	if len(cands) != 2 || cands[0].Model != "zero-meta" || cands[0].Billing != domain.BillingFree || cands[1].Model != "partial-meta" || cands[1].Billing != domain.BillingCommercial {
		t.Fatalf("unexpected Registry.Discover result: %+v", cands)
	}

	// Vercel with empty local models returns nil without error.
	vercel := NewVercelAdapter(nil)
	if vc, err := vercel.Discover(context.Background(), domain.Provider{ID: "vercel"}, domain.ProviderRule{Adapter: AdapterVercel}, nil); err != nil || len(vc) != 0 {
		t.Fatalf("expected empty when local models empty, got (%+v, %v)", vc, err)
	}
}
