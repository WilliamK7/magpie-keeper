package magpie

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"magpie-keeper/internal/domain"
)

type rawProvider struct {
	ID           string                  `json:"id"`
	Kind         string                  `json:"kind"`
	ChatURL      string                  `json:"chat"`
	API          string                  `json:"api"`
	BaseURL      string                  `json:"base"`
	BaseAPI      string                  `json:"baseAPI"`
	ResponsesURL string                  `json:"responses"`
	APIKey       string                  `json:"key"`
	Models       flexModelList           `json:"models"`
	ModelsOff    flexStringList          `json:"modelsOff"`
	ModelMeta    map[string]rawModelMeta `json:"_models"`
	Headers      map[string]string       `json:"headers"`
	Off          flexBool                `json:"off"`
	NoModels     flexBool                `json:"noModels"`
	SignedIn     *flexBool               `json:"signedIn"`
	Free         flexBool                `json:"free"`
	List         flexOffString           `json:"list"`
	ListOff      flexBool                `json:"listOff"`
	RPM          flexInt                 `json:"rpm"`
	MaxRPM       flexInt                 `json:"maxRPM"`
	Plugin       flexBool                `json:"plugin"`
}

type rawModalities struct {
	Output flexStringList `json:"output"`
}

type rawModelMeta struct {
	Decides             flexBool       `json:"decides"`
	Type                string         `json:"type"`
	Outputs             flexStringList `json:"outputs"`
	OutputModalities    flexStringList `json:"outputModalities"`
	OutputModalitiesAlt flexStringList `json:"output_modalities"`
	Modalities          *rawModalities `json:"modalities"`
	Free                flexBool       `json:"free"`
	InputPrice          *float64       `json:"input_price"`
	InputPriceAlt       *float64       `json:"inputPrice"`
	OutputPrice         *float64       `json:"output_price"`
	OutputPriceAlt      *float64       `json:"outputPrice"`
}

type rawPluginModel struct {
	ID string `json:"id"`
	rawModelMeta
}

type flexModelList struct {
	IDs  []string
	Meta map[string]domain.ModelMeta
}

func (f *flexModelList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		f.IDs = nil
		f.Meta = nil
		return nil
	}
	if trimmed[0] == '"' {
		var list flexStringList
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		f.IDs = []string(list)
		return nil
	}
	if trimmed[0] != '[' {
		return errors.New("expected string or array for models")
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(trimmed, &rawItems); err != nil {
		return err
	}
	ids := make([]string, 0, len(rawItems))
	var meta map[string]domain.ModelMeta
	for _, rawItem := range rawItems {
		itemTrimmed := bytes.TrimSpace(rawItem)
		if len(itemTrimmed) == 0 || isJSONNull(itemTrimmed) {
			continue
		}
		if itemTrimmed[0] == '{' {
			var pm rawPluginModel
			if err := json.Unmarshal(itemTrimmed, &pm); err != nil {
				return err
			}
			id := strings.TrimSpace(pm.ID)
			if id == "" {
				continue
			}
			ids = append(ids, id)
			if meta == nil {
				meta = make(map[string]domain.ModelMeta)
			}
			meta[id] = pm.rawModelMeta.toDomain()
			continue
		}
		var s string
		if err := json.Unmarshal(itemTrimmed, &s); err != nil {
			return err
		}
		if s = strings.TrimSpace(s); s != "" {
			ids = append(ids, s)
		}
	}
	f.IDs = ids
	f.Meta = meta
	return nil
}

type flexGroupOff struct {
	Disabled bool
	Members  []string
}

func (f *flexGroupOff) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		*f = flexGroupOff{}
		return nil
	}
	if trimmed[0] == '[' {
		var members []string
		if err := json.Unmarshal(trimmed, &members); err != nil {
			return err
		}
		f.Members = members
		f.Disabled = false
		return nil
	}
	var b flexBool
	if err := json.Unmarshal(trimmed, &b); err != nil {
		return err
	}
	f.Disabled = bool(b)
	return nil
}

type rawGroup struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Models   flexStringList `json:"models"`
	Members  flexStringList `json:"members"`
	Routing  string         `json:"routing"`
	Stays    flexOffString  `json:"stays"`
	Affinity flexOffString  `json:"affinity"`
	Off      flexGroupOff   `json:"off"`
	Hidden   flexBool       `json:"hidden"`
	Weight   flexWeights    `json:"weight"`
	Weights  flexWeights    `json:"weights"`
}

func parseContainer(raw json.RawMessage, kind string, onItem func(key string, item json.RawMessage) error) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		return nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		for i, item := range items {
			if isJSONNull(item) {
				continue
			}
			if err := onItem("", item); err != nil {
				return fmt.Errorf("%s[%d]: %w", kind, i, err)
			}
		}
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return err
	}
	for _, k := range sortedKeys(m) {
		if isJSONNull(m[k]) {
			continue
		}
		if err := onItem(k, m[k]); err != nil {
			return fmt.Errorf("%s %q: %w", kind, k, err)
		}
	}
	return nil
}

func parseProvidersContainer(raw json.RawMessage, asPlugin bool, providers map[string]domain.Provider) error {
	return parseContainer(raw, "provider", func(k string, item json.RawMessage) error {
		p, err := decodeProvider(k, item, asPlugin)
		if err != nil {
			return err
		}
		if p.ID == "" {
			return errors.New("missing id")
		}
		mergeProviderEntry(providers, p)
		return nil
	})
}

func parseProviderSlice(raw json.RawMessage, asPlugin bool, providers map[string]domain.Provider) error {
	return parseProvidersContainer(raw, asPlugin, providers)
}

func parseGroupsContainer(raw json.RawMessage, groups map[string]domain.Group) error {
	return parseContainer(raw, "group", func(k string, item json.RawMessage) error {
		g, err := decodeGroup(k, item)
		if err != nil {
			return err
		}
		if g.Name == "" {
			return errors.New("missing name")
		}
		groups[g.Name] = g
		return nil
	})
}

func looksLikeGroupEntry(key string, raw json.RawMessage) bool {
	if strings.HasPrefix(key, "group/") {
		return true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return false
	}
	for _, pf := range []string{"chat", "api", "base", "baseAPI", "responses", "key", "kind", "modelsOff", "_models", "noModels", "signedIn", "free", "list", "listOff", "rpm", "maxRPM", "headers"} {
		if _, ok := fields[pf]; ok {
			return false
		}
	}
	for _, gf := range []string{"routing", "stays", "affinity", "members", "hidden", "weight", "weights"} {
		if _, ok := fields[gf]; ok {
			return true
		}
	}
	return false
}

func decodeProvider(defaultID string, raw json.RawMessage, asPlugin bool) (domain.Provider, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return domain.Provider{}, errors.New("expected JSON object")
	}
	var rp rawProvider
	if err := json.Unmarshal(trimmed, &rp); err != nil {
		return domain.Provider{}, err
	}
	id := defaultID
	if rp.ID != "" {
		id = rp.ID
	}
	var signedIn *bool
	if rp.SignedIn != nil {
		v := bool(*rp.SignedIn)
		signedIn = &v
	}
	chatURL := rp.ChatURL
	if chatURL == "" {
		chatURL = rp.API
	}
	if chatURL == "" {
		chatURL = rp.ResponsesURL
	}
	baseURL := rp.BaseURL
	if baseURL == "" {
		baseURL = rp.BaseAPI
	}
	if baseURL == "" {
		baseURL = rp.API
	}
	listVal := string(rp.List)
	if listVal == "" && bool(rp.ListOff) {
		listVal = "off"
	}
	rpmVal := int(rp.RPM)
	if rpmVal == 0 && int(rp.MaxRPM) > 0 {
		rpmVal = int(rp.MaxRPM)
	}
	var meta map[string]domain.ModelMeta
	if len(rp.Models.Meta) > 0 || len(rp.ModelMeta) > 0 {
		meta = make(map[string]domain.ModelMeta, len(rp.Models.Meta)+len(rp.ModelMeta))
		for k, item := range rp.Models.Meta {
			meta[k] = item
		}
		for k, item := range rp.ModelMeta {
			dm := item.toDomain()
			if cur, ok := meta[k]; ok {
				meta[k] = mergeModelMeta(cur, dm)
			} else {
				meta[k] = dm
			}
		}
	}
	return domain.Provider{
		ID:        id,
		Kind:      rp.Kind,
		ChatURL:   chatURL,
		BaseURL:   baseURL,
		APIKey:    rp.APIKey,
		Models:    rp.Models.IDs,
		ModelsOff: []string(rp.ModelsOff),
		ModelMeta: meta,
		Headers:   rp.Headers,
		Off:       bool(rp.Off),
		NoModels:  bool(rp.NoModels),
		SignedIn:  signedIn,
		Free:      bool(rp.Free),
		List:      listVal,
		RPM:       rpmVal,
		Plugin:    asPlugin || bool(rp.Plugin),
	}, nil
}

func (item rawModelMeta) toDomain() domain.ModelMeta {
	mods := []string(item.OutputModalities)
	if len(mods) == 0 {
		mods = []string(item.OutputModalitiesAlt)
	}
	if len(mods) == 0 {
		mods = []string(item.Outputs)
	}
	if len(mods) == 0 && item.Modalities != nil {
		mods = []string(item.Modalities.Output)
	}
	inPrice := item.InputPrice
	if inPrice == nil {
		inPrice = item.InputPriceAlt
	}
	outPrice := item.OutputPrice
	if outPrice == nil {
		outPrice = item.OutputPriceAlt
	}
	return domain.ModelMeta{
		Decides:          bool(item.Decides),
		Type:             item.Type,
		OutputModalities: mods,
		Free:             bool(item.Free),
		InputPrice:       inPrice,
		OutputPrice:      outPrice,
	}
}

func decodeGroup(defaultName string, raw json.RawMessage) (domain.Group, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return domain.Group{}, errors.New("expected JSON object")
	}
	var rg rawGroup
	if err := json.Unmarshal(trimmed, &rg); err != nil {
		return domain.Group{}, err
	}
	name := strings.TrimPrefix(defaultName, "group/")
	if rg.ID != "" {
		name = strings.TrimPrefix(rg.ID, "group/")
	} else if rg.Name != "" {
		name = strings.TrimPrefix(rg.Name, "group/")
	}
	models := []string(rg.Models)
	if len(models) == 0 {
		models = []string(rg.Members)
	}
	stays := string(rg.Stays)
	if stays == "" {
		stays = string(rg.Affinity)
	}
	var weights map[string]int
	for _, src := range []map[string]int{map[string]int(rg.Weight), map[string]int(rg.Weights)} {
		if len(src) > 0 {
			if weights == nil {
				weights = make(map[string]int, len(src))
			}
			for k, v := range src {
				weights[k] = v
			}
		}
	}
	return domain.Group{
		Name:    name,
		Models:  models,
		Routing: rg.Routing,
		Stays:   stays,
		Off:     rg.Off.Disabled,
		Hidden:  bool(rg.Hidden),
		Weights: weights,
	}, nil
}

func mergeProviderEntry(providers map[string]domain.Provider, incoming domain.Provider) {
	existing, exists := providers[incoming.ID]
	if !exists {
		providers[incoming.ID] = incoming
		return
	}
	merged := existing
	merged.Plugin = existing.Plugin || incoming.Plugin
	merged.Off = existing.Off || incoming.Off
	merged.NoModels = existing.NoModels || incoming.NoModels
	merged.Free = existing.Free || incoming.Free
	if incoming.SignedIn != nil {
		merged.SignedIn = incoming.SignedIn
	}
	fillEmpty(&merged.Kind, incoming.Kind)
	fillEmpty(&merged.ChatURL, incoming.ChatURL)
	fillEmpty(&merged.BaseURL, incoming.BaseURL)
	fillEmpty(&merged.APIKey, incoming.APIKey)
	fillEmpty(&merged.List, incoming.List)
	if merged.RPM == 0 {
		merged.RPM = incoming.RPM
	}
	merged.Models = mergeUniqueStrings(existing.Models, incoming.Models)
	merged.ModelsOff = mergeUniqueStrings(existing.ModelsOff, incoming.ModelsOff)
	for k, v := range incoming.Headers {
		if merged.Headers == nil {
			merged.Headers = make(map[string]string, len(incoming.Headers))
		}
		if _, ok := merged.Headers[k]; !ok {
			merged.Headers[k] = v
		}
	}
	for id, meta := range incoming.ModelMeta {
		if merged.ModelMeta == nil {
			merged.ModelMeta = make(map[string]domain.ModelMeta, len(incoming.ModelMeta))
		}
		if cur, ok := merged.ModelMeta[id]; ok {
			merged.ModelMeta[id] = mergeModelMeta(cur, meta)
		} else {
			merged.ModelMeta[id] = meta
		}
	}
	providers[incoming.ID] = merged
}

func fillEmpty(dst *string, src string) {
	if *dst == "" {
		*dst = src
	}
}

func mergeModelMeta(base, extra domain.ModelMeta) domain.ModelMeta {
	base.Decides = base.Decides || extra.Decides
	base.Free = base.Free || extra.Free
	if base.Type == "" {
		base.Type = extra.Type
	}
	if len(base.OutputModalities) == 0 && len(extra.OutputModalities) > 0 {
		base.OutputModalities = slices.Clone(extra.OutputModalities)
	}
	if base.InputPrice == nil {
		base.InputPrice = extra.InputPrice
	}
	if base.OutputPrice == nil {
		base.OutputPrice = extra.OutputPrice
	}
	return base
}

func mergeUniqueStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make([]string, 0, len(a)+len(b))
	seen := make(map[string]bool, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
