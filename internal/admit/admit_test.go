package admit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"magpie-keeper/internal/domain"
)

func validCase1JSON() string {
	return `{"1":"不要禁用缓存。","2":"它存储12个结果。"}`
}

func validCase2JSON() string {
	return `{"1":"如果温度超过35度，","2":"请停止水泵。"}`
}

func validCase3JSON() string {
	return `{` +
		`"1":"首先初始化3个工作线程。",` +
		`"2":"重试前请等待30秒。",` +
		`"3":"先不要清空缓存。",` +
		`"4":"它保留最近12条条目。",` +
		`"5":"将每次超时写入磁盘日志。",` +
		`"6":"再次检查队列深度。",` +
		`"7":"在高负载下对请求限流。",` +
		`"8":"传输完成后验证校验和。",` +
		`"9":"向监控器汇报完成情况。"` +
		`}`
}

func writeSSEChunks(w io.Writer, content, finishReason, reasoningContent string, reasoningTokens int, useTopLevelUsage bool) {
	w.(http.Flusher).Flush()
	fmt.Fprint(w, ": keep-alive\n\n")

	if reasoningContent != "" {
		chunk, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"delta": map[string]any{"reasoning_content": reasoningContent},
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
	}

	// Split content into two chunks to verify streaming concatenation.
	mid := len(content) / 2
	for mid > 0 && mid < len(content) && (content[mid]&0xc0) == 0x80 {
		mid++
	}
	parts := []string{content[:mid], content[mid:]}
	for _, p := range parts {
		if p == "" {
			continue
		}
		chunk, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"delta":         map[string]any{"content": p},
				"finish_reason": nil,
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
	}

	finishChunk, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}},
	})
	fmt.Fprintf(w, "data: %s\n\n", finishChunk)

	usageObj := map[string]any{
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": reasoningTokens,
		},
	}
	if useTopLevelUsage {
		usageObj = map[string]any{
			"reasoning_tokens": reasoningTokens,
		}
	}
	usageChunk, _ := json.Marshal(map[string]any{
		"choices": []any{},
		"usage":   usageObj,
	})
	fmt.Fprintf(w, "data: %s\n\n", usageChunk)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestParseStrictJSONObjectAndNumberTokens(t *testing.T) {
	t.Run("accepts code fenced object", func(t *testing.T) {
		raw := "  ```json\n{\"1\": \"不要禁用缓存。\", \"2\": \"它存储12个结果。\"}\n```  "
		got, err := ParseStrictJSONObject(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["1"] != "不要禁用缓存。" || got["2"] != "它存储12个结果。" {
			t.Fatalf("unexpected parsed map: %#v", got)
		}
	})

	t.Run("rejects duplicate keys", func(t *testing.T) {
		raw := `{"1": "不要禁用缓存。", "1": "重复", "2": "它存储12个结果。"}`
		if _, err := ParseStrictJSONObject(raw); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("expected duplicate key error, got %v", err)
		}
	})

	t.Run("rejects non-object and non-string values", func(t *testing.T) {
		for _, bad := range []string{`["1"]`, `{"1": 12}`, `{"1": null}`, `{"1": {"a":"b"}}`, `{"1":"a"} trailing`} {
			if _, err := ParseStrictJSONObject(bad); err == nil {
				t.Fatalf("expected error for %s", bad)
			}
		}
	})

	t.Run("extracts exact number tokens without substring false positive", func(t *testing.T) {
		toks := ExtractNumberTokens("重试前请等待30秒，保留12条。")
		if !toks["30"] || !toks["12"] {
			t.Fatalf("expected 30 and 12 in %#v", toks)
		}
		if toks["3"] {
			t.Fatalf("did not expect token \"3\" from \"30\": %#v", toks)
		}
	})
}

func TestEvaluateAllCasesPassAndRequestContract(t *testing.T) {
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := reqCount.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("unexpected Authorization: %q", got)
		}
		if got := r.Header.Get("X-Magpie-Response-Model"); got != "openrouter/model-a" {
			t.Errorf("unexpected X-Magpie-Response-Model: %q", got)
		}
		sess := r.Header.Get("X-Magpie-Session")
		if !strings.HasPrefix(sess, "keeper-admit-") || len(sess) != len("keeper-admit-")+8 {
			t.Errorf("unexpected X-Magpie-Session: %q", sess)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["model"] != "openrouter/model-a" || body["stream"] != true {
			t.Errorf("unexpected model/stream: %#v", body)
		}
		if body["max_tokens"] != float64(4096) {
			t.Errorf("unexpected max_tokens: %#v", body["max_tokens"])
		}
		rf, _ := body["response_format"].(map[string]any)
		if rf["type"] != "json_object" {
			t.Errorf("unexpected response_format: %#v", body["response_format"])
		}
		so, _ := body["stream_options"].(map[string]any)
		if so["include_usage"] != true {
			t.Errorf("unexpected stream_options: %#v", body["stream_options"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 1:
			writeSSEChunks(w, validCase1JSON(), "stop", "", 0, false)
		case 2:
			writeSSEChunks(w, validCase2JSON(), "stop", "", 0, false)
		case 3:
			writeSSEChunks(w, validCase3JSON(), "stop", "", 0, false)
		default:
			t.Errorf("unexpected extra request #%d", idx)
		}
	}))
	defer srv.Close()

	before := time.Now().UTC().Add(-time.Second)
	eval := NewEvaluator(srv.URL+"/v1/", "secret-key", srv.Client())
	res, err := eval.Evaluate(context.Background(), "openrouter/model-a")
	after := time.Now().UTC().Add(time.Second)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if res.Status != domain.AdmissionPassed {
		t.Fatalf("expected status passed, got %q (reason: %s)", res.Status, res.Reason)
	}
	if res.ReasoningTokens != 0 || res.SuiteVersion != SuiteVersion || res.EndpointKeyHash == "" {
		t.Fatalf("unexpected result metadata: %#v", res)
	}
	if res.EvaluatedAt.Before(before) || res.EvaluatedAt.After(after) || len(res.Cases) != 3 {
		t.Fatalf("unexpected evaluatedAt or cases: %#v", res)
	}
	for i, c := range res.Cases {
		if !c.Passed || c.FinishReason != "stop" {
			t.Fatalf("case %d did not pass cleanly: %#v", i, c)
		}
	}
}

func TestEvaluateDeterministicFailures(t *testing.T) {
	cases := DefaultCases()

	t.Run("case1 duplicate json keys rejected via SSE", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSEChunks(w, `{"1":"不要禁用缓存。","1":"重复","2":"它存储12个结果。"}`, "stop", "", 0, false)
		}))
		defer srv.Close()

		eval := NewEvaluator(srv.URL+"/v1", "k", srv.Client())
		res, err := eval.Evaluate(context.Background(), "m1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Status != domain.AdmissionFailed || !strings.Contains(res.Reason, "duplicate") {
			t.Fatalf("expected duplicate failure, got %#v", res)
		}
		if calls.Load() != 1 || len(res.Cases) != 1 {
			t.Fatalf("expected early stop after 1 case, got calls=%d cases=%d", calls.Load(), len(res.Cases))
		}
	})

	t.Run("case1 missing glossary term rejected", func(t *testing.T) {
		ok, reason := ValidateCaseResponse(cases[0], `{"1":"不要禁用存储。","2":"它存储12个结果。"}`, "stop", 0, "")
		if ok || !strings.Contains(reason, "缓存") {
			t.Fatalf("expected missing term failure, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("case1 missing negation marker rejected", func(t *testing.T) {
		ok, reason := ValidateCaseResponse(cases[0], `{"1":"禁用缓存。","2":"它存储12个结果。"}`, "stop", 0, "")
		if ok || !strings.Contains(reason, "negation") {
			t.Fatalf("expected negation failure, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("verbatim copy rejected", func(t *testing.T) {
		ok, reason := ValidateCaseResponse(cases[0], `{"1":"Do not disable the cache.","2":"它存储12个结果。"}`, "stop", 0, "")
		if ok {
			t.Fatalf("expected verbatim failure, got ok=%v", ok)
		}
		ok, reason = ValidateCaseResponse(cases[0], `{"1":"Do not disable the cache. 不要禁用缓存。","2":"它存储12个结果。"}`, "stop", 0, "")
		if ok || !strings.Contains(reason, "verbatim") {
			t.Fatalf("expected verbatim echo failure, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("case2 duplicate fragments rejected", func(t *testing.T) {
		ok, reason := ValidateCaseResponse(cases[1], `{"1":"如果温度超过35度，","2":"如果温度超过 35 度。"}`, "stop", 0, "")
		if ok || !strings.Contains(reason, "duplicate") {
			t.Fatalf("expected duplicate fragment failure, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("case3 missing exact number 3 when only 30 present", func(t *testing.T) {
		missing3 := `{` +
			`"1":"首先初始化工作线程。",` +
			`"2":"重试前请等待30秒。",` +
			`"3":"先不要清空缓存。",` +
			`"4":"它保留最近12条条目。",` +
			`"5":"将每次超时写入磁盘日志。",` +
			`"6":"再次检查队列深度。",` +
			`"7":"在高负载下对请求限流。",` +
			`"8":"传输完成后验证校验和。",` +
			`"9":"向监控器汇报完成情况。"` +
			`}`
		ok, reason := ValidateCaseResponse(cases[2], missing3, "stop", 0, "")
		if ok || !strings.Contains(reason, `"3"`) {
			t.Fatalf("expected missing exact number 3 failure, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("case3 cross-sentence long repetition rejected", func(t *testing.T) {
		repeated := `{` +
			`"1":"首先初始化3个工作线程。",` +
			`"2":"重试前请等待30秒。",` +
			`"3":"先不要清空缓存。",` +
			`"4":"它保留最近12条条目。",` +
			`"5":"将每次超时写入磁盘日志并重启服务。",` +
			`"6":"将每次超时写入磁盘日志再检查队列。",` +
			`"7":"在高负载下对请求限流。",` +
			`"8":"传输完成后验证校验和。",` +
			`"9":"向监控器汇报完成情况。"` +
			`}`
		ok, reason := ValidateCaseResponse(cases[2], repeated, "stop", 0, "")
		if ok || !strings.Contains(reason, "repetition") {
			t.Fatalf("expected cross-sentence repetition failure, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("case3 finish_reason other than stop rejected", func(t *testing.T) {
		ok, reason := ValidateCaseResponse(cases[2], validCase3JSON(), "length", 0, "")
		if ok || !strings.Contains(reason, "finish_reason") {
			t.Fatalf("expected finish_reason failure, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("reasoning tokens and reasoning_content rejected", func(t *testing.T) {
		srvTokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSEChunks(w, validCase1JSON(), "stop", "", 42, true)
		}))
		defer srvTokens.Close()

		eval := NewEvaluator(srvTokens.URL+"/v1", "k", srvTokens.Client())
		res, err := eval.Evaluate(context.Background(), "m-think")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Status != domain.AdmissionFailed || res.ReasoningTokens != 42 {
			t.Fatalf("expected failed with reasoning_tokens=42, got %#v", res)
		}

		srvDetails := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSEChunks(w, validCase1JSON(), "stop", "", 15, false)
		}))
		defer srvDetails.Close()

		evalDetails := NewEvaluator(srvDetails.URL+"/v1", "k", srvDetails.Client())
		resDetails, err := evalDetails.Evaluate(context.Background(), "m-think-details")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if resDetails.Status != domain.AdmissionFailed || resDetails.ReasoningTokens != 15 {
			t.Fatalf("expected failed with details reasoning_tokens=15, got %#v", resDetails)
		}

		srvReasoningDelta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSEChunks(w, validCase1JSON(), "stop", "let me think first", 0, false)
		}))
		defer srvReasoningDelta.Close()

		eval2 := NewEvaluator(srvReasoningDelta.URL+"/v1", "k", srvReasoningDelta.Client())
		res2, err := eval2.Evaluate(context.Background(), "m-think2")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res2.Status != domain.AdmissionFailed || !strings.Contains(res2.Reason, "reasoning_content") {
			t.Fatalf("expected reasoning_content failure, got %#v", res2)
		}
	})
}

func TestEvaluateHTTPAndTransportClassification(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantStatus string
	}{
		{name: "http 429 transient", statusCode: http.StatusTooManyRequests, wantStatus: domain.AdmissionTransient},
		{name: "http 503 transient", statusCode: http.StatusServiceUnavailable, wantStatus: domain.AdmissionTransient},
		{name: "http 408 transient", statusCode: http.StatusRequestTimeout, wantStatus: domain.AdmissionTransient},
		{name: "http 400 failed", statusCode: http.StatusBadRequest, wantStatus: domain.AdmissionFailed},
		{name: "http 403 failed", statusCode: http.StatusForbidden, wantStatus: domain.AdmissionFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "upstream error", tc.statusCode)
			}))
			defer srv.Close()

			eval := NewEvaluator(srv.URL+"/v1", "k", srv.Client())
			res, err := eval.Evaluate(context.Background(), "m1")
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if res.Status != tc.wantStatus {
				t.Fatalf("got status %q, want %q (reason: %s)", res.Status, tc.wantStatus, res.Reason)
			}
		})
	}

	t.Run("timeout classified as transient", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
		}))
		defer srv.Close()

		eval := NewEvaluator(srv.URL+"/v1", "k", srv.Client(), WithTimeout(40*time.Millisecond))
		res, err := eval.Evaluate(context.Background(), "m-slow")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Status != domain.AdmissionTransient {
			t.Fatalf("expected transient on timeout, got %q (%s)", res.Status, res.Reason)
		}
	})

	t.Run("malformed SSE classified as failed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {not-valid-json}\n\n")
		}))
		defer srv.Close()

		eval := NewEvaluator(srv.URL+"/v1", "k", srv.Client())
		res, err := eval.Evaluate(context.Background(), "m-bad-sse")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Status != domain.AdmissionFailed {
			t.Fatalf("expected failed on malformed sse, got %q (%s)", res.Status, res.Reason)
		}
	})

	t.Run("transport error classified as transient", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close()

		eval := NewEvaluator(url+"/v1", "k", nil, WithEndpointKeyHash("custom-hash"))
		res, err := eval.Evaluate(context.Background(), "m-down")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.Status != domain.AdmissionTransient || res.EndpointKeyHash != "custom-hash" {
			t.Fatalf("expected transient with custom-hash, got %#v", res)
		}
	})

	t.Run("parent context cancel returns error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		eval := NewEvaluator("http://127.0.0.1:1/v1", "k", nil)
		_, err := eval.Evaluate(ctx, "m1")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
}
