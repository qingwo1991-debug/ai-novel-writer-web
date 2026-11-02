// Package embedding implements the OpenAI and Gemini embedding wire protocols.
// Every dispatch requires an explicitly configured native egress boundary. No
// ambient HTTP client, proxy, redirect following, or provider SDK is used.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
)

type Protocol string

const (
	OpenAI Protocol = "openai"
	Gemini Protocol = "gemini"

	MaxTexts = 10000
	MaxTextBytes = 1 << 20
	MaxInputBytes = 32 << 20
	MaxRequestBytes = 64 << 20
	MaxResponseBytes = 16 << 20
	MaxDimension = 65536
	MaxNumbers = 1000000
	MaxJSONDepth = 64
	requestTimeout = 60 * time.Second
)

// All provider errors are fixed symbols: URLs, keys, response bodies, and native
// transport diagnostics are never included. Known egress/context errors and the
// caller's authority callback error retain their identity.
var (
	ErrRequestInvalid = errors.New("EMBEDDING_REQUEST_INVALID")
	ErrResponseInvalid = errors.New("EMBEDDING_RESPONSE_INVALID")
	ErrRequestFailed = errors.New("EMBEDDING_REQUEST_FAILED")
	ErrTooLarge = errors.New("EMBEDDING_TOO_LARGE")
	ErrOllamaNativeURL = errors.New("EMBEDDING_OLLAMA_NATIVE_URL")
)

type Model struct {
	BaseURL string
	APIKey string
	ModelName string
}

// Client must be constructed with New. A nil or zero client fails closed.
// The egress client remains owned by its creator, including its Close lifetime.
type Client struct {
	egress *egress.Client
}

func New(client *egress.Client) *Client { return &Client{egress: client} }

func (c *Client) ready(ctx context.Context, protocol Protocol) error {
	if ctx == nil || c == nil || c.egress == nil || (protocol != OpenAI && protocol != Gemini) {
		return ErrRequestInvalid
	}
	return ctx.Err()
}

// Generate applies protocol defaults, checks authority on both sides of every
// batch, and returns no partial results on any failure. Empty input needs no URL
// or credentials and makes no dispatch. Input must not be concurrently mutated.
func (c *Client) Generate(ctx context.Context, protocol Protocol, model Model, texts []string, configuredBatchSize *int, assertActive func() error) ([][]float64, error) {
	if err := c.ready(ctx, protocol); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return [][]float64{}, nil
	}
	if err := validateTexts(texts); err != nil {
		return nil, err
	}
	// Validate the whole model before invoking caller authority or sending a batch.
	if _, _, err := endpoint(protocol, model); err != nil {
		return nil, err
	}
	batchSize := 50
	if protocol == Gemini {
		batchSize = 100
	}
	if configuredBatchSize != nil {
		batchSize = *configuredBatchSize
		if batchSize < 1 { batchSize = 1 }
		if batchSize > 50 { batchSize = 50 }
	}
	results := make([][]float64, 0, len(texts))
	dimension, numbers := 0, 0
	for start := 0; start < len(texts); start += batchSize {
		if err := active(ctx, assertActive); err != nil { return nil, err }
		end := start + batchSize
		if end > len(texts) { end = len(texts) }
		vectors, err := c.EmbedBatch(ctx, protocol, model, texts[start:end])
		if err != nil { return nil, err }
		if err := active(ctx, assertActive); err != nil { return nil, err }
		for _, vector := range vectors {
			if dimension == 0 { dimension = len(vector) }
			if len(vector) != dimension { return nil, ErrResponseInvalid }
			numbers += len(vector)
			if numbers > MaxNumbers { return nil, ErrTooLarge }
		}
		results = append(results, vectors...)
	}
	if err := ctx.Err(); err != nil { return nil, err }
	return results, nil
}

func active(ctx context.Context, assertActive func() error) error {
	if assertActive != nil {
		if err := assertActive(); err != nil { return err }
	}
	return ctx.Err()
}

// EmbedBatch sends exactly one bounded native HTTP request and validates the
// complete response. The returned vectors are freshly allocated numeric slices.
func (c *Client) EmbedBatch(ctx context.Context, protocol Protocol, model Model, texts []string) ([][]float64, error) {
	if err := c.ready(ctx, protocol); err != nil { return nil, err }
	if err := validateTexts(texts); err != nil { return nil, err }
	target, modelName, err := endpoint(protocol, model)
	if err != nil { return nil, err }
	if len(texts) == 0 { return [][]float64{}, nil }
	payload, err := requestBody(protocol, modelName, texts)
	if err != nil { return nil, err }
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil { return nil, ErrRequestInvalid }
	request.Header.Set("Content-Type", "application/json")
	if protocol == OpenAI {
		request.Header.Set("Authorization", "Bearer " + model.APIKey)
	} else {
		request.Header.Set("x-goog-api-key", model.APIKey)
	}
	response, err := c.egress.Do(request)
	if err != nil { return nil, safeError(requestCtx, err) }
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrRequestFailed
	}
	if response.ContentLength > MaxResponseBytes { return nil, ErrTooLarge }
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes + 1))
	if err != nil { return nil, safeError(requestCtx, err) }
	if err := requestCtx.Err(); err != nil { return nil, err }
	if len(body) > MaxResponseBytes { return nil, ErrTooLarge }
	decoded, err := decodeResponse(body)
	if err != nil { return nil, err }
	vectors, err := responseVectors(protocol, decoded, len(texts))
	if err != nil { return nil, err }
	if err := requestCtx.Err(); err != nil { return nil, err }
	return vectors, nil
}

func safeError(ctx context.Context, err error) error {
	if ctx.Err() != nil { return ctx.Err() }
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, egress.ErrPolicyInvalid, egress.ErrNotAllowed, egress.ErrRedirectForbidden, egress.ErrRequestFailed} {
		if errors.Is(err, known) { return known }
	}
	return ErrRequestFailed
}

func validateTexts(texts []string) error {
	if len(texts) > MaxTexts { return ErrTooLarge }
	total := 0
	for _, text := range texts {
		if len(text) > MaxTextBytes { return ErrTooLarge }
		total += len(text)
		if total > MaxInputBytes { return ErrTooLarge }
		if !utf8.ValidString(text) { return ErrRequestInvalid }
	}
	return nil
}

// Query strings (including a bare '?'), fragments, and all userinfo are
// deliberately unsupported, unlike some Electron URL inputs. Never silently
// remove them. Egress performs final exact-origin/authority validation.
func endpoint(protocol Protocol, model Model) (string, string, error) {
	if len(model.BaseURL) > 8192 || len(model.APIKey) > 16384 || len(model.ModelName) > 1024 { return "", "", ErrTooLarge }
	if !utf8.ValidString(model.BaseURL) || !utf8.ValidString(model.APIKey) || !utf8.ValidString(model.ModelName) { return "", "", ErrRequestInvalid }
	if strings.ContainsAny(model.BaseURL, "?#\\ \t\r\n") || strings.ContainsAny(model.ModelName, "?#%\\ \t\r\n") { return "", "", ErrRequestInvalid }
	for _, value := range []string{model.BaseURL, model.APIKey, model.ModelName} {
		for _, char := range value { if char < 32 || char == 127 { return "", "", ErrRequestInvalid } }
	}
	parsed, err := url.Parse(model.BaseURL)
	if err != nil || parsed == nil || parsed.User != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.OmitHost || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "", ErrRequestInvalid
	}
	name := model.ModelName
	if protocol == Gemini {
		if name == "" { name = "gemini-embedding-001" }
		return strings.TrimSuffix(model.BaseURL, "/") + "/v1beta/models/" + name + ":batchEmbedContents", name, nil
	}
	if protocol != OpenAI { return "", "", ErrRequestInvalid }
	if parsed.Scheme == "http" && parsed.Port() == "11434" && (strings.EqualFold(parsed.Hostname(), "localhost") || parsed.Hostname() == "127.0.0.1") && strings.TrimRight(parsed.Path, "/") == "/api" {
		return "", "", ErrOllamaNativeURL
	}
	if name == "" { name = "text-embedding-3-small" }
	base := strings.TrimRight(model.BaseURL, "/")
	if strings.HasSuffix(base, "/embeddings") { return base, name, nil }
	switch strings.ToLower(base) {
	case "https://api.openai.com", "https://api.deepseek.com", "http://localhost:11434", "http://127.0.0.1:11434":
		return base + "/v1/embeddings", name, nil
	default:
		return base + "/embeddings", name, nil
	}
}

// ResolveURL resolves the protocol endpoint without performing network I/O.
func ResolveURL(protocol Protocol, model Model) (string, error) {
	target, _, err := endpoint(protocol, model)
	return target, err
}

// ValidateResponse validates a JSON value decoded with json.Decoder.UseNumber.
// The wire decoder additionally enforces UTF-8, depth, byte and single-value caps.
func ValidateResponse(protocol Protocol, raw any, batchLength int) ([][]float64, error) {
	if protocol != OpenAI && protocol != Gemini { return nil, ErrRequestInvalid }
	if batchLength < 0 { return nil, ErrRequestInvalid }
	if batchLength > MaxTexts { return nil, ErrTooLarge }
	return responseVectors(protocol, raw, batchLength)
}

func requestBody(protocol Protocol, modelName string, texts []string) ([]byte, error) {
	// Preflight worst-case structural overhead plus exact escaped text sizes so
	// json.Marshal itself cannot create an unbounded intermediate allocation.
	size := 128 + escapedSize(modelName)
	for _, text := range texts {
		size += escapedSize(text) + 128 + escapedSize(modelName)
		if size > MaxRequestBytes { return nil, ErrTooLarge }
	}
	var value any
	if protocol == OpenAI {
		value = struct {
			Model string `json:"model"`
			Input []string `json:"input"`
		}{modelName, texts}
	} else {
		type part struct { Text string `json:"text"` }
		type content struct { Parts []part `json:"parts"` }
		type item struct {
			Model string `json:"model"`
			Content content `json:"content"`
			TaskType string `json:"taskType"`
		}
		requests := make([]item, len(texts))
		for i, text := range texts { requests[i] = item{"models/" + modelName, content{[]part{{text}}}, "RETRIEVAL_DOCUMENT"} }
		value = struct { Requests []item `json:"requests"` }{requests}
	}
	body, err := json.Marshal(value)
	if err != nil { return nil, ErrRequestInvalid }
	if len(body) > MaxRequestBytes { return nil, ErrTooLarge }
	return body, nil
}

func escapedSize(value string) int {
	size := 2
	for _, char := range value {
		switch char {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t': size += 2
		case '<', '>', '&', '\u2028', '\u2029': size += 6
		default:
			if char < 32 { size += 6 } else { size += utf8.RuneLen(char) }
		}
	}
	return size
}

func decodeResponse(body []byte) (any, error) {
	if !utf8.Valid(body) { return nil, ErrResponseInvalid }
	// Scan nesting before decoding; brackets inside strings do not count.
	depth, quoted, escaped := 0, false, false
	for _, char := range body {
		if quoted {
			if escaped { escaped = false } else if char == '\\' { escaped = true } else if char == '"' { quoted = false }
			continue
		}
		switch char {
		case '"': quoted = true
		case '[', '{':
			depth++
			if depth > MaxJSONDepth { return nil, ErrResponseInvalid }
		case ']', '}': depth--
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil { return nil, ErrResponseInvalid }
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF { return nil, ErrResponseInvalid }
	return value, nil
}

func responseVectors(protocol Protocol, decoded any, count int) ([][]float64, error) {
	root, ok := decoded.(map[string]any)
	if !ok { return nil, ErrResponseInvalid }
	field, vectorField := "data", "embedding"
	if protocol == Gemini { field, vectorField = "embeddings", "values" }
	items, ok := root[field].([]any)
	if !ok || len(items) != count { return nil, ErrResponseInvalid }
	vectors := make([][]float64, count)
	dimension, numbers := 0, 0
	for position, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok { return nil, ErrResponseInvalid }
		index := position
		if protocol == OpenAI {
			number, ok := item["index"].(json.Number)
			if !ok { return nil, ErrResponseInvalid }
			value, err := number.Float64()
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= float64(count) || math.Trunc(value) != value { return nil, ErrResponseInvalid }
			index = int(value)
			if vectors[index] != nil { return nil, ErrResponseInvalid }
		}
		values, ok := item[vectorField].([]any)
		if !ok || len(values) == 0 { return nil, ErrResponseInvalid }
		if len(values) > MaxDimension { return nil, ErrTooLarge }
		if dimension == 0 { dimension = len(values) }
		if len(values) != dimension { return nil, ErrResponseInvalid }
		numbers += len(values)
		if numbers > MaxNumbers { return nil, ErrTooLarge }
		vector := make([]float64, len(values))
		for i, rawNumber := range values {
			number, ok := rawNumber.(json.Number)
			if !ok { return nil, ErrResponseInvalid }
			value, err := number.Float64()
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) { return nil, ErrResponseInvalid }
			vector[i] = value
		}
		vectors[index] = vector
	}
	for _, vector := range vectors { if vector == nil { return nil, ErrResponseInvalid } }
	return vectors, nil
}
