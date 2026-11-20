package magpie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"magpie-keeper/internal/domain"
)

const defaultMagpiePort = 3425

// ValidatePortableDir checks that magpieDir is a valid portable Magpie directory
// and verifies that settings.json's port matches magpieURL.
func ValidatePortableDir(magpieDir, magpieURL string) (int, error) {
	if strings.TrimSpace(magpieDir) == "" {
		return 0, errors.New("magpie_dir is empty")
	}

	if err := requireExists(filepath.Join(magpieDir, ".portable"), false); err != nil {
		return 0, fmt.Errorf("validate magpie_dir .portable: %w", err)
	}
	if err := requireExists(filepath.Join(magpieDir, "magpie.com"), true); err != nil {
		return 0, fmt.Errorf("validate magpie_dir magpie.com: %w", err)
	}
	if err := requireExists(filepath.Join(magpieDir, "data", "providers.json"), true); err != nil {
		return 0, fmt.Errorf("validate magpie_dir data/providers.json: %w", err)
	}

	port, err := readSettingsPort(filepath.Join(magpieDir, "data", "settings.json"))
	if err != nil {
		return 0, err
	}

	urlPort, err := parseURLPort(magpieURL)
	if err != nil {
		return 0, fmt.Errorf("validate magpie_url %q: %w", magpieURL, err)
	}
	if urlPort != port {
		return 0, fmt.Errorf("magpie port mismatch: settings.json port=%d, magpie_url port=%d", port, urlPort)
	}
	return port, nil
}

// ReadRegistry reads and merges portable Magpie registry, plugin providers,
// and endpoint-verified cached model lists into a RegistrySnapshot.
func (g *Gateway) ReadRegistry(ctx context.Context) (domain.RegistrySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.RegistrySnapshot{}, err
	}

	port, err := ValidatePortableDir(g.magpieDir, g.magpieURL)
	if err != nil {
		return domain.RegistrySnapshot{}, err
	}

	providers := make(map[string]domain.Provider)
	groups := make(map[string]domain.Group)

	providersPath := filepath.Join(g.magpieDir, "data", "providers.json")
	if err := loadRegistryFile(providersPath, false, providers, groups); err != nil {
		return domain.RegistrySnapshot{}, fmt.Errorf("read providers.json: %w", err)
	}

	pluginPath := filepath.Join(g.magpieDir, "data", "plugin-providers.json")
	if err := loadRegistryFile(pluginPath, true, providers, groups); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return domain.RegistrySnapshot{}, fmt.Errorf("read plugin-providers.json: %w", err)
		}
	}

	ids := sortedKeys(providers)
	cacheDir := filepath.Join(g.magpieDir, "data", "cache", "models")
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return domain.RegistrySnapshot{}, err
		}
		if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
			continue
		}
		p, err := applyModelCache(filepath.Join(cacheDir, id+".json"), providers[id])
		if err != nil {
			return domain.RegistrySnapshot{}, fmt.Errorf("read model cache for %q: %w", id, err)
		}
		providers[id] = p
	}

	return domain.RegistrySnapshot{
		MagpieDir: g.magpieDir,
		Port:      port,
		Providers: providers,
		Groups:    groups,
	}, nil
}

func requireExists(target string, mustBeFile bool) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if mustBeFile && info.IsDir() {
		return fmt.Errorf("%q is a directory, expected file", target)
	}
	return nil
}

func readSettingsPort(settingsPath string) (int, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultMagpiePort, nil
		}
		return 0, fmt.Errorf("read settings.json: %w", err)
	}

	data = bytes.TrimSpace(stripBOM(data))
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return 0, fmt.Errorf("parse settings.json: %w", err)
	}
	if obj == nil {
		return 0, errors.New("parse settings.json: expected JSON object")
	}

	rawPort, ok := obj["port"]
	if !ok || isJSONNull(rawPort) {
		return defaultMagpiePort, nil
	}

	var p flexInt
	if err := json.Unmarshal(rawPort, &p); err != nil || p < 0 || p > 65535 {
		return 0, fmt.Errorf("invalid port in settings.json: %s", string(bytes.TrimSpace(rawPort)))
	}
	if p == 0 {
		return defaultMagpiePort, nil
	}
	return int(p), nil
}

func parseURLPort(rawURL string) (int, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return 0, err
	}
	if u.Scheme == "" || u.Host == "" {
		return 0, errors.New("URL must include scheme and host")
	}
	if p := u.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil || port <= 0 || port > 65535 {
			return 0, fmt.Errorf("invalid port %q", p)
		}
		return port, nil
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return 80, nil
	case "https":
		return 443, nil
	default:
		return 0, fmt.Errorf("unsupported scheme %q without explicit port", u.Scheme)
	}
}

func loadRegistryFile(
	filePath string,
	asPlugin bool,
	providers map[string]domain.Provider,
	groups map[string]domain.Group,
) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	data = bytes.TrimSpace(stripBOM(data))
	if len(data) == 0 {
		return errors.New("empty JSON file")
	}
	if data[0] == '[' {
		return parseProviderSlice(data, asPlugin, providers)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	if top == nil {
		return errors.New("expected JSON object or array")
	}

	rawProviders, hasProviders := top["providers"]
	rawGroups, hasGroups := top["groups"]
	if hasProviders || hasGroups {
		if hasProviders && !isJSONNull(rawProviders) {
			if err := parseProvidersContainer(rawProviders, asPlugin, providers); err != nil {
				return fmt.Errorf("parse providers: %w", err)
			}
		}
		if hasGroups && !isJSONNull(rawGroups) {
			if err := parseGroupsContainer(rawGroups, groups); err != nil {
				return fmt.Errorf("parse groups: %w", err)
			}
		}
		return nil
	}

	for _, k := range sortedKeys(top) {
		raw := bytes.TrimSpace(top[k])
		if isJSONNull(raw) {
			continue
		}
		if !asPlugin && looksLikeGroupEntry(k, raw) {
			g, err := decodeGroup(k, raw)
			if err != nil {
				return fmt.Errorf("parse group %q: %w", k, err)
			}
			groups[g.Name] = g
			continue
		}
		p, err := decodeProvider(k, raw, asPlugin)
		if err != nil {
			return fmt.Errorf("parse provider %q: %w", k, err)
		}
		mergeProviderEntry(providers, p)
	}
	return nil
}

func applyModelCache(cachePath string, p domain.Provider) (domain.Provider, error) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		return p, err
	}
	var cache domain.ModelCache
	if err := json.Unmarshal(bytes.TrimSpace(stripBOM(data)), &cache); err != nil {
		return p, err
	}
	expected := providerEndpointBase(p)
	actual := normalizeEndpointURL(cache.Base)
	if expected == "" || actual == "" || expected != actual {
		return p, nil
	}
	for _, entry := range cache.Models {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		if !slices.Contains(p.Models, id) {
			p.Models = append(p.Models, id)
		}
		if p.ModelMeta == nil {
			p.ModelMeta = make(map[string]domain.ModelMeta)
		}
		cachedMeta := domain.ModelMeta{
			Decides:          entry.Decides,
			Type:             entry.Type,
			OutputModalities: slices.Clone(entry.OutputModalities),
			Free:             entry.Free,
		}
		if existing, ok := p.ModelMeta[id]; ok {
			p.ModelMeta[id] = mergeModelMeta(existing, cachedMeta)
		} else {
			p.ModelMeta[id] = cachedMeta
		}
	}
	return p, nil
}

func providerEndpointBase(p domain.Provider) string {
	if strings.TrimSpace(p.BaseURL) != "" {
		return normalizeEndpointURL(p.BaseURL)
	}
	if strings.TrimSpace(p.ChatURL) != "" {
		return deriveBaseFromChatURL(p.ChatURL)
	}
	return ""
}

func deriveBaseFromChatURL(rawChat string) string {
	norm := normalizeEndpointURL(rawChat)
	if norm == "" {
		return ""
	}
	u, err := url.Parse(norm)
	if err != nil {
		return ""
	}
	p := u.Path
	for _, suffix := range []string{"/chat/completions", "/completions", "/responses", "/messages", "/chat"} {
		if len(p) >= len(suffix) && strings.EqualFold(p[len(p)-len(suffix):], suffix) {
			p = strings.TrimRight(p[:len(p)-len(suffix)], "/")
			break
		}
	}
	u.Path = p
	return u.String()
}

func normalizeEndpointURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return ""
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	cleanPath := ""
	if u.Path != "" {
		cleanPath = strings.TrimRight(path.Clean("/"+u.Path), "/")
	}
	return (&url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     cleanPath,
		RawQuery: u.RawQuery,
	}).String()
}
