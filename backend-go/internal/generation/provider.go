package generation

import (
	"context"
	"errors"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
)

// ProviderRequest is constructed by a future trusted model/policy owner, never
// decoded from public RPC. Dispatch must enforce exact origin, DNS and current
// authority at each HTTP request; this adapter does not grant that authority.
type ProviderRequest struct {
	Adapter  string
	Model    llm.Model
	Messages []llm.Message
	Options  llm.Options
}

// NativeProvider joins existing native provider protocols to the INTERNAL run
// service. Known protocol evidence is attested here, but arithmetic settlement is
// still performed by SettleProviderUsage. No credentials enter durable requests.
// This function is not installed into any public handler or the main binary.
func NativeProvider(dispatch llm.Dispatch) Dispatch {
	return func(ctx context.Context, value any, callbacks Callbacks) (DispatchResult, error) {
		if ctx.Err() != nil {
			return DispatchResult{}, errors.New("GENERATION_CANCELLED")
		}
		request, ok := value.(ProviderRequest)
		if !ok || dispatch == nil || (request.Adapter != "openai" && request.Adapter != "gemini") {
			return DispatchResult{}, errors.New("GENERATION_PROVIDER_REQUEST_INVALID")
		}
		options := request.Options
		options.VisibleOnly = true
		var mu sync.Mutex
		terminal := false
		result := DispatchResult{}
		var failure error
		usageEvidence := map[string]any(nil)
		providerCallbacks := llm.Callbacks{
			OnChunk: func(text string) {
				mu.Lock()
				live := !terminal && ctx.Err() == nil
				mu.Unlock()
				if live && callbacks.OnVisible != nil {
					callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: text})
				}
			},
			OnReasoning: func(text string) {
				mu.Lock()
				live := !terminal && ctx.Err() == nil
				mu.Unlock()
				if live && callbacks.OnReasoning != nil {
					callbacks.OnReasoning(text)
				}
			},
			OnUsageEvidence: func(evidence llm.UsageEvidence) {
				mu.Lock()
				defer mu.Unlock()
				if terminal || ctx.Err() != nil || evidence.Protocol != request.Adapter {
					return
				}
				// Preserve raw Gemini completion=candidates, reasoning=thoughts. Normalized
				// llm.Usage already sums the two and would double-count separate billing.
				var fields map[string]any
				switch usage := evidence.Usage.(type) {
				case map[string]any:
					fields = usage
				case llm.Usage:
					count := func(value *float64) any {
						if value == nil {
							return nil
						}
						return *value
					}
					fields = map[string]any{"promptTokens": count(usage.PromptTokens), "completionTokens": count(usage.CompletionTokens), "totalTokens": count(usage.TotalTokens)}
				default:
					usageEvidence = nil
					return
				}
				reasoning := evidence.ReasoningTokens
				if value, ok := reasoning.(*float64); ok {
					if value == nil {
						reasoning = nil
					} else {
						reasoning = *value
					}
				}
				usageEvidence = map[string]any{"promptTokens": fields["promptTokens"], "completionTokens": fields["completionTokens"], "totalTokens": fields["totalTokens"], "reasoningTokens": reasoning, "accounting": evidence.Accounting, "totalIncludesReasoning": evidence.TotalIncludesReasoning, "trusted": true}
			},
			OnDone: func(_ string, _ *llm.Usage, finish string) {
				mu.Lock()
				defer mu.Unlock()
				if terminal {
					return
				}
				terminal = true
				if ctx.Err() != nil {
					failure = errors.New("GENERATION_CANCELLED")
					return
				}
				// Final content is cumulative; native OnChunk already delivered it. Never
				// append OnDone text or copy hidden reasoning from a fallback content string.
				result = DispatchResult{Usage: usageEvidence, FinishReason: finish}
			},
			OnError: func(code, _ string, _ *llm.Usage) {
				mu.Lock()
				defer mu.Unlock()
				if terminal {
					return
				}
				terminal = true
				if ctx.Err() != nil {
					failure = errors.New("GENERATION_CANCELLED")
					return
				}
				// Only semantic terminal context length is an execution policy signal. Raw
				// provider error strings/HTTP bodies/URLs never leave this adapter.
				switch code {
				case "LLM_CONTEXT_LENGTH_EXCEEDED":
					failure = errors.New("GENERATION_CONTEXT_LENGTH_EXCEEDED")
				case "LLM_STREAM_NETWORK_FAILED", "LLM_STREAM_UNREADABLE", "LLM_STREAM_INCOMPLETE":
					failure = errors.New("NETWORK_ERROR")
				default:
					failure = errors.New("GENERATION_PROVIDER_FAILED")
				}
			},
		}
		if request.Adapter == "openai" {
			provider := llm.OpenAI{Dispatch: dispatch}
			provider.Stream(ctx, request.Model, request.Messages, options, providerCallbacks)
		} else {
			provider := llm.Gemini{Dispatch: dispatch}
			provider.Stream(ctx, request.Model, request.Messages, options, providerCallbacks)
		}
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil {
			return DispatchResult{}, errors.New("GENERATION_CANCELLED")
		}
		if !terminal {
			return DispatchResult{}, errors.New("GENERATION_PROVIDER_FAILED")
		}
		return result, failure
	}
}
