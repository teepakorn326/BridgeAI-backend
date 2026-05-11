package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"studymind-backend/internal/models"
)

// Document-pipeline Bedrock methods. Lectures are time-based (transcripts +
// timestamps); documents are page-and-order based (no playback). The prompts
// here treat the input as written material — slides, textbook, reading —
// and emit citations as page numbers, not timestamps.

// ExtractDocumentPDF sends a PDF to Claude (vision + document) and returns
// page-aware blocks. Each block is one paragraph, slide bullet, or list
// item, tagged with its source page so the frontend can build a real reader
// view (page nav, "jump to page" in chat citations).
func (b *BedrockService) ExtractDocumentPDF(pdfBytes []byte) ([]models.DocumentBlock, error) {
	encoded := base64.StdEncoding.EncodeToString(pdfBytes)

	system := `You are extracting text from an academic PDF (lecture slides, textbook chapter, or course reading) for a study assistant.

TASK: Read the PDF and output its main content as a JSON array of paragraph-sized blocks, each tagged with its source page number.

OUTPUT: a JSON array of objects, in reading order. Each object has:
- "page": the 1-based page number where this text appears
- "text": one paragraph, slide bullet, or list-item of source text

RULES:
1. Skip page numbers, running headers/footers, copyright notices, and decorative text.
2. For tables, output one entry per row with columns joined by " | ".
3. For figures, include any caption text but do NOT describe images.
4. For equations, render in plain LaTeX-like form (e.g. "R^2 = SSR/SST").
5. Preserve the original language — do NOT translate.
6. If a paragraph is longer than ~60 words, split it into ~30-50 word chunks at sentence boundaries (each split keeps the same page number).
7. Return ONLY the raw JSON array. No markdown fences, no preamble, no commentary.`

	reqBody := claudeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        16384,
		System:           system,
		Messages: []claudeMessage{
			{
				Role: "user",
				Content: []contentBlock{
					{
						Type: "document",
						Source: &docSource{
							Type:      "base64",
							MediaType: "application/pdf",
							Data:      encoded,
						},
					},
					{
						Type: "text",
						Text: "Extract the text from this PDF as a page-tagged JSON array.",
					},
				},
			},
		},
	}

	rawText, err := b.invokeForDocument("ExtractDocumentPDF", reqBody, len(pdfBytes))
	if err != nil {
		return nil, err
	}

	var rows []struct {
		Page int    `json:"page"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(cleanJSON(rawText)), &rows); err != nil {
		return nil, fmt.Errorf("parse document-pdf JSON: %w\nRaw: %s", err, rawText)
	}

	blocks := make([]models.DocumentBlock, 0, len(rows))
	for _, r := range rows {
		t := strings.TrimSpace(r.Text)
		if t == "" {
			continue
		}
		page := r.Page
		if page < 1 {
			page = 1
		}
		blocks = append(blocks, models.DocumentBlock{
			Page:   page,
			Order:  len(blocks),
			TextEN: t,
		})
	}
	log.Printf("[Bedrock] ExtractDocumentPDF: %d blocks", len(blocks))
	return blocks, nil
}

// ExtractDocumentImage sends a single image (slide screenshot, photo of
// notes, etc.) through Claude vision and returns text blocks. Single page,
// blocks reflect distinct text regions / paragraphs in reading order.
func (b *BedrockService) ExtractDocumentImage(imgBytes []byte, mediaType string) ([]models.DocumentBlock, error) {
	encoded := base64.StdEncoding.EncodeToString(imgBytes)

	system := `You are extracting readable text from an image (slide screenshot, photo of handwritten notes, scanned page, or whiteboard) for a study assistant.

TASK: Read the image and output its text as a JSON array of strings, in reading order.

OUTPUT: a JSON array of strings. Each string is one paragraph, bullet, list-item, or labeled equation.

RULES:
1. Skip decorative text, watermarks, page numbers.
2. For tables, output one entry per row with columns joined by " | ".
3. For equations, render in plain LaTeX-like form (e.g. "R^2 = SSR/SST").
4. If the image has no readable text, return [].
5. Preserve the original language — do NOT translate.
6. Return ONLY the raw JSON array. No markdown fences, no preamble.`

	reqBody := claudeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        4096,
		System:           system,
		Messages: []claudeMessage{
			{
				Role: "user",
				Content: []contentBlock{
					{
						Type: "image",
						Source: &docSource{
							Type:      "base64",
							MediaType: mediaType,
							Data:      encoded,
						},
					},
					{
						Type: "text",
						Text: "Extract readable text from this image as a JSON array of strings.",
					},
				},
			},
		},
	}

	rawText, err := b.invokeForDocument("ExtractDocumentImage", reqBody, len(imgBytes))
	if err != nil {
		return nil, err
	}

	var lines []string
	if err := json.Unmarshal([]byte(cleanJSON(rawText)), &lines); err != nil {
		return nil, fmt.Errorf("parse document-image JSON: %w\nRaw: %s", err, rawText)
	}

	blocks := make([]models.DocumentBlock, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		blocks = append(blocks, models.DocumentBlock{
			Page:   1,
			Order:  len(blocks),
			TextEN: t,
		})
	}
	log.Printf("[Bedrock] ExtractDocumentImage: %d blocks", len(blocks))
	return blocks, nil
}

// invokeForDocument runs a one-shot Bedrock call and returns the raw text
// content. Shared by ExtractDocumentPDF and ExtractDocumentImage so the
// boilerplate stays in one place.
func (b *BedrockService) invokeForDocument(label string, reqBody claudeRequest, sourceBytes int) (string, error) {
	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal %s request: %w", label, err)
	}
	log.Printf("[Bedrock] %s: %d source bytes", label, sourceBytes)
	result, err := b.client.InvokeModel(context.TODO(), &bedrockruntime.InvokeModelInput{
		ModelId:     stringPtr(bedrockModel),
		ContentType: stringPtr("application/json"),
		Body:        reqBytes,
	})
	if err != nil {
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

// TranslateBlocks translates document blocks in-place by reusing
// TranslateSegments under the hood. Reuses the same chunking, concurrency,
// prompt caching, and retry logic as the lecture pipeline.
func (b *BedrockService) TranslateBlocks(blocks []models.DocumentBlock, targetLang string) ([]models.DocumentBlock, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	segs := make([]TranscriptSegment, len(blocks))
	for i, blk := range blocks {
		segs[i] = TranscriptSegment{Text: blk.TextEN}
	}
	subs, err := b.TranslateSegments(segs, targetLang)
	if err != nil {
		return nil, err
	}
	if len(subs) != len(blocks) {
		return nil, fmt.Errorf("translation count mismatch: got %d, want %d", len(subs), len(blocks))
	}
	out := make([]models.DocumentBlock, len(blocks))
	for i, blk := range blocks {
		out[i] = models.DocumentBlock{
			Page:           blk.Page,
			Order:          blk.Order,
			TextEN:         blk.TextEN,
			TextTranslated: subs[i].TextTranslated,
		}
	}
	return out, nil
}

// joinBlocks concatenates document text for prompt context. Page tags help
// the model produce page-cited answers.
func joinBlocksWithPages(blocks []models.DocumentBlock) string {
	var sb strings.Builder
	for i, blk := range blocks {
		if blk.Page > 0 {
			fmt.Fprintf(&sb, "[%d] (p.%d) %s\n", i+1, blk.Page, blk.TextEN)
		} else {
			fmt.Fprintf(&sb, "[%d] %s\n", i+1, blk.TextEN)
		}
	}
	return sb.String()
}

func joinBlocksPlain(blocks []models.DocumentBlock) string {
	var sb strings.Builder
	for _, blk := range blocks {
		sb.WriteString(blk.TextEN)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// SummarizeDocument produces document-tuned study notes with inline citations
// pointing back to source blocks. Output is markdown that may contain [N]
// markers; the returned citations array resolves each N to a (page, order,
// text) tuple so the frontend can render "jump to source" pills.
func (b *BedrockService) SummarizeDocument(blocks []models.DocumentBlock, targetLang string) (string, []models.DocumentChatCitation, error) {
	body := joinBlocksWithPages(blocks)
	system := fmt.Sprintf(`You are an educational study assistant. Produce polished, exam-prep-style study notes IN %s from the numbered document blocks below (slides, textbook chapter, paper, or reading). Output MUST be GitHub-flavored markdown.

STRUCTURE:
1. "# <Document Title or Topic>" heading.
2. **Overview** — 2-3 sentences on what the document covers.
3. **Key Concepts** — 3-5 bullets listing the most important takeaways.
4. A horizontal rule (---).
5. 3-7 main sections that mirror the document's actual structure. Each section:
   - Starts with "## <Section Title>".
   - Uses bulleted lists with *italicized* key terms inline.
   - Uses "> <one-sentence definition>" blockquotes for the first appearance of each core term.
   - Uses GFM tables for any side-by-side comparison the document presents.
   - Uses inline math in $...$ and block math in $$...$$ when the document has formulas.
   - Adds an "**Example:**" block whenever the document gives concrete numbers or worked examples.
   - Separated by horizontal rules (---).
6. Final section "## Exam-Prep Checklist":
   - **Must-Know Concepts** — bullets.
   - **Key Formulas / Facts** — bullets if applicable.
   - **Common Pitfalls** — bullets if the document raises any.

LANGUAGE RULES:
- All prose IN %s.
- Keep technical terms in English even when writing %s ("machine learning", "ReLU", "p-value", "SQL", etc.).
- Math/LaTeX stay as-is.

CITATIONS — STRICT:
1. The FIRST line of your output MUST be a single-line JSON block of the form:
   <CITES>[{"n":1,"seg":42},{"n":2,"seg":57}]</CITES>
   - Emit this line EVEN IF you don't plan citations: <CITES>[]</CITES>
   - One entry per distinct [N] marker you will use.
   - "n" MUST equal the integer you put inside brackets in the markdown (e.g. [1] -> {"n":1,...}).
   - "seg" is the 1-based block number from the numbered document below (the leading [N] on each line) that supports that claim.
2. After the <CITES> line, output the markdown summary.
3. Inside the markdown, append [N] markers after factual claims, definitions, examples, and formulas — anywhere a reader might want to jump back to the source. Reuse the same N across sentences where appropriate. Aim for 8-20 citations total; don't over-cite trivial words.

OUTPUT RULES:
- Only include sections, tables, examples, and formulas the document actually supports. Do NOT invent content.
- No preamble, no closing remarks. Just <CITES> followed by the markdown.`, targetLang, targetLang, targetLang)

	log.Printf("[Bedrock] SummarizeDocument: %d blocks → %s", len(blocks), targetLang)
	raw, err := b.invokeClaude(system, body, 4096)
	if err != nil {
		return "", nil, err
	}
	summary, citations := parseDocumentChatCitations(raw, blocks)
	return summary, citations, nil
}

// GenerateDocumentQuiz creates 8 multiple-choice questions tuned for written
// material (vs. spoken lectures).
func (b *BedrockService) GenerateDocumentQuiz(blocks []models.DocumentBlock, targetLang string) ([]models.QuizQuestion, error) {
	body := joinBlocksPlain(blocks)
	system := fmt.Sprintf(`You are an educational quiz generator. Create 8 multiple-choice questions IN %s based on the document below (slides, textbook chapter, paper, or reading).
Test understanding of key concepts and the document's actual content, not surface trivia.

Return ONLY a JSON array, no markdown, no explanation. Format:
[{"question":"...","options":["A","B","C","D"],"correct":0,"explanation":"why this is correct"}]

Rules:
- All text (question, options, explanation) IN %s.
- Keep technical terms in English.
- "correct" is the 0-indexed position of the right answer.
- Make wrong options plausible but clearly wrong.
- 4 options per question.
- Prefer questions that test reasoning or application over rote recall when the document supports it.`, targetLang, targetLang)

	log.Printf("[Bedrock] GenerateDocumentQuiz: %d blocks → %s", len(blocks), targetLang)
	raw, err := b.invokeClaude(system, body, 4096)
	if err != nil {
		return nil, err
	}
	var quiz []models.QuizQuestion
	if err := json.Unmarshal([]byte(cleanJSON(raw)), &quiz); err != nil {
		return nil, fmt.Errorf("parse document-quiz JSON: %w\nRaw: %s", err, raw)
	}
	return quiz, nil
}

// ExtractDocumentVocab pulls 12-15 important technical terms from the
// document, with translations and definitions.
func (b *BedrockService) ExtractDocumentVocab(blocks []models.DocumentBlock, targetLang string) ([]models.VocabEntry, error) {
	body := joinBlocksPlain(blocks)
	system := fmt.Sprintf(`You are a vocabulary extractor for academic documents (slides, textbook chapter, paper). Extract 12-15 important technical terms or concepts from the document.

Return ONLY a JSON array, no markdown. Format:
[{"term":"English term","translation":"translation in %s","definition":"simple 1-sentence definition in %s"}]

Rules:
- "term" is ALWAYS in English (the original technical term).
- "translation" is the term in %s, or kept in English if no good translation exists ("API", "ReLU").
- "definition" is a clear, simple explanation IN %s, drawn from how the document uses the term.
- Pick terms that are educational and central to the document, not common words.
- Order from most to least important.`, targetLang, targetLang, targetLang, targetLang)

	log.Printf("[Bedrock] ExtractDocumentVocab: %d blocks → %s", len(blocks), targetLang)
	raw, err := b.invokeClaude(system, body, 3072)
	if err != nil {
		return nil, err
	}
	var vocab []models.VocabEntry
	if err := json.Unmarshal([]byte(cleanJSON(raw)), &vocab); err != nil {
		return nil, fmt.Errorf("parse document-vocab JSON: %w\nRaw: %s", err, raw)
	}
	return vocab, nil
}

// ChatWithDocument answers a question about the document with citations
// pointing to specific blocks. Citations carry the source page number so
// the frontend can render "jump to page" buttons.
func (b *BedrockService) ChatWithDocument(blocks []models.DocumentBlock, targetLang string, history []ChatMessage) (string, []models.DocumentChatCitation, error) {
	numbered := joinBlocksWithPages(blocks)

	system := fmt.Sprintf(`You are a helpful study assistant for a university student reading a document (slides, textbook chapter, paper, or course reading).
Answer their question IN %s, grounded strictly in the numbered document below.
Keep English technical terms (API, GPU, ReLU, CNN, SQL, etc.) in English.
If the document does not contain the answer, say so honestly IN %s — do not invent facts.
Be concise and explain at undergraduate level. Use plain conversational prose; avoid heavy markdown (no ## headings, no --- separators). Short paragraphs and simple **bold** or bullet lists are fine.

OUTPUT FORMAT — STRICT:
1. The FIRST line of your response MUST be a single-line JSON block of the form:
   <CITES>[{"n":1,"seg":42},{"n":2,"seg":57}]</CITES>
   - Emit this line EVEN IF you don't plan citations: <CITES>[]</CITES>
   - One entry per distinct [N] marker you will use in the answer.
   - "n" MUST equal the exact integer you put inside brackets in the answer (e.g. [1] -> {"n":1,...}).
   - "seg" is the 1-based block number from the numbered document below (the leading [N] on each line) that supports that claim.
2. After the <CITES> line, write your answer in %s.
3. Inside the answer, append the matching [N] marker after each factual claim or paraphrase. Use small numbers starting from [1].

Putting <CITES> first guarantees citations survive even if your answer is long.

NUMBERED DOCUMENT:
%s`, targetLang, targetLang, targetLang, numbered)

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
		return "", nil, fmt.Errorf("marshal document-chat request: %w", err)
	}
	result, err := b.client.InvokeModel(context.TODO(), &bedrockruntime.InvokeModelInput{
		ModelId:     stringPtr(bedrockModel),
		ContentType: stringPtr("application/json"),
		Body:        reqBytes,
	})
	if err != nil {
		return "", nil, fmt.Errorf("bedrock document-chat error: %w", err)
	}
	var resp claudeResponse
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		return "", nil, fmt.Errorf("unmarshal document-chat response: %w", err)
	}
	if len(resp.Content) == 0 {
		return "", nil, fmt.Errorf("bedrock returned empty document-chat content")
	}

	answer, citations := parseDocumentChatCitations(resp.Content[0].Text, blocks)
	return answer, citations, nil
}

// parseDocumentChatCitations mirrors parseChatCitations but resolves seg
// indices to (page, order) pairs so the frontend can do page-jump UX.
func parseDocumentChatCitations(raw string, blocks []models.DocumentBlock) (string, []models.DocumentChatCitation) {
	openIdx := strings.Index(raw, "<CITES>")
	if openIdx < 0 {
		return strings.TrimSpace(raw), nil
	}
	before := raw[:openIdx]
	closeIdx := strings.Index(raw, "</CITES>")
	if closeIdx < 0 || closeIdx < openIdx {
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

	out := make([]models.DocumentChatCitation, 0, len(rows))
	for _, r := range rows {
		idx := r.Seg - 1
		if idx < 0 || idx >= len(blocks) {
			continue
		}
		blk := blocks[idx]
		out = append(out, models.DocumentChatCitation{
			N:      r.N,
			Page:   blk.Page,
			Order:  blk.Order,
			TextEN: blk.TextEN,
		})
	}
	return answer, out
}
