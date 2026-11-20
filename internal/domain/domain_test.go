package domain

import (
	"strings"
	"testing"
	"time"
)

func TestBeijingDate(t *testing.T) {
	utcLate := time.Date(2026, 11, 20, 16, 30, 0, 0, time.UTC)
	if got := BeijingDate(utcLate); got != "2026-11-21" {
		t.Fatalf("BeijingDate(%s) = %q, want 2026-11-21", utcLate, got)
	}
	utcEarly := time.Date(2026, 11, 20, 15, 59, 59, 0, time.UTC)
	if got := BeijingDate(utcEarly); got != "2026-11-20" {
		t.Fatalf("BeijingDate(%s) = %q, want 2026-11-20", utcEarly, got)
	}
}

func TestTrialIDAndHashes(t *testing.T) {
	id := TrialProviderID("qwen-plus")
	if !strings.HasPrefix(id, TrialProviderPrefix) {
		t.Fatalf("unexpected prefix: %q", id)
	}
	if len(id) != len(TrialProviderPrefix)+12 {
		t.Fatalf("unexpected length: %d (%q)", len(id), id)
	}
	if id != TrialProviderID("qwen-plus") {
		t.Fatal("TrialProviderID must be deterministic")
	}
	if TrialMember("qwen-plus") != id+"/qwen-plus" {
		t.Fatalf("unexpected TrialMember: %q", TrialMember("qwen-plus"))
	}

	h1 := EndpointKeyHash("https://example.com/v1/", "sk-1", map[string]string{"b": "2", "a": "1"})
	h2 := EndpointKeyHash("https://example.com/v1", "sk-1", map[string]string{"a": "1", "b": "2"})
	if h1 != h2 {
		t.Fatalf("EndpointKeyHash should ignore trailing slash and map order: %s != %s", h1, h2)
	}
	h3 := EndpointKeyHash("https://example.com/v1", "sk-2", map[string]string{"a": "1", "b": "2"})
	if h1 == h3 {
		t.Fatal("EndpointKeyHash must change when key changes")
	}

	c1 := AdmissionCacheKey(h1, "m1", "v1")
	c2 := AdmissionCacheKey(h1, "m1", "v2")
	if c1 == c2 {
		t.Fatal("AdmissionCacheKey must include suiteVersion")
	}
}

func TestAvailabilityFilters(t *testing.T) {
	signedOut := false
	signedIn := true

	if IsProviderAvailable(Provider{Off: true}) {
		t.Fatal("off provider must be unavailable")
	}
	if IsProviderAvailable(Provider{NoModels: true}) {
		t.Fatal("noModels provider must be unavailable")
	}
	if IsProviderAvailable(Provider{SignedIn: &signedOut}) {
		t.Fatal("signedIn=false provider must be unavailable")
	}

	p := Provider{
		ID:        "p1",
		SignedIn:  &signedIn,
		ModelsOff: []string{"disabled-model"},
		ModelMeta: map[string]ModelMeta{
			"decider":   {Decides: true, Type: "language", OutputModalities: []string{"text"}},
			"image-gen": {Type: "image", OutputModalities: []string{"image"}},
			"audio":     {Type: "language", OutputModalities: []string{"audio"}},
			"good":      {Type: "language", OutputModalities: []string{"text", "json"}},
		},
	}
	if !IsProviderAvailable(p) {
		t.Fatal("expected p to be available")
	}
	for _, bad := range []string{"", "disabled-model", "decider", "image-gen", "audio"} {
		if IsModelAvailable(p, bad) {
			t.Fatalf("expected model %q to be unavailable", bad)
		}
	}
	for _, good := range []string{"good", "unlisted-model"} {
		if !IsModelAvailable(p, good) {
			t.Fatalf("expected model %q to be available", good)
		}
	}
}
