package admit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"magpie-keeper/internal/domain"
)

const defaultEvalTimeout = 120 * time.Second

var _ domain.Admitter = (*Evaluator)(nil)

// Option configures an Evaluator instance.
type Option func(*Evaluator)

// WithTimeout overrides the overall evaluation timeout (default 120s).
func WithTimeout(d time.Duration) Option {
	return func(e *Evaluator) {
		if d > 0 {
			e.timeout = d
		}
	}
}

// WithEndpointKeyHash overrides the endpoint fingerprint stored on AdmissionResult.
func WithEndpointKeyHash(hash string) Option {
	return func(e *Evaluator) {
		if hash != "" {
			e.endpointKeyHash = hash
		}
	}
}

// Evaluator runs the deterministic synthetic admission suite against Magpie via streaming SSE.
type Evaluator struct {
	chatURL         string
	magpieKey       string
	endpointKeyHash string
	httpClient      *http.Client
	timeout         time.Duration
	cases           []CaseSpec
}

// NewEvaluator constructs a streaming SSE deterministic Admitter for Magpie.
func NewEvaluator(magpieURL, magpieKey string, httpClient *http.Client, opts ...Option) *Evaluator {
	baseURL := strings.TrimRight(strings.TrimSpace(magpieURL), "/")
	chatURL := ""
	if baseURL != "" {
		chatURL = baseURL + "/chat/completions"
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	e := &Evaluator{
		chatURL:         chatURL,
		magpieKey:       magpieKey,
		endpointKeyHash: domain.EndpointKeyHash(baseURL, magpieKey, nil),
		httpClient:      httpClient,
		timeout:         defaultEvalTimeout,
		cases:           DefaultCases(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
}

// Evaluate sequentially runs all synthetic admission cases for member, stopping early on failure.
func (e *Evaluator) Evaluate(ctx context.Context, member string) (domain.AdmissionResult, error) {
	if strings.TrimSpace(member) == "" || e.chatURL == "" {
		return domain.AdmissionResult{}, errors.New("admit: member and magpie URL are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return domain.AdmissionResult{}, ctx.Err()
	}

	evalStart := time.Now()
	evalCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	result := domain.AdmissionResult{
		Member:          member,
		Status:          domain.AdmissionPassed,
		SuiteVersion:    SuiteVersion,
		EndpointKeyHash: e.endpointKeyHash,
		Cases:           make([]domain.AdmissionCaseResult, 0, len(e.cases)),
	}

	for _, c := range e.cases {
		caseRes, status, err := e.runCase(evalCtx, ctx, member, c)
		if err != nil {
			return domain.AdmissionResult{}, err
		}
		result.Cases = append(result.Cases, caseRes)
		if caseRes.ReasoningTokens > result.ReasoningTokens {
			result.ReasoningTokens = caseRes.ReasoningTokens
		}
		if !caseRes.Passed {
			result.Status = status
			result.Reason = caseRes.Reason
			break
		}
	}

	result.DurationMS = time.Since(evalStart).Milliseconds()
	result.EvaluatedAt = time.Now().UTC()
	return result, nil
}

func (e *Evaluator) runCase(evalCtx, parentCtx context.Context, member string, c CaseSpec) (domain.AdmissionCaseResult, string, error) {
	caseStart := time.Now()
	caseRes := domain.AdmissionCaseResult{CaseID: c.ID}

	req, err := e.buildRequest(evalCtx, member, c)
	if err != nil {
		return caseRes, "", err
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		if errors.Is(parentCtx.Err(), context.Canceled) {
			return caseRes, "", parentCtx.Err()
		}
		caseRes.DurationMS = time.Since(caseStart).Milliseconds()
		caseRes.Reason = classifyTransportError(evalCtx, err)
		return caseRes, domain.AdmissionTransient, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		caseRes.DurationMS = time.Since(caseStart).Milliseconds()
		caseRes.Reason = strings.TrimSuffix(fmt.Sprintf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), ": ")
		if isTransientStatus(resp.StatusCode) {
			return caseRes, domain.AdmissionTransient, nil
		}
		return caseRes, domain.AdmissionFailed, nil
	}

	acc, status, reason, err := consumeSSE(resp.Body, evalCtx, parentCtx)
	caseRes.FinishReason = acc.finishReason
	caseRes.ReasoningTokens = acc.reasoningTokens
	caseRes.DurationMS = time.Since(caseStart).Milliseconds()
	if err != nil {
		return caseRes, "", err
	}
	if status != "" {
		caseRes.Reason = reason
		return caseRes, status, nil
	}

	ok, reason := ValidateCaseResponse(c, acc.content.String(), acc.finishReason, acc.reasoningTokens, acc.reasoning.String())
	if !ok {
		caseRes.Reason = reason
		return caseRes, domain.AdmissionFailed, nil
	}
	caseRes.Passed = true
	return caseRes, domain.AdmissionPassed, nil
}

func (e *Evaluator) buildRequest(ctx context.Context, member string, c CaseSpec) (*http.Request, error) {
	body, err := json.Marshal(map[string]any{
		"model":           member,
		"stream":          true,
		"max_tokens":      4096,
		"response_format": map[string]string{"type": "json_object"},
		"stream_options":  map[string]bool{"include_usage": true},
		"messages": []map[string]string{
			{"role": "system", "content": c.SystemPrompt},
			{"role": "user", "content": c.UserPrompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("admit: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.chatURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("admit: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Magpie-Response-Model", member)
	req.Header.Set("X-Magpie-Session", "keeper-admit-"+randomHex8())
	if e.magpieKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.magpieKey)
	}
	return req, nil
}

type sseAcc struct {
	content         strings.Builder
	reasoning       strings.Builder
	finishReason    string
	reasoningTokens int
	sawData         bool
	sawDone         bool
}

func consumeSSE(r io.Reader, evalCtx, parentCtx context.Context) (sseAcc, string, string, error) {
	var acc sseAcc
	reader := bufio.NewReader(r)
	for !acc.sawDone {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			if status, reason := acc.handleLine(strings.TrimRight(line, "\r\n")); status != "" {
				return acc, status, reason, nil
			}
		}
		if readErr != nil {
			if errors.Is(parentCtx.Err(), context.Canceled) {
				return sseAcc{}, "", "", parentCtx.Err()
			}
			if errors.Is(evalCtx.Err(), context.DeadlineExceeded) {
				return acc, domain.AdmissionTransient, "evaluation timeout exceeded", nil
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			return acc, domain.AdmissionTransient, classifyTransportError(evalCtx, readErr), nil
		}
	}
	if !acc.sawData {
		return acc, domain.AdmissionFailed, "empty sse stream", nil
	}
	return acc, "", "", nil
}

func (a *sseAcc) handleLine(trimmed string) (string, string) {
	if trimmed == "" || strings.HasPrefix(trimmed, ":") ||
		strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "id:") || strings.HasPrefix(trimmed, "retry:") {
		return "", ""
	}
	if !strings.HasPrefix(trimmed, "data:") {
		return domain.AdmissionFailed, "malformed sse stream line"
	}
	a.sawData = true
	data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if data == "" {
		return "", ""
	}
	if data == "[DONE]" {
		a.sawDone = true
		return "", ""
	}
	var chunk sseChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return domain.AdmissionFailed, fmt.Sprintf("malformed sse json: %v", err)
	}
	if chunk.Error != nil {
		msg := strings.TrimSpace(chunk.Error.Message)
		if msg == "" {
			msg = "sse error payload"
		}
		if isTransientSSEError(chunk.Error.Code) {
			return domain.AdmissionTransient, msg
		}
		return domain.AdmissionFailed, msg
	}
	a.applyChunk(chunk)
	return "", ""
}

func (a *sseAcc) applyChunk(chunk sseChunk) {
	if len(chunk.Choices) > 0 {
		ch := chunk.Choices[0]
		a.content.WriteString(ch.Delta.Content)
		a.reasoning.WriteString(ch.Delta.ReasoningContent)
		a.reasoning.WriteString(ch.Delta.Reasoning)
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.finishReason = *ch.FinishReason
		}
	}
	if chunk.Usage == nil {
		return
	}
	if d := chunk.Usage.CompletionTokensDetails; d != nil && d.ReasoningTokens > a.reasoningTokens {
		a.reasoningTokens = d.ReasoningTokens
	}
	if chunk.Usage.ReasoningTokens > a.reasoningTokens {
		a.reasoningTokens = chunk.Usage.ReasoningTokens
	}
}

func isTransientStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

func classifyTransportError(evalCtx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(evalCtx.Err(), context.DeadlineExceeded) {
		return "evaluation timeout exceeded"
	}
	return fmt.Sprintf("transport error: %v", err)
}

func isTransientSSEError(code any) bool {
	if f, ok := code.(float64); ok {
		return isTransientStatus(int(f))
	}
	if s, ok := code.(string); ok {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		return err == nil && isTransientStatus(n)
	}
	return false
}

func randomHex8() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type sseChunk struct {
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices,omitempty"`
	Usage *struct {
		ReasoningTokens         int `json:"reasoning_tokens"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}
