package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"studymind-backend/internal/models"
)

const bedrockModel = "apac.anthropic.claude-sonnet-4-20250514-v1:0"

// BedrockService wraps the Bedrock Runtime client for AI inference.
type BedrockService struct {
	client *bedrockruntime.Client
}

// claudeRequest is the Anthropic Messages API request body for Bedrock.
type claudeRequest struct {
	AnthropicVersion string          `json:"anthropic_version"`
	MaxTokens        int             `json:"max_tokens"`
	System           string          `json:"system"`
	Messages         []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// claudeResponse is the Anthropic Messages API response body.
type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// NewBedrockService creates a new Bedrock service.
func NewBedrockService() (*BedrockService, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}

	client := bedrockruntime.NewFromConfig(cfg)
	log.Println("[Bedrock] Service initialized with model:", bedrockModel)
	return &BedrockService{client: client}, nil
}

const chunkSize = 15 // segments per Bedrock call

// TranslateSegments chunks the transcript and translates each batch,
// then merges results with original timestamps.
func (b *BedrockService) TranslateSegments(segments []TranscriptSegment, targetLang string) ([]models.SubtitleLine, error) {
	var allSubtitles []models.SubtitleLine

	for i := 0; i < len(segments); i += chunkSize {
		end := i + chunkSize
		if end > len(segments) {
			end = len(segments)
		}
		chunk := segments[i:end]
		log.Printf("[Bedrock] Translating chunk %d-%d of %d", i+1, end, len(segments))

		var subtitles []models.SubtitleLine
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			subtitles, lastErr = b.translateChunk(chunk, targetLang)
			if lastErr == nil {
				break
			}
			log.Printf("[Bedrock] Chunk %d-%d attempt %d failed: %v", i+1, end, attempt, lastErr)
		}
		if lastErr != nil {
			return nil, fmt.Errorf("chunk %d-%d failed after 3 attempts: %w", i+1, end, lastErr)
		}
		allSubtitles = append(allSubtitles, subtitles...)
	}

	log.Printf("[Bedrock] Total: %d subtitle lines translated", len(allSubtitles))
	return allSubtitles, nil
}

func (b *BedrockService) translateChunk(segments []TranscriptSegment, targetLang string) ([]models.SubtitleLine, error) {
	// Use numbered format so Claude can't merge lines
	var numberedInput strings.Builder
	for i, seg := range segments {
		fmt.Fprintf(&numberedInput, "[%d] %s\n", i+1, seg.Text)
	}

	systemPrompt := fmt.Sprintf(`You are professional education translator in %s, Translate each numbered line into %s.
Return a JSON array of EXACTLY %d strings, one translation per line, in the same order.
Line [1] -> array index 0, line [2] -> array index 1, etc.

CRITICAL RULES:
- Output array MUST have EXACTLY %d elements
- Do NOT merge lines. Do NOT skip lines. Each numbered line = one array element.
- Even if a line is short (one word), it still gets its own translation.
- Keep technical terms in English (machine learning, deep learning, neural network, algorithm, etc.)
- Return ONLY the JSON array. No markdown. No explanation.`, targetLang, targetLang, len(segments), len(segments))

	reqBody := claudeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        8192,
		System:           systemPrompt,
		Messages: []claudeMessage{
			{Role: "user", Content: numberedInput.String()},
		},
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request error: %w", err)
	}

	log.Printf("[Bedrock] Invoking model for %d lines to %s", len(segments), targetLang)

	result, err := b.client.InvokeModel(context.TODO(), &bedrockruntime.InvokeModelInput{
		ModelId:     stringPtr(bedrockModel),
		ContentType: stringPtr("application/json"),
		Body:        reqBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock InvokeModel error: %w", err)
	}

	var resp claudeResponse
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal bedrock response error: %w", err)
	}

	if len(resp.Content) == 0 {
		return nil, fmt.Errorf("bedrock returned empty content")
	}

	rawJSON := resp.Content[0].Text
	log.Printf("[Bedrock] Received response (%d chars)", len(rawJSON))

	// Clean up common JSON issues from Claude's response
	cleaned := strings.TrimSpace(rawJSON)
	// Strip markdown code fences if present
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)
	// Fix missing commas between strings: }" "{  or ]"\n"[ patterns
	cleaned = strings.ReplaceAll(cleaned, "\"\n\"", "\",\"")
	cleaned = strings.ReplaceAll(cleaned, "\"\r\n\"", "\",\"")

	var translations []string
	if err := json.Unmarshal([]byte(cleaned), &translations); err != nil {
		return nil, fmt.Errorf("failed to parse translation JSON: %w\nRaw: %s", err, rawJSON)
	}

	log.Printf("[Bedrock] Expected %d translations, got %d", len(segments), len(translations))
	if len(translations) != len(segments) {
		return nil, fmt.Errorf("translation count mismatch: expected %d, got %d", len(segments), len(translations))
	}

	subtitles := make([]models.SubtitleLine, len(segments))
	for i, seg := range segments {
		subtitles[i] = models.SubtitleLine{
			StartSeconds:   seg.StartSeconds,
			EndSeconds:     seg.EndSeconds,
			TextEN:         seg.Text,
			TextTranslated: translations[i],
		}
	}

	return subtitles, nil
}

func stringPtr(s string) *string {
	return &s
}
