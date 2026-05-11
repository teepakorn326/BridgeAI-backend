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

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"studymind-backend/internal/models"
)

// Lesson generation — turns a lecture transcript or a translated document
// into a series of self-contained chapter cards. Each chapter has a
// citation-rich summary and a small multiple-choice quiz tied to its own
// slice of source material.
//
// Architecture: two-pass with parallel fan-out. A single mega-call would
// blow past Bedrock's 8192-token output cap on a 5+ chapter lesson and
// truncate the JSON. Instead we:
//  1. One small "outline" call to find chapter boundaries.
//  2. N parallel "content" calls, one per chapter, each producing only that
//     chapter's summary + quiz. Each call comfortably fits in 4k output.
//
// Concurrency is capped at 3 to avoid Bedrock RPM throttling.

const lessonContentMaxTokens = 4096
const lessonOutlineMaxTokens = 2048

// lessonChapterConcurrency = 3. Earlier serial setting was a stopgap before
// invokeClaudeLong got retry-on-throttle backoff (see below in this file) —
// now that 429s recover automatically with exponential backoff up to 60s
// (one full TPM bucket refill), we can run chapters in parallel for ~2-3x
// faster lesson generation. If you raise this further, also request a
// Bedrock TPM quota bump or expect more visible retry stalls.
const lessonChapterConcurrency = 3

// outlineMaxLines caps the number of segments/blocks shown to the outline
// pass. Long lectures (1500+ segments) blow past Bedrock's per-minute token
// budget when dumped whole; sampling keeps input under ~5K tokens while
// preserving enough topical flow for the model to find chapter boundaries.
const outlineMaxLines = 200

// chapterContentMaxLines caps per-chapter input. Lower than outline because
// content prompts produce more output tokens, and we want the combined
// input+output of each chapter call to stay well under any per-minute spike.
const chapterContentMaxLines = 180

// LectureLesson is one chapter of a generated lecture lesson — chapter
// title, time-slice (start/end seconds), citation-rich summary, and a small
// quiz.
type LectureLesson struct {
	StartSeconds     float64               `json:"start_seconds"`
	EndSeconds       float64               `json:"end_seconds"`
	TitleEN          string                `json:"title_en"`
	TitleTranslated  string                `json:"title_translated"`
	Summary          string                `json:"summary"`
	SummaryCitations []ChatCitation        `json:"summary_citations"`
	Quiz             []models.QuizQuestion `json:"quiz"`
}

// chapterOutline is what the outline pass returns — just the boundaries and
// titles, no body content.
type chapterOutline struct {
	StartSeg        int    `json:"start_seg"`
	TitleEN         string `json:"title_en"`
	TitleTranslated string `json:"title_translated"`
}

// chapterContentRaw is what the per-chapter content pass returns. Citation
// "seg" indices are LOCAL to that chapter (1-based within the chapter's
// own segment slice); the orchestrator maps them back to global indices.
type chapterContentRaw struct {
	Summary      string                `json:"summary"`
	SummaryCites []lessonCiteRaw       `json:"summary_cites"`
	Quiz         []models.QuizQuestion `json:"quiz"`
}

type lessonCiteRaw struct {
	N   int `json:"n"`
	Seg int `json:"seg"`
}

// identifyLessonChapters runs the small "where do chapters begin" pass.
func (b *BedrockService) identifyLessonChapters(numbered, sourceKind, targetLang string) ([]chapterOutline, error) {
	system := fmt.Sprintf(`Identify 3-5 sequential chapters in the numbered %s below. Each chapter begins at a specific 1-based segment/block number.

Return ONLY a JSON array of chapter objects — no markdown fences, no preamble. Each chapter:
- "start_seg": 1-based number where the chapter BEGINS.
- "title_en": short English title, 3-7 words, sentence case, no trailing punctuation.
- "title_translated": same title IN %s. Keep technical terms in English.

RULES:
- First chapter MUST start at 1.
- Sequential, non-overlapping.
- 3 minimum, 5 maximum.
- Each chapter MUST span enough source material to support a 300-500 word summary plus 4-5 quiz questions. Prefer FEWER, LONGER chapters over many tiny ones — a chapter covering 30 seconds of lecture is too short. Aim for at least 15-30 segments / blocks per chapter.
- Match how the material is actually structured — don't slice arbitrarily.`, sourceKind, targetLang)

	raw, err := b.invokeClaudeLong("identifyLessonChapters", system, numbered, lessonOutlineMaxTokens)
	if err != nil {
		return nil, err
	}
	var rows []chapterOutline
	if err := json.Unmarshal([]byte(cleanJSON(raw)), &rows); err != nil {
		return nil, fmt.Errorf("parse chapter outlines JSON: %w\nRaw: %s", err, raw)
	}
	return rows, nil
}

// generateChapterContent runs one per-chapter content pass. The chapter's
// own segments are passed locally numbered [1..N] so the model cites within
// the chapter; the orchestrator remaps those local indices to global ones.
func (b *BedrockService) generateChapterContent(numberedChapter, sourceKind, titleEN, titleTranslated, targetLang string) (*chapterContentRaw, error) {
	system := fmt.Sprintf(`You are an educational lesson designer. Produce a richly-detailed study unit for ONE chapter of a %s. The output must feel like a real textbook section, not a paragraph blurb.

Chapter title: "%s" / "%s"
Chapter source is the numbered %s below.

Return ONLY a single JSON object — NOT an array, NOT inside markdown fences, no preamble:
{
  "summary": "...",
  "summary_cites": [...],
  "quiz": [...]
}

Where:
- "summary": GitHub-flavored markdown, 300-600 words. The summary MUST include ALL of these sections in this order, each as its own heading or labeled block — do NOT collapse the chapter into one flowing paragraph:

    ### Overview
    A 3-5 sentence opening that frames what this chapter teaches and why it matters in context. Make it specific to this chapter, not generic.

    ### Key concepts
    A bulleted list of 4-7 items. Each bullet is a complete sentence explaining ONE idea — not just a noun phrase. Include the most important takeaways from this chapter.

    ### Definitions
    Use > blockquotes for each core term introduced in this chapter. One term = one one-sentence definition. Include at least 2 definitions when the source supports terminology; if the chapter is conceptual without named terms, use blockquotes for 2-3 short principle-statements instead. Never skip this section entirely.

    ### Worked example  ← REQUIRED if the source has any numbers, scenarios, comparisons, or step-by-step content. Reproduce the example with concrete details. If the source genuinely has no example, replace this section with **Practical context** explaining a realistic scenario where the chapter's ideas apply.

    ### Common pitfalls
    1-3 short bullets covering misconceptions, edge cases, or things students typically get wrong. If the lecturer/document doesn't explicitly flag pitfalls, INFER plausible ones from the material. Always include this section.

    Use inline math in $...$ and block math in $$...$$ wherever the source mentions formulas, ratios, or numeric relationships.
    Append [N] markers throughout — after every factual claim, every definition, every example, every formula. Aim for 8-15 distinct markers per chapter.

- "summary_cites": JSON array of {"n":1,"seg":42} entries — one per distinct [N] you used. Emit [] if you used none. "seg" is the 1-based number from the chapter's numbered %s below.

- "quiz": EXACTLY 4 to 5 multiple-choice questions IN %s about THIS chapter only — never fewer than 4. Format: [{"question":"...","options":["A","B","C","D"],"correct":0,"explanation":"..."}]. Mix difficulty deliberately:
    * 1-2 recall questions (definitions, facts).
    * 1-2 reasoning questions (compare/contrast, why-questions).
    * 1 applied question (which scenario fits, what would happen if).
  Each question has 4 options. Wrong options must be plausible but clearly wrong on close reading. "explanation" is 1-2 sentences saying WHY the right answer is right (not just restating it). "correct" is 0-indexed. Keep technical terms in English.

LANGUAGE: All prose IN %s. Technical terms in English ("ReLU", "p-value", "API", "SQL", proper nouns). Math/LaTeX as-is.

OUTPUT: ONE JSON object only. No markdown fences, no array brackets, no preamble — just the object. Be substantive — this chapter is a study unit, not a description of one.`, sourceKind, titleEN, titleTranslated, sourceKind, sourceKind, targetLang, targetLang)

	raw, err := b.invokeClaudeLong("generateChapterContent", system, numberedChapter, lessonContentMaxTokens)
	if err != nil {
		return nil, err
	}
	var content chapterContentRaw
	if err := json.Unmarshal([]byte(cleanJSON(raw)), &content); err != nil {
		return nil, fmt.Errorf("parse chapter content JSON: %w\nRaw: %s", err, raw)
	}
	return &content, nil
}

// GenerateLectureLesson identifies chapter boundaries (1 small Bedrock call)
// then generates each chapter's full content in parallel (N small calls).
// Avoids the 8192-token output cap that a single mega-call would hit on
// long lectures.
//
// For long sources, the outline pass receives a sampled subset of segments
// to keep input under Bedrock's per-minute token budget. The model returns
// chapter starts in sample-local indices; we map those back to global
// segment indices below.
func (b *BedrockService) GenerateLectureLesson(segments []TranscriptSegment, targetLang string) ([]LectureLesson, error) {
	if len(segments) == 0 {
		return nil, nil
	}

	sampledIdx := sampledIndices(len(segments), outlineMaxLines)
	var numberedAll strings.Builder
	for j, gi := range sampledIdx {
		seg := segments[gi]
		fmt.Fprintf(&numberedAll, "[%d] (%s) %s\n", j+1, formatTS(seg.StartSeconds), seg.Text)
	}

	log.Printf("[Bedrock] GenerateLectureLesson: identifying chapters (%d segments, %d sampled → %s)", len(segments), len(sampledIdx), targetLang)
	outlines, err := b.identifyLessonChapters(numberedAll.String(), "lecture transcript", targetLang)
	if err != nil {
		return nil, fmt.Errorf("identify chapters: %w", err)
	}
	if len(outlines) == 0 {
		return nil, fmt.Errorf("no chapters identified")
	}

	// Map sample-local start_seg back to global segment indices.
	for i := range outlines {
		localIdx := outlines[i].StartSeg - 1
		if localIdx < 0 || localIdx >= len(sampledIdx) {
			outlines[i].StartSeg = 1
			continue
		}
		outlines[i].StartSeg = sampledIdx[localIdx] + 1
	}
	// Force first chapter to start at segment 1.
	if len(outlines) > 0 {
		outlines[0].StartSeg = 1
	}

	log.Printf("[Bedrock] GenerateLectureLesson: %d chapters → generating content in parallel", len(outlines))

	type result struct {
		idx    int
		lesson LectureLesson
		err    error
	}
	results := make([]result, len(outlines))
	var wg sync.WaitGroup
	sem := make(chan struct{}, lessonChapterConcurrency)

	for i := range outlines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			outline := outlines[i]
			startIdx, endIdx := chapterRange(outlines, i, len(segments))
			if startIdx >= endIdx {
				results[i] = result{i, LectureLesson{}, fmt.Errorf("empty chapter range")}
				return
			}
			chapterSegs := segments[startIdx:endIdx]

			// Sample within the chapter when it's still large after splitting,
			// so per-chapter content prompts also stay under Bedrock's TPM.
			localSampleIdx := sampledIndices(len(chapterSegs), chapterContentMaxLines)
			var local strings.Builder
			for j, ci := range localSampleIdx {
				seg := chapterSegs[ci]
				fmt.Fprintf(&local, "[%d] (%s) %s\n", j+1, formatTS(seg.StartSeconds), seg.Text)
			}

			content, err := b.generateChapterContent(
				local.String(), "lecture transcript",
				outline.TitleEN, outline.TitleTranslated, targetLang,
			)
			if err != nil {
				results[i] = result{i, LectureLesson{}, err}
				return
			}

			cites := make([]ChatCitation, 0, len(content.SummaryCites))
			for _, c := range content.SummaryCites {
				sampleLocal := c.Seg - 1
				if sampleLocal < 0 || sampleLocal >= len(localSampleIdx) {
					continue
				}
				s := chapterSegs[localSampleIdx[sampleLocal]]
				cites = append(cites, ChatCitation{
					N:            c.N,
					StartSeconds: s.StartSeconds,
					EndSeconds:   s.EndSeconds,
					TextEN:       s.Text,
				})
			}

			results[i] = result{i, LectureLesson{
				StartSeconds:     segments[startIdx].StartSeconds,
				EndSeconds:       segments[endIdx-1].EndSeconds,
				TitleEN:          outline.TitleEN,
				TitleTranslated:  outline.TitleTranslated,
				Summary:          content.Summary,
				SummaryCitations: cites,
				Quiz:             content.Quiz,
			}, nil}
		}(i)
	}
	wg.Wait()

	lessons := make([]LectureLesson, 0, len(outlines))
	for _, r := range results {
		if r.err != nil {
			log.Printf("[Bedrock] LectureLesson chapter %d failed (skipping): %v", r.idx+1, r.err)
			continue
		}
		lessons = append(lessons, r.lesson)
	}
	if len(lessons) == 0 {
		return nil, fmt.Errorf("all chapter content generations failed")
	}
	return lessons, nil
}

// GenerateDocumentLesson is the document mirror of GenerateLectureLesson.
// Same sample-then-map approach for the outline pass on long documents.
func (b *BedrockService) GenerateDocumentLesson(blocks []models.DocumentBlock, targetLang string) ([]models.DocumentLesson, error) {
	if len(blocks) == 0 {
		return nil, nil
	}

	sampledIdx := sampledIndices(len(blocks), outlineMaxLines)
	var numberedAll strings.Builder
	for j, gi := range sampledIdx {
		blk := blocks[gi]
		if blk.Page > 0 {
			fmt.Fprintf(&numberedAll, "[%d] (p.%d) %s\n", j+1, blk.Page, blk.TextEN)
		} else {
			fmt.Fprintf(&numberedAll, "[%d] %s\n", j+1, blk.TextEN)
		}
	}

	log.Printf("[Bedrock] GenerateDocumentLesson: identifying chapters (%d blocks, %d sampled → %s)", len(blocks), len(sampledIdx), targetLang)
	outlines, err := b.identifyLessonChapters(numberedAll.String(), "document", targetLang)
	if err != nil {
		return nil, fmt.Errorf("identify chapters: %w", err)
	}
	if len(outlines) == 0 {
		return nil, fmt.Errorf("no chapters identified")
	}

	// Map sample-local start_seg back to global block indices.
	for i := range outlines {
		localIdx := outlines[i].StartSeg - 1
		if localIdx < 0 || localIdx >= len(sampledIdx) {
			outlines[i].StartSeg = 1
			continue
		}
		outlines[i].StartSeg = sampledIdx[localIdx] + 1
	}
	if len(outlines) > 0 {
		outlines[0].StartSeg = 1
	}

	log.Printf("[Bedrock] GenerateDocumentLesson: %d chapters → generating content in parallel", len(outlines))

	type result struct {
		idx    int
		lesson models.DocumentLesson
		err    error
	}
	results := make([]result, len(outlines))
	var wg sync.WaitGroup
	sem := make(chan struct{}, lessonChapterConcurrency)

	for i := range outlines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			outline := outlines[i]
			startIdx, endIdx := chapterRange(outlines, i, len(blocks))
			if startIdx >= endIdx {
				results[i] = result{i, models.DocumentLesson{}, fmt.Errorf("empty chapter range")}
				return
			}
			chapterBlocks := blocks[startIdx:endIdx]

			localSampleIdx := sampledIndices(len(chapterBlocks), chapterContentMaxLines)
			var local strings.Builder
			for j, ci := range localSampleIdx {
				blk := chapterBlocks[ci]
				if blk.Page > 0 {
					fmt.Fprintf(&local, "[%d] (p.%d) %s\n", j+1, blk.Page, blk.TextEN)
				} else {
					fmt.Fprintf(&local, "[%d] %s\n", j+1, blk.TextEN)
				}
			}

			content, err := b.generateChapterContent(
				local.String(), "document",
				outline.TitleEN, outline.TitleTranslated, targetLang,
			)
			if err != nil {
				results[i] = result{i, models.DocumentLesson{}, err}
				return
			}

			cites := make([]models.DocumentChatCitation, 0, len(content.SummaryCites))
			for _, c := range content.SummaryCites {
				sampleLocal := c.Seg - 1
				if sampleLocal < 0 || sampleLocal >= len(localSampleIdx) {
					continue
				}
				blk := chapterBlocks[localSampleIdx[sampleLocal]]
				cites = append(cites, models.DocumentChatCitation{
					N:      c.N,
					Page:   blk.Page,
					Order:  blk.Order,
					TextEN: blk.TextEN,
				})
			}

			results[i] = result{i, models.DocumentLesson{
				TitleEN:          outline.TitleEN,
				TitleTranslated:  outline.TitleTranslated,
				Summary:          content.Summary,
				SummaryCitations: cites,
				Quiz:             content.Quiz,
			}, nil}
		}(i)
	}
	wg.Wait()

	lessons := make([]models.DocumentLesson, 0, len(outlines))
	for _, r := range results {
		if r.err != nil {
			log.Printf("[Bedrock] DocumentLesson chapter %d failed (skipping): %v", r.idx+1, r.err)
			continue
		}
		lessons = append(lessons, r.lesson)
	}
	if len(lessons) == 0 {
		return nil, fmt.Errorf("all chapter content generations failed")
	}
	return lessons, nil
}

// chapterRange resolves the [start, end) slice indices for chapter `i` given
// the outline list and total source length. Clamps degenerate values.
func chapterRange(outlines []chapterOutline, i, total int) (int, int) {
	startIdx := outlines[i].StartSeg - 1
	if startIdx < 0 {
		startIdx = 0
	}
	endIdx := total
	if i+1 < len(outlines) {
		next := outlines[i+1].StartSeg - 1
		if next > startIdx && next <= total {
			endIdx = next
		}
	}
	if startIdx >= total {
		return total, total
	}
	return startIdx, endIdx
}

// invokeClaudeLong calls Bedrock with retry-on-throttle. The lesson pipeline
// fans out parallel chapter calls, so transient 429 ThrottlingException
// (per-minute token bucket exhausted) is the most common failure mode.
// Retries with exponential backoff so one chapter doesn't fail the whole
// lesson when the bucket briefly empties.
func (b *BedrockService) invokeClaudeLong(label, system, user string, maxTokens int) (string, error) {
	reqBody := claudeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        maxTokens,
		System:           system,
		Messages:         []claudeMessage{{Role: "user", Content: user}},
	}
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal %s request: %w", label, err)
	}

	const maxAttempts = 8
	const maxBackoff = 60 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := b.client.InvokeModel(context.TODO(), &bedrockruntime.InvokeModelInput{
			ModelId:     stringPtr(bedrockModel),
			ContentType: stringPtr("application/json"),
			Body:        reqBytes,
		})
		if err != nil {
			lastErr = err
			if isThrottlingError(err) && attempt < maxAttempts {
				// Exponential backoff with jitter, capped at 60s. Bedrock's
				// per-minute TPM bucket needs ~60s to fully refill, so we
				// want backoff to plateau there rather than blow past.
				base := time.Duration(1<<(attempt-1)) * 2 * time.Second
				if base > maxBackoff {
					base = maxBackoff
				}
				jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
				log.Printf("[Bedrock] %s attempt %d throttled, retrying in %v", label, attempt, base+jitter)
				time.Sleep(base + jitter)
				continue
			}
			return "", fmt.Errorf("bedrock %s error: %w", label, err)
		}
		var resp claudeResponse
		if err := json.Unmarshal(result.Body, &resp); err != nil {
			return "", fmt.Errorf("unmarshal %s response: %w", label, err)
		}
		if len(resp.Content) == 0 {
			return "", fmt.Errorf("bedrock %s returned empty content", label)
		}
		return resp.Content[0].Text, nil
	}
	return "", fmt.Errorf("bedrock %s failed after %d attempts: %w", label, maxAttempts, lastErr)
}

// isThrottlingError returns true for Bedrock 429 / ThrottlingException
// responses, which we want to retry rather than surface as a hard failure.
func isThrottlingError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "ThrottlingException") ||
		strings.Contains(msg, "StatusCode: 429") ||
		strings.Contains(msg, "Too many requests") ||
		strings.Contains(msg, "Too many tokens")
}

// sampledIndices returns evenly-spaced indices from [0..total) such that
// the result has at most maxLines entries, always including 0 and total-1.
// Used to condense long transcripts/documents for the outline pass without
// exceeding Bedrock's per-minute token budget.
func sampledIndices(total, maxLines int) []int {
	if total <= 0 {
		return nil
	}
	if total <= maxLines {
		out := make([]int, total)
		for i := range out {
			out[i] = i
		}
		return out
	}
	step := float64(total-1) / float64(maxLines-1)
	out := make([]int, 0, maxLines)
	for i := 0; i < maxLines; i++ {
		idx := int(float64(i)*step + 0.5)
		if idx >= total {
			idx = total - 1
		}
		if len(out) == 0 || out[len(out)-1] != idx {
			out = append(out, idx)
		}
	}
	if out[len(out)-1] != total-1 {
		out = append(out, total-1)
	}
	return out
}
