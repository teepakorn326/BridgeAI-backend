package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
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

	// Disable the SDK's internal retry budget AND rate limiter — we have
	// our own exponential backoff in TranslateSegments. NopRetryer alone
	// only stops retries; the rate limiter still depletes tokens and
	// refuses calls with "0 available, 5 requested" errors.
	client := bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		o.Retryer = retry.NewStandard(func(so *retry.StandardOptions) {
			so.MaxAttempts = 1
			so.RateLimiter = ratelimit.None
		})
	})
	log.Println("[Bedrock] Service initialized with model:", bedrockModel)
	return &BedrockService{client: client}, nil
}

const (
	chunkSize   = 15 // segments per Bedrock call
	concurrency = 5  // parallel Bedrock calls (lower = fewer 429s)
	maxAttempts = 6  // total attempts per chunk with exponential backoff
)

// TranslateSegments chunks the transcript and translates batches in parallel,
// then merges results with original timestamps.
func (b *BedrockService) TranslateSegments(segments []TranscriptSegment, targetLang string) ([]models.SubtitleLine, error) {
	// Build chunks
	var chunks [][]TranscriptSegment
	for i := 0; i < len(segments); i += chunkSize {
		end := i + chunkSize
		if end > len(segments) {
			end = len(segments)
		}
		chunks = append(chunks, segments[i:end])
	}

	log.Printf("[Bedrock] Translating %d segments in %d chunks (concurrency=%d)", len(segments), len(chunks), concurrency)

	results := make([][]models.SubtitleLine, len(chunks))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for idx, chunk := range chunks {
		wg.Add(1)
		go func(idx int, chunk []TranscriptSegment) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var subtitles []models.SubtitleLine
			var lastErr error
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				subtitles, lastErr = b.translateChunk(chunk, targetLang)
				if lastErr == nil {
					break
				}
				log.Printf("[Bedrock] Chunk %d attempt %d failed: %v", idx+1, attempt, lastErr)
				if attempt < maxAttempts {
					// Exponential backoff with jitter: 1s, 2s, 4s, 8s, 16s
					base := time.Duration(1<<(attempt-1)) * time.Second
					jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
					time.Sleep(base + jitter)
				}
			}
			if lastErr != nil {
				// Fallback: use English text so the rest of the lecture still works.
				log.Printf("[Bedrock] Chunk %d permanently failed, using fallback: %v", idx+1, lastErr)
				subtitles = make([]models.SubtitleLine, len(chunk))
				for j, seg := range chunk {
					subtitles[j] = models.SubtitleLine{
						StartSeconds:   seg.StartSeconds,
						EndSeconds:     seg.EndSeconds,
						TextEN:         seg.Text,
						TextTranslated: seg.Text,
					}
				}
			}
			results[idx] = subtitles
		}(idx, chunk)
	}

	wg.Wait()

	// Flatten preserving order
	var allSubtitles []models.SubtitleLine
	for _, r := range results {
		allSubtitles = append(allSubtitles, r...)
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

	systemPrompt := fmt.Sprintf(`You are a professional educational translator. Your job is to translate English lecture subtitles into natural, fluent %s for university students.

TASK: Translate each numbered line into %s.
OUTPUT: a JSON array of EXACTLY %d strings, one translation per numbered line, in order.

ABSOLUTE RULES (violations will be rejected):
1. Every output string MUST be written in %s. Do NOT return the English input unchanged.
2. Translate everyday English naturally into %s. However, KEEP domain-specific technical terms in English — these are specialized terms that university students and professionals in the field commonly use in English even when speaking %s. This applies to ALL academic fields (science, engineering, medicine, business, law, etc.), not just IT.
3. Also keep in English: acronyms, abbreviations, product/brand names, code identifiers, mathematical symbols, and proper nouns.
4. The translation must sound natural to a %s-speaking university student — mix %s grammar with English technical terms as they would in a real classroom or textbook.
5. Output array MUST have EXACTLY %d elements. Do NOT merge, skip, or split lines. Line [1] -> index 0, line [2] -> index 1, etc. One numbered line = one array element, even if it is only one word.
6. Return ONLY the raw JSON array. No markdown fences. No commentary. No preamble.

If a line is a single technical term, acronym, or number, keep it as-is. Otherwise translate the non-technical parts.`,
		targetLang, targetLang, len(segments), targetLang, targetLang, targetLang, targetLang, targetLang, len(segments))

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
		diff := len(segments) - len(translations)
		// Tolerate small mismatches (±2): pad with English or truncate.
		// This is common when Claude merges very short segments like "OK."
		if diff >= -2 && diff <= 2 {
			log.Printf("[Bedrock] Count off by %d, auto-adjusting", diff)
			if diff > 0 {
				// Missing translations: pad with source text
				for j := len(translations); j < len(segments); j++ {
					translations = append(translations, segments[j].Text)
				}
			} else {
				// Too many translations: truncate
				translations = translations[:len(segments)]
			}
		} else {
			return nil, fmt.Errorf("translation count mismatch: expected %d, got %d", len(segments), len(translations))
		}
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

// invokeClaude calls Bedrock with a system + user message and returns the raw text response.
// Used by Summarize/GenerateQuiz/ExtractVocab — no chunking, single call.
func (b *BedrockService) invokeClaude(systemPrompt, userMessage string, maxTokens int) (string, error) {
	reqBody := claudeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        maxTokens,
		System:           systemPrompt,
		Messages:         []claudeMessage{{Role: "user", Content: userMessage}},
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request error: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := b.client.InvokeModel(context.TODO(), &bedrockruntime.InvokeModelInput{
			ModelId:     stringPtr(bedrockModel),
			ContentType: stringPtr("application/json"),
			Body:        reqBytes,
		})
		if err != nil {
			lastErr = err
			log.Printf("[Bedrock] invokeClaude attempt %d failed: %v", attempt, err)
			if attempt < maxAttempts {
				base := time.Duration(1<<(attempt-1)) * time.Second
				jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
				time.Sleep(base + jitter)
			}
			continue
		}

		var resp claudeResponse
		if err := json.Unmarshal(result.Body, &resp); err != nil {
			return "", fmt.Errorf("unmarshal response: %w", err)
		}
		if len(resp.Content) == 0 {
			return "", fmt.Errorf("bedrock returned empty content")
		}
		return resp.Content[0].Text, nil
	}
	return "", fmt.Errorf("invokeClaude failed after %d attempts: %w", maxAttempts, lastErr)
}

// ChatMessage is a single turn in a conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCitation maps an inline [N] marker in a chat answer to the transcript
// segment it references, so the frontend can render "jump to timestamp" buttons.
type ChatCitation struct {
	N            int     `json:"n"`
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	TextEN       string  `json:"text_en"`
}

// ChatWithTranscript answers a question about a lecture transcript in the
// target language. The answer contains inline [N] markers; each N resolves to
// a ChatCitation pointing at the segment the model relied on.
func (b *BedrockService) ChatWithTranscript(segments []TranscriptSegment, targetLang string, history []ChatMessage) (string, []ChatCitation, error) {
	var numbered strings.Builder
	for i, seg := range segments {
		fmt.Fprintf(&numbered, "[%d] (%s) %s\n", i+1, formatTS(seg.StartSeconds), seg.Text)
	}

	system := fmt.Sprintf(`You are a helpful study assistant for a university student watching a recorded lecture.
Answer their question IN %s, grounded strictly in the numbered transcript below.
Keep English technical terms (API, GPU, ReLU, CNN, SQL, etc.) in English.
If the transcript does not contain the answer, say so honestly IN %s — do not invent facts.
Be concise and explain at undergraduate level. Use plain conversational prose; avoid heavy markdown (no ## headings, no --- separators). Short paragraphs and simple **bold** or bullet lists are fine.

OUTPUT FORMAT — STRICT:
1. The FIRST line of your response MUST be a single-line JSON block of the form:
   <CITES>[{"n":1,"seg":42},{"n":2,"seg":57}]</CITES>
   - Emit this line EVEN IF you don't plan citations: <CITES>[]</CITES>
   - One entry per distinct [N] marker you will use in the answer.
   - "n" MUST equal the exact integer you put inside brackets in the answer (e.g. [1] -> {"n":1,...}). Never use arbitrary numbers like 1001.
   - "seg" is the 1-based segment number from the numbered transcript below (the leading [N] on each transcript line) that supports that claim.
2. After the <CITES> line, write your answer in %s.
3. Inside the answer, whenever you make a factual claim or paraphrase, append the matching [N] marker. Use small numbers starting from [1]. You may reuse the same marker across sentences.

Putting <CITES> first guarantees citations survive even if your answer is long — so plan it before you write.

NUMBERED TRANSCRIPT:
%s`, targetLang, targetLang, targetLang, numbered.String())

	msgs := make([]claudeMessage, 0, len(history))
	for _, m := range history {
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		msgs = append(msgs, claudeMessage{Role: role, Content: m.Content})
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
		return "", nil, fmt.Errorf("chat history must end with a user message")
	}

	reqBody := claudeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        4096,
		System:           system,
		Messages:         msgs,
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", nil, fmt.Errorf("marshal chat request: %w", err)
	}

	result, err := b.client.InvokeModel(context.TODO(), &bedrockruntime.InvokeModelInput{
		ModelId:     stringPtr(bedrockModel),
		ContentType: stringPtr("application/json"),
		Body:        reqBytes,
	})
	if err != nil {
		return "", nil, fmt.Errorf("bedrock chat error: %w", err)
	}

	var resp claudeResponse
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		return "", nil, fmt.Errorf("unmarshal chat response: %w", err)
	}
	if len(resp.Content) == 0 {
		return "", nil, fmt.Errorf("bedrock returned empty chat content")
	}

	answer, citations := parseChatCitations(resp.Content[0].Text, segments)
	return answer, citations, nil
}

// parseChatCitations extracts the <CITES>[...]</CITES> block — which may be
// at the start, end, or middle of the response — and returns the answer text
// with the block stripped, plus the resolved citations.
//
// The model is instructed to emit <CITES> FIRST so citations survive answer
// truncation, but the parser doesn't rely on position.
//
// Robust to truncation: if the block opens but never closes (max_tokens hit
// inside the JSON), strip from <CITES> to end so no raw tag reaches the user.
func parseChatCitations(raw string, segments []TranscriptSegment) (string, []ChatCitation) {
	openIdx := strings.Index(raw, "<CITES>")
	if openIdx < 0 {
		return strings.TrimSpace(raw), nil
	}
	before := raw[:openIdx]

	closeIdx := strings.Index(raw, "</CITES>")
	if closeIdx < 0 || closeIdx < openIdx {
		// Truncated — keep whatever came before the open tag as the answer.
		return strings.TrimSpace(before), nil
	}

	after := raw[closeIdx+len("</CITES>"):]
	answer := strings.TrimSpace(before + after)
	payload := strings.TrimSpace(raw[openIdx+len("<CITES>") : closeIdx])

	var rows []struct {
		N   int `json:"n"`
		Seg int `json:"seg"`
	}
	if err := json.Unmarshal([]byte(payload), &rows); err != nil {
		return answer, nil
	}

	out := make([]ChatCitation, 0, len(rows))
	for _, r := range rows {
		idx := r.Seg - 1
		if idx < 0 || idx >= len(segments) {
			continue
		}
		s := segments[idx]
		out = append(out, ChatCitation{
			N:            r.N,
			StartSeconds: s.StartSeconds,
			EndSeconds:   s.EndSeconds,
			TextEN:       s.Text,
		})
	}
	return answer, out
}

// Chapter is a topical segment of a lecture identified by the model —
// not a fixed-time slice.
type Chapter struct {
	StartSeconds    float64 `json:"start_seconds"`
	EndSeconds      float64 `json:"end_seconds"`
	TitleEN         string  `json:"title_en"`
	TitleTranslated string  `json:"title_translated"`
}

// GenerateChapters asks the model to group the transcript into 5-10 topical
// chapters with bilingual titles, then resolves segment indices to timestamps.
func (b *BedrockService) GenerateChapters(segments []TranscriptSegment, targetLang string) ([]Chapter, error) {
	if len(segments) == 0 {
		return nil, nil
	}

	var numbered strings.Builder
	for i, seg := range segments {
		fmt.Fprintf(&numbered, "[%d] (%s) %s\n", i+1, formatTS(seg.StartSeconds), seg.Text)
	}

	system := fmt.Sprintf(`You are an expert at organizing lecture content into logical, topical chapters — NOT fixed-time slices.

TASK: Read the numbered transcript and identify 5-10 chapters that match how the instructor actually structured the material (introduction, main concepts, worked examples, summary, etc).

OUTPUT: ONLY a JSON array, no markdown fences, no preamble. Each chapter:
- "start_seg": the 1-based segment number where this chapter begins.
- "title_en": a short English title, 3-6 words, sentence case, no trailing punctuation.
- "title_translated": the same title in %s. Keep technical terms in English (ReLU, API, SQL, gradient descent, etc.).

RULES:
- First chapter always starts at "start_seg": 1.
- Chapters must be in order and non-overlapping.
- 5 chapters minimum, 10 maximum.
- No empty titles.`, targetLang)

	reqBody := claudeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        2048,
		System:           system,
		Messages:         []claudeMessage{{Role: "user", Content: numbered.String()}},
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal chapters request: %w", err)
	}

	log.Printf("[Bedrock] GenerateChapters: %d segments → %s", len(segments), targetLang)
	result, err := b.client.InvokeModel(context.TODO(), &bedrockruntime.InvokeModelInput{
		ModelId:     stringPtr(bedrockModel),
		ContentType: stringPtr("application/json"),
		Body:        reqBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock chapters error: %w", err)
	}

	var resp claudeResponse
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal chapters response: %w", err)
	}
	if len(resp.Content) == 0 {
		return nil, fmt.Errorf("bedrock returned empty chapters content")
	}

	var rows []struct {
		StartSeg        int    `json:"start_seg"`
		TitleEN         string `json:"title_en"`
		TitleTranslated string `json:"title_translated"`
	}
	if err := json.Unmarshal([]byte(cleanJSON(resp.Content[0].Text)), &rows); err != nil {
		return nil, fmt.Errorf("parse chapters JSON: %w\nRaw: %s", err, resp.Content[0].Text)
	}

	chapters := make([]Chapter, 0, len(rows))
	lastEnd := segments[len(segments)-1].EndSeconds
	for i, r := range rows {
		idx := r.StartSeg - 1
		if idx < 0 || idx >= len(segments) || r.TitleEN == "" {
			continue
		}
		end := lastEnd
		if i+1 < len(rows) {
			nextIdx := rows[i+1].StartSeg - 1
			if nextIdx > idx && nextIdx < len(segments) {
				end = segments[nextIdx].StartSeconds
			}
		}
		chapters = append(chapters, Chapter{
			StartSeconds:    segments[idx].StartSeconds,
			EndSeconds:      end,
			TitleEN:         r.TitleEN,
			TitleTranslated: r.TitleTranslated,
		})
	}
	return chapters, nil
}

// formatTS renders seconds as mm:ss for prompt-side timestamps.
func formatTS(seconds float64) string {
	total := int(seconds)
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// joinSegments concatenates segment texts into a single transcript string.
func joinSegments(segments []TranscriptSegment) string {
	var sb strings.Builder
	for _, s := range segments {
		sb.WriteString(s.Text)
		sb.WriteString(" ")
	}
	return sb.String()
}

// Summarize generates a structured markdown summary of the lecture in the target language.
func (b *BedrockService) Summarize(segments []TranscriptSegment, targetLang string) (string, error) {
	transcript := joinSegments(segments)
	system := fmt.Sprintf(`You are an educational study assistant for IT/CS students. Produce polished, exam-prep-style study notes IN %s from the lecture transcript below. Output MUST be GitHub-flavored markdown.

STRUCTURE — follow this skeleton in order:

1. A top-level "# <emoji> <Lecture Title>" heading, where the emoji reflects the topic (e.g. 📈 for stats, 🧠 for ML, 🔐 for security, 💾 for databases, 🌐 for networking).
2. **Brief Overview** — 2-3 sentences describing what the lecture covers.
3. **Key Points** — 3-5 bullets listing learning objectives.
4. A horizontal rule (---).
5. 4-8 main sections. Each section:
   - Starts with a "## <emoji> <Section Title>" header (emoji at the START, optionally a second emoji at the end).
   - May contain "### <Subsection>" subheaders.
   - Uses bulleted lists with *italicized* key terms inline.
   - Uses "> <one-sentence definition>" blockquotes to define each core concept the first time it appears.
   - Uses "> ⚠️ **Warning:** ..." blockquotes for common misconceptions or pitfalls explicitly raised in the transcript.
   - Uses GFM tables (| col | col |) for any side-by-side comparison (scenarios, options, pros/cons, examples).
   - Uses inline math in $...$ and block math in $$...$$ whenever the lecture discusses formulas (e.g. $R^2 = SSR/SST$).
   - Includes an "**Example:**" or "### Example: <name>" subsection with concrete numbers whenever the transcript provides them.
   - Separate sections with a horizontal rule (---).
6. Final section "## 🧠 Exam-Prep Checklist" containing:
   - **Quick Decision Thresholds** — bullets with concrete rules of thumb.
   - **Must-Know Formulas / Facts** — bullets.
   - **Common Pitfalls** — bullets.
   - **Quick Reference** — bullets with critical values, constants, or cheatsheet items.

LANGUAGE RULES:
- All prose, explanations, and examples in %s.
- Keep technical terms in English even when writing in %s: e.g. "machine learning", "neural network", "ReLU", "Sigmoid", "p-value", "gradient descent", "CNN", "SQL", "TCP".
- Math symbols and LaTeX stay as-is.

OUTPUT RULES:
- Only include sections, tables, warnings, and examples that the transcript actually supports. Do NOT invent facts or examples.
- Be concise but comprehensive: prioritize scannability and study usefulness.
- No preamble ("Here is the summary…"), no closing remarks. Output the markdown directly.`, targetLang, targetLang, targetLang)

	log.Printf("[Bedrock] Summarize: %d segments → %s", len(segments), targetLang)
	return b.invokeClaude(system, transcript, 4096)
}

// GenerateQuiz creates 8 multiple-choice questions in the target language.
func (b *BedrockService) GenerateQuiz(segments []TranscriptSegment, targetLang string) ([]models.QuizQuestion, error) {
	transcript := joinSegments(segments)
	system := fmt.Sprintf(`You are an educational quiz generator. Create 8 multiple-choice questions IN %s based on the lecture transcript.
Test understanding of key concepts, not trivia.

Return ONLY a JSON array, no markdown, no explanation. Format:
[{"question":"...","options":["A","B","C","D"],"correct":0,"explanation":"why this is correct"}]

Rules:
- All text (question, options, explanation) in %s
- Keep technical terms in English (e.g. "neural network", "ReLU")
- "correct" is the 0-indexed position of the right answer
- Make wrong options plausible but clearly wrong
- 4 options per question`, targetLang, targetLang)

	log.Printf("[Bedrock] GenerateQuiz: %d segments → %s", len(segments), targetLang)
	raw, err := b.invokeClaude(system, transcript, 4096)
	if err != nil {
		return nil, err
	}

	cleaned := cleanJSON(raw)
	var quiz []models.QuizQuestion
	if err := json.Unmarshal([]byte(cleaned), &quiz); err != nil {
		return nil, fmt.Errorf("parse quiz JSON: %w\nRaw: %s", err, raw)
	}
	return quiz, nil
}

// ExtractVocab pulls 12-15 important technical terms from the lecture.
func (b *BedrockService) ExtractVocab(segments []TranscriptSegment, targetLang string) ([]models.VocabEntry, error) {
	transcript := joinSegments(segments)
	system := fmt.Sprintf(`You are a vocabulary extractor for IT/CS lectures. Extract 12-15 important technical terms or concepts from the transcript.

Return ONLY a JSON array, no markdown. Format:
[{"term":"English term","translation":"translation in %s","definition":"simple 1-sentence definition in %s"}]

Rules:
- "term" is ALWAYS in English (the original technical term)
- "translation" is the term in %s (or kept in English if no good translation, like "API", "ReLU")
- "definition" is a clear, simple explanation IN %s (1-2 sentences)
- Pick terms that are educational, not common words
- Order from most to least important`, targetLang, targetLang, targetLang, targetLang)

	log.Printf("[Bedrock] ExtractVocab: %d segments → %s", len(segments), targetLang)
	raw, err := b.invokeClaude(system, transcript, 3072)
	if err != nil {
		return nil, err
	}

	cleaned := cleanJSON(raw)
	var vocab []models.VocabEntry
	if err := json.Unmarshal([]byte(cleaned), &vocab); err != nil {
		return nil, fmt.Errorf("parse vocab JSON: %w\nRaw: %s", err, raw)
	}
	return vocab, nil
}

// cleanJSON strips markdown fences and trims whitespace.
func cleanJSON(raw string) string {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	return strings.TrimSpace(cleaned)
}
