// SPDX-License-Identifier: LicenseRef-OpenLBM-Community-Source-1.0
// SPDX-FileCopyrightText: 2026 FutureBuild, Inc. and OpenLBM contributors

// Package ai is a single-key, OpenAI-compatible chat client pointed at
// OpenRouter (https://openrouter.ai/api/v1). It mirrors the proven GableLBM
// ai.Client shape: one runtime-settable API key, an open-weight default model,
// and graceful degradation — when no key is configured the client reports
// "not configured" so dependent features (e.g. the dispatch briefing) can
// degrade without hard-failing the core workflow.
//
// The client carries no vendor identity: OpenRouter's optional attribution
// headers are configuration (see Attribution), empty by default, so a
// self-hosted or white-labelled install is not reported under someone else's
// name. Point OPENROUTER_BASE_URL at a local OpenAI-compatible server
// (vLLM/Ollama) for a deployment with no third-party egress at all.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://openrouter.ai/api/v1"
	// defaultModel is an open-weight (OSS) model id. Override with OPENROUTER_MODEL.
	defaultModel = "meta-llama/llama-3.3-70b-instruct"

	// Environment keys for the optional attribution headers. Left unset, no
	// attribution is sent at all.
	envAppURL   = "OPENROUTER_APP_URL"
	envAppTitle = "OPENROUTER_APP_TITLE"
)

// ErrNotConfigured is returned by chat methods when no API key is set.
var ErrNotConfigured = errors.New("ai: OpenRouter API key not configured")

// Attribution carries OpenRouter's optional app-identification headers
// (HTTP-Referer and X-Title), which place a caller on OpenRouter's public app
// leaderboard and label the traffic in the key owner's dashboard.
//
// The zero value sends neither header, and that is the default. AI_LM used to
// hardcode this repository's URL and product name, which meant every
// self-hosted, white-labelled or forked deployment reported its customer's
// inference traffic under the original vendor's identity — wrong for the
// operator, and misleading on the leaderboard. Attribution is now something an
// operator opts into for their own deployment.
type Attribution struct {
	// URL is sent as HTTP-Referer. Typically the operator's own site or repo.
	URL string
	// Title is sent as X-Title. Typically the operator's product name.
	Title string
}

func (a Attribution) empty() bool { return a.URL == "" && a.Title == "" }

// Option configures a Client at construction.
type Option func(*Client)

// WithAttribution sets the OpenRouter attribution headers explicitly, taking
// precedence over the environment. This is the seam for wiring attribution
// through the application config once it carries these fields.
func WithAttribution(a Attribution) Option {
	return func(c *Client) {
		c.attribution = Attribution{
			URL:   strings.TrimSpace(a.URL),
			Title: strings.TrimSpace(a.Title),
		}
	}
}

// Client is an OpenRouter chat client.
type Client struct {
	apiKey      string
	baseURL     string
	model       string
	attribution Attribution
	http        *http.Client
}

// NewClient builds an OpenRouter client. An empty apiKey is valid: the client
// is then "not configured" and Generate returns ErrNotConfigured. baseURL and
// model fall back to OpenRouter defaults / an open-weight model when empty.
//
// Attribution defaults to whatever OPENROUTER_APP_URL / OPENROUTER_APP_TITLE
// say — nothing, unless the operator sets them — and can be overridden with
// WithAttribution.
func NewClient(apiKey, baseURL, model string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	c := &Client{
		apiKey:      strings.TrimSpace(apiKey),
		baseURL:     strings.TrimRight(baseURL, "/"),
		model:       model,
		attribution: attributionFromEnv(),
		http:        &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// attributionFromEnv reads the operator-supplied attribution, if any.
func attributionFromEnv() Attribution {
	return Attribution{
		URL:   strings.TrimSpace(os.Getenv(envAppURL)),
		Title: strings.TrimSpace(os.Getenv(envAppTitle)),
	}
}

// Attribution returns the attribution this client will send (zero value when
// none is configured).
func (c *Client) Attribution() Attribution {
	if c == nil {
		return Attribution{}
	}
	return c.attribution
}

// Configured reports whether an API key is present. Callers gate on this and
// choose their own degradation (the transport guards too as a backstop).
func (c *Client) Configured() bool { return c != nil && c.apiKey != "" }

// Model returns the configured model id (for surfacing in responses/logs).
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// --- wire types (OpenAI-compatible) -----------------------------------------

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

// Generate runs a single system+user chat completion and returns the assistant
// text. maxTokens caps the completion (0 ⇒ provider default). Returns
// ErrNotConfigured when no key is set.
func (c *Client) Generate(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (string, error) {
	if !c.Configured() {
		return "", ErrNotConfigured
	}

	reqBody := chatRequest{
		Model:       c.model,
		MaxTokens:   maxTokens,
		Temperature: 0.3,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	// Attribution is opt-in and carries no vendor identity by default, so an
	// unconfigured deployment is anonymous to the leaderboard rather than
	// mislabelled as someone else's.
	if !c.attribution.empty() {
		if c.attribution.URL != "" {
			httpReq.Header.Set("HTTP-Referer", c.attribution.URL)
		}
		if c.attribution.Title != "" {
			httpReq.Header.Set("X-Title", c.attribution.Title)
		}
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ai: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("ai: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var er chatResponse
		if json.Unmarshal(respBody, &er) == nil && er.Error != nil {
			return "", fmt.Errorf("ai: OpenRouter error (%d): %s", resp.StatusCode, er.Error.Message)
		}
		return "", fmt.Errorf("ai: OpenRouter error (%d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return "", fmt.Errorf("ai: parse response: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("ai: OpenRouter error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("ai: empty response (no choices)")
	}
	text := strings.TrimSpace(cr.Choices[0].Message.Content)
	if text == "" {
		slog.Warn("ai: model returned empty content", "model", cr.Model)
	}
	return text, nil
}
