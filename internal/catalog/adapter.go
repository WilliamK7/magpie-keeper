package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

// Supported CatalogAdapter names matching data/channels.json.
const (
	AdapterRegistry   = "registry"
	AdapterOpenRouter = "openrouter"
	AdapterVercel     = "vercel"
	AdapterVLLMSR     = "vllm-sr"
)

const (
	defaultHTTPTimeout    = 30 * time.Second
	maxCatalogBodyBytes   = 32 << 20
	maxErrorSnippetBytes  = 512
	perTokenCeilingMarker = 0.01
)

// Registry maps adapter names to CatalogAdapter implementations.
type Registry struct {
	adapters map[string]domain.CatalogAdapter
}

// NewRegistry builds a Registry populated with the supplied adapters.
func NewRegistry(adapters ...domain.CatalogAdapter) *Registry {
	r := &Registry{
		adapters: make(map[string]domain.CatalogAdapter, len(adapters)),
	}
	for _, a := range adapters {
		r.Register(a)
	}
	return r
}

// NewDefaultRegistry returns a Registry containing all built-in catalog adapters.
func NewDefaultRegistry(httpClient *http.Client) *Registry {
	client := ensureHTTPClient(httpClient)
	return NewRegistry(
		NewRegistryAdapter(),
		NewOpenRouterAdapter(client),
		NewVercelAdapter(client),
		NewVLLMSRAdapter(client),
	)
}

// Register adds or replaces an adapter keyed by adapter.Name().
func (r *Registry) Register(adapter domain.CatalogAdapter) {
	if r == nil || adapter == nil {
		return
	}
	if r.adapters == nil {
		r.adapters = make(map[string]domain.CatalogAdapter)
	}
	r.adapters[adapter.Name()] = adapter
}

// Get retrieves a registered adapter by name.
func (r *Registry) Get(name string) (domain.CatalogAdapter, bool) {
	if r == nil {
		return nil, false
	}
	a, ok := r.adapters[strings.TrimSpace(name)]
	return a, ok
}

// Names returns all registered adapter names in sorted order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Discover delegates model discovery to the adapter named by rule.Adapter.
func (r *Registry) Discover(ctx context.Context, provider domain.Provider, rule domain.ProviderRule, served map[string]bool) ([]domain.Candidate, error) {
	adapter, ok := r.Get(rule.Adapter)
	if !ok {
		return nil, fmt.Errorf("catalog: unknown adapter %q for provider %q", rule.Adapter, provider.ID)
	}
	return adapter.Discover(ctx, provider, rule, served)
}

func newCandidate(provider domain.Provider, rule domain.ProviderRule, model, billing string, served map[string]bool, inUSD, outUSD float64, priceException bool, allowedRoles []string) domain.Candidate {
	member := provider.ID + "/" + model
	var roles []string
	if len(allowedRoles) > 0 {
		roles = slices.Clone(allowedRoles)
	}
	return domain.Candidate{
		ProviderID:          provider.ID,
		Model:               model,
		Member:              member,
		Billing:             billing,
		NeedsExpose:         !served[member],
		Preferred:           IsPreferred(rule, model),
		PriceException:      priceException,
		InputUSDPerMillion:  inUSD,
		OutputUSDPerMillion: outUSD,
		AllowedRoles:        roles,
		SupplierSlug:        DeriveSupplierSlug(provider.ID, rule, billing),
		EndpointKeyHash:     domain.EndpointKeyHash(provider.ChatURL, provider.APIKey, provider.Headers),
	}
}

func ensureHTTPClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

func resolveEndpoint(ruleURL, adapterURL, fallback string) string {
	if u := strings.TrimSpace(ruleURL); u != "" {
		return u
	}
	if u := strings.TrimSpace(adapterURL); u != "" {
		return u
	}
	return fallback
}

func fetchJSON(ctx context.Context, client *http.Client, endpoint string, dst any) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("catalog: endpoint URL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("catalog: build request %q: %w", endpoint, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "magpie-keeper/1.0")

	resp, err := ensureHTTPClient(client).Do(req)
	if err != nil {
		return fmt.Errorf("catalog: request %q: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippetBytes))
		return fmt.Errorf("catalog: %q returned HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBodyBytes)).Decode(dst); err != nil {
		return fmt.Errorf("catalog: decode JSON from %q: %w", endpoint, err)
	}
	return nil
}

// parseFlexiblePrice decodes a JSON number or numeric string into float64.
// Returns (0, false, nil) when the field is absent, null, or empty string.
func parseFlexiblePrice(raw json.RawMessage) (float64, bool, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0, false, nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, false, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, false, nil
		}
		val, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false, err
		}
		return val, true, nil
	}
	var val float64
	if err := json.Unmarshal(raw, &val); err != nil {
		return 0, false, err
	}
	return val, true, nil
}

func roundUSD(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	return math.Round(v*1e9) / 1e9
}

func containsFold(items []string, target string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}
