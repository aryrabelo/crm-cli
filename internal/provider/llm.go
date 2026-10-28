package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// LLM defaults, read when the provider configuration leaves them unset.
const (
	EnvLLMBaseURL       = "CRM_LLM_BASE_URL"
	EnvLLMModel         = "CRM_LLM_MODEL"
	EnvLLMPriceIn       = "CRM_LLM_PRICE_IN"
	EnvLLMPriceOut      = "CRM_LLM_PRICE_OUT"
	DefaultLLMBaseURL   = "https://api.openai.com/v1"
	DefaultLLMMaxTokens = 512
)

// Token bounds used by Estimate. A byte-level BPE token covers at least one
// byte, so a message costs at most its byte length plus its framing;
// callOverheadTokens covers the chat template and any structured-output
// scaffolding a gateway injects around the messages.
const (
	callOverheadTokens    = 256
	messageOverheadTokens = 8
	maxRepairErrorBytes   = 200
)

const repairNote = "Your previous reply was rejected: %s. Reply again with only the JSON object described in the instructions, and no other text."

// Contract is the structured answer an LLM call must produce: Instructions
// is the system message (it must describe the JSON object), Check validates
// the decoded object and returns the value to store.
type Contract struct {
	Instructions string
	Check        func(answer map[string]any) (any, error)
}

// LLM is an OpenAI-compatible chat completions endpoint (POST
// {base}/chat/completions). Prices are micro-USD per 1M tokens.
type LLM struct {
	client    *http.Client
	name      string
	base      string
	model     string
	authRef   string
	token     string
	priceIn   Cost
	priceOut  Cost
	maxTokens int
}

// Bind returns the Adapter answering contract c: its input is the user
// prompt, its output the checked JSON answer. An answer that is not valid
// JSON (or fails Check) gets one repair request; a second invalid answer
// fails the call (not retryable).
func (l *LLM) Bind(c Contract) Adapter { return &boundLLM{llm: l, contract: c} }

// cost converts token counts to money, rounding up.
func (l *LLM) cost(in, out int64) Cost {
	micro := addCost(mulCost(in, l.priceIn), mulCost(out, l.priceOut))
	return addCost(micro, 999_999) / 1_000_000
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	MaxTokens      int           `json:"max_tokens"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// promptTokens bounds the prompt tokens of msgs.
func promptTokens(msgs []chatMessage) int64 {
	n := int64(callOverheadTokens)
	for _, m := range msgs {
		n += int64(len(m.Content)) + messageOverheadTokens
	}
	return n
}

// complete performs one chat completion. Usage is added to the context's
// Usage and priced; a response without usage is charged the worst case for
// the call (prompt bound, max tokens).
func (l *LLM) complete(ctx context.Context, msgs []chatMessage) (string, Cost, error) {
	payload := chatRequest{Model: l.model, Messages: msgs, MaxTokens: l.maxTokens}
	payload.ResponseFormat.Type = "json_object"
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, fmt.Errorf("encode %s request: %w", l.name, err)
	}
	req, err := http.NewRequest(http.MethodPost, l.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("build %s request: %w", l.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if l.token != "" {
		req.Header.Set("Authorization", "Bearer "+l.token)
	}

	resp, found, err := call(ctx, l.client, req)
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) && pe.Status == http.StatusUnauthorized && l.token == "" && l.authRef != "" {
			pe.Msg += " (environment variable " + l.authRef + " is not set)"
		}
		return "", 0, err
	}
	if !found {
		return "", 0, &Error{Status: http.StatusNotFound, Msg: fmt.Sprintf("%s: %s/chat/completions not found (check the base URL and model %q)", l.name, l.base, l.model)}
	}
	var r chatResponse
	decodeErr := json.Unmarshal(resp, &r)
	in, out := promptTokens(msgs), int64(l.maxTokens)
	// Usage is trusted only when both counters are present and sane;
	// otherwise ({}, a missing or negative counter) the worst case stands.
	if u := r.Usage; decodeErr == nil && u != nil && u.PromptTokens != nil && u.CompletionTokens != nil &&
		*u.PromptTokens >= 0 && *u.CompletionTokens >= 0 {
		in, out = *u.PromptTokens, *u.CompletionTokens
	}
	addUsage(ctx, in, out)
	cost := l.cost(in, out)
	if decodeErr != nil {
		return "", cost, &Error{Msg: l.name + ": decode response: " + decodeErr.Error()}
	}
	if len(r.Choices) == 0 || r.Choices[0].Message.Content == nil {
		return "", cost, nil
	}
	return *r.Choices[0].Message.Content, cost, nil
}

type boundLLM struct {
	llm      *LLM
	contract Contract
}

func (b *boundLLM) messages(prompt string) []chatMessage {
	return []chatMessage{{Role: "system", Content: b.contract.Instructions}, {Role: "user", Content: prompt}}
}

func (b *boundLLM) Key(in Input) Hash {
	l := b.llm
	return hashKey("llm", l.base, l.model, strconv.Itoa(l.maxTokens), b.contract.Instructions, in.Value)
}

// Estimate is the worst case of Enrich: the first call plus the repair call,
// which resends the prompt with the rejected answer (at most max tokens) and
// the repair note, each answering with max tokens.
func (b *boundLLM) Estimate(in Input) Cost {
	first := promptTokens(b.messages(in.Value))
	repair := first + int64(b.llm.maxTokens) + int64(len(repairNote)+maxRepairErrorBytes+2*messageOverheadTokens)
	max := int64(b.llm.maxTokens)
	// Enrich rounds each call's cost up separately, so the bound is the sum
	// of the two rounded costs, not one rounding of the combined tokens.
	return addCost(b.llm.cost(first, max), b.llm.cost(repair, max))
}

// InputSized: prompt tokens are priced, so the cost grows with the prompt.
func (b *boundLLM) InputSized() bool { return b.llm.priceIn > 0 }

func (b *boundLLM) Enrich(ctx context.Context, in Input) (Output, Cost, error) {
	msgs := b.messages(in.Value)
	content, cost, err := b.llm.complete(ctx, msgs)
	if err != nil {
		return nil, cost, err
	}
	out, invalid := b.answer(content)
	if invalid == nil {
		return out, cost, nil
	}
	msgs = append(msgs,
		chatMessage{Role: "assistant", Content: content},
		chatMessage{Role: "user", Content: fmt.Sprintf(repairNote, truncate(invalid.Error(), maxRepairErrorBytes))})
	content, repairCost, err := b.llm.complete(ctx, msgs)
	cost += repairCost
	if err != nil {
		return nil, cost, err
	}
	if out, invalid = b.answer(content); invalid != nil {
		return nil, cost, &Error{Msg: fmt.Sprintf("%s: invalid answer after one repair: %s", b.llm.name, invalid)}
	}
	return out, cost, nil
}

// answer decodes and checks a reply.
func (b *boundLLM) answer(content string) (Output, error) {
	obj, err := decodeObject(content)
	if err != nil {
		return nil, err
	}
	v, err := b.contract.Check(obj)
	if err != nil {
		return nil, err
	}
	enc, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode answer: %w", err)
	}
	return Output(enc), nil
}

// decodeObject parses a reply as a JSON object, tolerating a Markdown code
// fence or prose around the object.
func decodeObject(content string) (map[string]any, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return nil, errors.New("empty reply")
	}
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("reply is not a JSON object: %q", truncate(strings.TrimSpace(content), 80))
	}
	return obj, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// MaxPricePerMTok is the highest accepted llm token price, in USD per 1M
// tokens; anything above is a typo that would make budget arithmetic absurd.
const MaxPricePerMTok = 1e6

// usdPerMTok converts a USD-per-1M-tokens price to micro-USD per 1M tokens.
// ok is false for negative, non-finite and above MaxPricePerMTok prices.
func usdPerMTok(f float64) (Cost, bool) {
	if f > MaxPricePerMTok {
		return 0, false
	}
	return microUSD(f)
}

// Usage counts the LLM tokens of the calls made with a context from
// WithUsage. Safe for concurrent use.
type Usage struct {
	mu  sync.Mutex
	in  int64
	out int64
}

// Tokens returns the prompt (in) and completion (out) tokens counted so far.
func (u *Usage) Tokens() (in, out int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.in, u.out
}

type usageKey struct{}

// WithUsage returns a context whose LLM calls add their token usage to the
// returned Usage (the Adapter contract reports cost only).
func WithUsage(ctx context.Context) (context.Context, *Usage) {
	u := &Usage{}
	return context.WithValue(ctx, usageKey{}, u), u
}

func addUsage(ctx context.Context, in, out int64) {
	u, ok := ctx.Value(usageKey{}).(*Usage)
	if !ok {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.in += in
	u.out += out
}
