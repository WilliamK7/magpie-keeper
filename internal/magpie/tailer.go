package magpie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

var _ domain.RouteTailer = (*Tailer)(nil)

// TailerOption configures a RouteTailer instance.
type TailerOption func(*Tailer)

// WithPollInterval overrides the polling interval for routing JSONL files.
func WithPollInterval(d time.Duration) TailerOption {
	return func(t *Tailer) {
		if d > 0 {
			t.pollInterval = d
		}
	}
}

// WithStartAtEOF skips pre-existing lines when Start is called; new files still start at byte 0.
func WithStartAtEOF(startAtEOF bool) TailerOption {
	return func(t *Tailer) { t.startAtEOF = startAtEOF }
}

// WithTrialPrefix overrides the provider ID prefix matched by the tailer.
func WithTrialPrefix(prefix string) TailerOption {
	return func(t *Tailer) {
		if prefix != "" {
			t.trialPrefix = prefix
		}
	}
}

// Tailer incrementally reads <magpie_dir>/data/routing/*.jsonl and emits RouteAlerts
// for matching Bailian trial sub-channel failures.
type Tailer struct {
	magpieDir    string
	pollInterval time.Duration
	startAtEOF   bool
	trialPrefix  string
}

// NewTailer creates a Tailer for a portable Magpie directory.
func NewTailer(magpieDir string, opts ...TailerOption) *Tailer {
	t := &Tailer{magpieDir: magpieDir, pollInterval: time.Second, trialPrefix: domain.TrialProviderPrefix}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

// Start begins polling routing JSONL logs and returns a channel that closes when ctx is canceled.
func (t *Tailer) Start(ctx context.Context) (<-chan domain.RouteAlert, error) {
	if strings.TrimSpace(t.magpieDir) == "" {
		return nil, errors.New("magpie_dir is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	offsets := make(map[string]int64)
	if t.startAtEOF {
		for _, f := range t.globRoutingFiles() {
			if info, err := os.Stat(f); err == nil && !info.IsDir() {
				offsets[f] = info.Size()
			}
		}
	}

	ch := make(chan domain.RouteAlert, 64)
	go t.run(ctx, ch, offsets)
	return ch, nil
}

func (t *Tailer) run(ctx context.Context, ch chan<- domain.RouteAlert, offsets map[string]int64) {
	defer close(ch)
	t.poll(ctx, ch, offsets)

	ticker := time.NewTicker(t.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.poll(ctx, ch, offsets)
		}
	}
}

func (t *Tailer) globRoutingFiles() []string {
	files, err := filepath.Glob(filepath.Join(t.magpieDir, "data", "routing", "*.jsonl"))
	if err != nil {
		return nil
	}
	sort.Strings(files)
	return files
}

func (t *Tailer) poll(ctx context.Context, ch chan<- domain.RouteAlert, offsets map[string]int64) {
	files := t.globRoutingFiles()
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if ctx.Err() != nil {
			return
		}
		info, err := os.Stat(file)
		if err != nil || info.IsDir() {
			continue
		}
		seen[file] = true
		t.readFileIncremental(ctx, file, info.Size(), ch, offsets)
	}
	for path := range offsets {
		if !seen[path] {
			delete(offsets, path)
		}
	}
}

func (t *Tailer) readFileIncremental(ctx context.Context, path string, size int64, ch chan<- domain.RouteAlert, offsets map[string]int64) {
	offset := offsets[path]
	if size < offset {
		offset = 0
	}
	if size == offset {
		offsets[path] = offset
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || len(data) == 0 {
		return
	}
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		offsets[path] = offset
		return
	}
	offsets[path] = offset + int64(lastNL+1)
	for _, rawLine := range bytes.Split(data[:lastNL+1], []byte{'\n'}) {
		if line := bytes.TrimSpace(rawLine); len(line) > 0 {
			for _, alert := range t.parseLineAlerts(line) {
				select {
				case ch <- alert:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

type rawRouteTry struct {
	Provider   string          `json:"provider"`
	ProviderID string          `json:"provider_id"`
	ProviderId string          `json:"providerId"`
	Member     string          `json:"member"`
	Model      string          `json:"model"`
	Fail       string          `json:"fail"`
	Status     int             `json:"status"`
	StatusCode int             `json:"status_code"`
	StatusAlt  int             `json:"statusCode"`
	Error      json.RawMessage `json:"error"`
	Err        json.RawMessage `json:"err"`
	Message    json.RawMessage `json:"message"`
	Msg        json.RawMessage `json:"msg"`
}

type rawRouteLine struct {
	Timestamp json.RawMessage `json:"timestamp"`
	TS        json.RawMessage `json:"ts"`
	Time      json.RawMessage `json:"time"`
	At        json.RawMessage `json:"at"`
	CreatedAt json.RawMessage `json:"created_at"`
	rawRouteTry
	Tries []rawRouteTry `json:"tries"`
}

func (t *Tailer) parseLineAlerts(line []byte) []domain.RouteAlert {
	var rec rawRouteLine
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}

	ts := parseFlexibleTimestamp(firstNonEmptyRaw(rec.Timestamp, rec.TS, rec.Time, rec.At, rec.CreatedAt))
	tries := rec.Tries
	if len(tries) == 0 {
		tries = []rawRouteTry{rec.rawRouteTry}
	}

	var alerts []domain.RouteAlert
	for _, item := range tries {
		providerID, model := resolveProviderAndModel(item, rec.rawRouteTry)
		if !strings.HasPrefix(providerID, t.trialPrefix) {
			continue
		}
		status := firstNonZero(item.Status, item.StatusCode, item.StatusAlt)
		errMsg := parseErrorText(firstNonEmptyRaw(item.Error, item.Err, item.Message, item.Msg))
		kind := classifyRouteFailure(item.Fail, status, errMsg)
		if kind == "" {
			continue
		}
		alerts = append(alerts, domain.RouteAlert{
			ProviderID: providerID,
			Model:      model,
			Kind:       kind,
			Error:      errMsg,
			Status:     status,
			Timestamp:  ts,
		})
	}
	return alerts
}

func resolveProviderAndModel(item, parent rawRouteTry) (string, string) {
	member := firstNonEmptyString(item.Member, parent.Member)
	var memberProvider, memberModel string
	if parts := strings.SplitN(member, "/", 2); len(parts) == 2 {
		memberProvider, memberModel = parts[0], parts[1]
	}
	return firstNonEmptyString(item.Provider, item.ProviderID, item.ProviderId, memberProvider),
		firstNonEmptyString(item.Model, memberModel, parent.Model)
}

func classifyRouteFailure(fail string, status int, errMsg string) string {
	if strings.EqualFold(strings.TrimSpace(fail), "credit") {
		return domain.RouteAlertAccountCredit
	}
	lower := strings.ToLower(errMsg)
	if strings.Contains(lower, "free quota exhausted") ||
		(strings.Contains(lower, "free tier") && strings.Contains(lower, "exhaust")) {
		return domain.RouteAlertQuotaExhausted
	}
	if status == 401 || status == 403 {
		return domain.RouteAlertPermissionDenied
	}
	return ""
}

func parseErrorText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
	}
	var obj struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil {
		if obj.Message != "" {
			return obj.Message
		}
		if len(obj.Error) > 0 {
			if inner := parseErrorText(obj.Error); inner != "" {
				return inner
			}
		}
	}
	return string(trimmed)
}

func parseFlexibleTimestamp(raw json.RawMessage) time.Time {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil || s == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts
			}
		}
		return time.Time{}
	}
	n, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	if n > 1_000_000_000_000 {
		return time.UnixMilli(n).UTC()
	}
	return time.Unix(n, 0).UTC()
}

func firstNonEmptyRaw(vals ...json.RawMessage) json.RawMessage {
	for _, v := range vals {
		if trimmed := bytes.TrimSpace(v); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
			return trimmed
		}
	}
	return nil
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonZero[T comparable](vals ...T) T {
	var zero T
	for _, v := range vals {
		if v != zero {
			return v
		}
	}
	return zero
}
