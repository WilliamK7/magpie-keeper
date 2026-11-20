package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"time"
)

// TrialHeaderName isolates Magpie cooldown keys across Bailian trial sub-channels.
const TrialHeaderName = "X-Transcribe-Trial-Model"

// TrialProviderPrefix is the fixed prefix of derived Bailian trial sub-channels.
const TrialProviderPrefix = "transcribe-bailian-trial-"

// BeijingZone uses a fixed +08:00 offset to avoid Windows tzdata lookup failures.
var BeijingZone = time.FixedZone("Asia/Shanghai", 8*3600)

// BeijingDate returns YYYY-MM-DD in Asia/Shanghai for t.
func BeijingDate(t time.Time) string {
	return t.In(BeijingZone).Format("2006-01-02")
}

// TrialProviderID deterministically derives the sub-channel provider ID for a model.
func TrialProviderID(model string) string {
	sum := sha256.Sum256([]byte(model))
	return TrialProviderPrefix + hex.EncodeToString(sum[:])[:12]
}

// TrialMember returns the Magpie member identifier "<trial_provider>/<model>".
func TrialMember(model string) string {
	return TrialProviderID(model) + "/" + model
}

// EndpointKeyHash computes a stable SHA-256 fingerprint for endpoint URL, key, and headers.
func EndpointKeyHash(chatURL, key string, headers map[string]string) string {
	h := sha256.New()
	h.Write([]byte(strings.TrimRight(strings.TrimSpace(chatURL), "/")))
	h.Write([]byte{0})
	h.Write([]byte(key))
	if len(headers) > 0 {
		keys := make([]string, 0, len(headers))
		for k := range headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte{0})
			h.Write([]byte(k))
			h.Write([]byte{'='})
			h.Write([]byte(headers[k]))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AdmissionCacheKey computes the SHA-256 cache key for an evaluation result.
func AdmissionCacheKey(endpointKeyHash, model, suiteVersion string) string {
	h := sha256.New()
	h.Write([]byte(endpointKeyHash))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(suiteVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// ManualHoldKey builds a canonical map key for a group/member manual hold.
func ManualHoldKey(group, member string) string {
	return group + "|" + member
}

// IsProviderAvailable reports whether a Magpie provider is enabled and signed in.
func IsProviderAvailable(p Provider) bool {
	if p.Off || p.NoModels {
		return false
	}
	if p.SignedIn != nil && !*p.SignedIn {
		return false
	}
	return true
}

// IsModelAvailable reports whether model passes provider-level and model-level
// availability filters per SPEC §4.1.
func IsModelAvailable(p Provider, model string) bool {
	if !IsProviderAvailable(p) || model == "" {
		return false
	}
	if slices.Contains(p.ModelsOff, model) {
		return false
	}
	if meta, ok := p.ModelMeta[model]; ok {
		if meta.Decides {
			return false
		}
		if meta.Type != "" && !strings.EqualFold(meta.Type, "language") {
			return false
		}
		if len(meta.OutputModalities) > 0 && !containsFold(meta.OutputModalities, "text") {
			return false
		}
	}
	return true
}

func containsFold(items []string, target string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}
