package magpie

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type flexBool bool

func (f *flexBool) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		*f = false
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "yes", "on", "1":
			*f = true
		case "false", "no", "off", "0", "":
			*f = false
		default:
			return fmt.Errorf("invalid boolean string %q", s)
		}
		return nil
	}
	var b bool
	if err := json.Unmarshal(trimmed, &b); err != nil {
		return err
	}
	*f = flexBool(b)
	return nil
}

type flexOffString string

func (f *flexOffString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		*f = ""
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*f = flexOffString(s)
		return nil
	}
	var b bool
	if err := json.Unmarshal(trimmed, &b); err != nil {
		return err
	}
	if !b {
		*f = "off"
	} else {
		*f = "on"
	}
	return nil
}

type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		*f = 0
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}
	n, err := strconv.Atoi(string(trimmed))
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

type flexStringList []string

func (f *flexStringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		*f = nil
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		var out []string
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		*f = out
		return nil
	}
	var out []string
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return err
	}
	*f = out
	return nil
}

type flexWeights map[string]int

func (f *flexWeights) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || isJSONNull(trimmed) {
		*f = nil
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		out := make(map[string]int)
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				return fmt.Errorf("invalid weight spec %q", part)
			}
			w, err := strconv.Atoi(strings.TrimSpace(kv[1]))
			if err != nil {
				return fmt.Errorf("invalid weight %q: %w", part, err)
			}
			out[strings.TrimSpace(kv[0])] = w
		}
		*f = out
		return nil
	}
	var m map[string]flexInt
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return err
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = int(v)
	}
	*f = out
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
