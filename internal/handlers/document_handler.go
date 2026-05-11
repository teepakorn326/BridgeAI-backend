package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v2"

	"studymind-backend/internal/database"
	"studymind-backend/internal/models"
	"studymind-backend/internal/services"
)

// DocumentHandler owns all document-pipeline endpoints. Lives alongside
// CourseHandler but does not share state — the two pipelines are deliberately
// independent so document UX (page navigation, page-cited chat) doesn't have
// to fit the lecture data model.
type DocumentHandler struct {
	cache    *database.CacheService
	bedrock  *services.BedrockService
	extract  *services.DocumentExtractor
}

func NewDocumentHandler(cache *database.CacheService, bedrock *services.BedrockService) *DocumentHandler {
	return &DocumentHandler{
		cache:   cache,
		bedrock: bedrock,
		extract: services.NewDocumentExtractor(bedrock),
	}
}

// UploadDocument handles POST /api/upload-document — accepts a multipart
// file (PDF/DOCX/PPTX/TXT/MD/PNG/JPG/WEBP/GIF) plus target_lang, dispatches
// to the right extractor, translates, caches, and kicks off study-material
// generation.
func (h *DocumentHandler) UploadDocument(c *fiber.Ctx) error {
	targetLang := c.FormValue("target_lang")
	if targetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "target_lang required"})
	}
	titleOverride := strings.TrimSpace(c.FormValue("title"))

	fh, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "file required"})
	}
	src, err := fh.Open()
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "could not open uploaded file"})
	}
	defer src.Close()

	raw, err := io.ReadAll(src)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "could not read uploaded file"})
	}

	blocks, source, err := h.extract.Extract(fh.Filename, raw)
	if err != nil {
		var unsupp *services.ErrUnsupportedDocument
		if errors.As(err, &unsupp) {
			return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{"error": unsupp.Error()})
		}
		log.Printf("[Document] extract failed for %s: %v", fh.Filename, err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "could not extract text: " + err.Error()})
	}
	if len(blocks) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "document contained no extractable text"})
	}

	hash := documentHash(raw)
	documentID := source + "-" + hash[:16]
	sourceURL := "doc://" + documentID

	title := titleOverride
	if title == "" {
		title = strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename))
	}
	if title == "" {
		title = "Untitled document"
	}

	// Cache hit? Return immediately and link the user to it.
	if cached, _ := h.cache.GetCachedDocument(documentID, targetLang); cached != nil {
		log.Printf("[Document] cache HIT for %s/%s — returning existing", documentID, targetLang)
		if uid, _ := c.Locals("userID").(string); uid != "" {
			if err := h.cache.LinkUserToDocument(uid, cached); err != nil {
				log.Printf("[Document] LinkUserToDocument error (non-fatal): %v", err)
			}
		}
		return c.JSON(cached)
	}

	translated, err := h.bedrock.TranslateBlocks(blocks, targetLang)
	if err != nil {
		log.Printf("[Document] translation failed: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "translation failed"})
	}

	doc := &models.Document{
		DocumentID: documentID,
		Source:     source,
		SourceURL:  sourceURL,
		Title:      title,
		TargetLang: targetLang,
		Blocks:     translated,
		FromCache:  false,
	}

	if err := h.cache.SaveDocumentToCache(doc); err != nil {
		log.Printf("[Document] cache save error: %v", err)
	}
	if uid, _ := c.Locals("userID").(string); uid != "" {
		if err := h.cache.LinkUserToDocument(uid, doc); err != nil {
			log.Printf("[Document] LinkUserToDocument error (non-fatal): %v", err)
		}
	}

	go h.generateDocumentStudyMaterials(documentID, targetLang, translated)

	log.Printf("[Document] ingest complete: id=%s blocks=%d source=%s", documentID, len(translated), source)
	return c.JSON(doc)
}

// GetDocument handles GET /api/document?id=...&lang=...
func (h *DocumentHandler) GetDocument(c *fiber.Ctx) error {
	documentID := c.Query("id")
	targetLang := c.Query("lang")
	if documentID == "" || targetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "id and lang required"})
	}
	cached, err := h.cache.GetCachedDocument(documentID, targetLang)
	if err != nil {
		log.Printf("[Document] GetDocument cache error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "cache error"})
	}
	if cached == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "document not found — upload it first"})
	}

	go h.ensureDocumentStudyMaterials(documentID, targetLang, cached.Blocks)

	return c.JSON(cached)
}

// ListDocuments handles GET /api/documents — returns all documents the
// authenticated user has uploaded.
func (h *DocumentHandler) ListDocuments(c *fiber.Ctx) error {
	userID, _ := c.Locals("userID").(string)
	if userID == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Not authenticated"})
	}
	summaries, err := h.cache.ListUserDocuments(userID)
	if err != nil {
		log.Printf("[Document] ListDocuments error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Failed to fetch documents"})
	}
	return c.JSON(fiber.Map{"documents": summaries})
}

// documentSummaryEnvelope wraps a markdown summary with its source-block
// citations so they round-trip through the cache as a single string. Older
// cached entries are plain markdown without citations — decodeDocumentSummary
// handles both shapes.
type documentSummaryEnvelope struct {
	Summary   string                         `json:"summary"`
	Citations []models.DocumentChatCitation  `json:"citations,omitempty"`
}

func decodeDocumentSummary(cached string) documentSummaryEnvelope {
	var env documentSummaryEnvelope
	if json.Unmarshal([]byte(cached), &env) == nil && env.Summary != "" {
		return env
	}
	return documentSummaryEnvelope{Summary: cached}
}

// SummarizeDocument handles POST /api/document/summary.
func (h *DocumentHandler) SummarizeDocument(c *fiber.Ctx) error {
	doc, status, lerr := h.loadDocumentForStudy(c)
	if lerr != nil {
		return c.Status(status).JSON(fiber.Map{"error": lerr.Error()})
	}
	if cached, _ := h.cache.GetDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "summary"); cached != "" {
		env := decodeDocumentSummary(cached)
		return c.JSON(fiber.Map{"summary": env.Summary, "citations": env.Citations, "from_cache": true})
	}
	summary, citations, err := h.bedrock.SummarizeDocument(doc.Blocks, doc.TargetLang)
	if err != nil {
		log.Printf("[Document] Summarize error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Summary generation failed"})
	}
	if data, jerr := json.Marshal(documentSummaryEnvelope{Summary: summary, Citations: citations}); jerr == nil {
		go h.cache.SaveDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "summary", string(data))
	}
	return c.JSON(fiber.Map{"summary": summary, "citations": citations, "from_cache": false})
}

// GenerateDocumentQuiz handles POST /api/document/quiz.
func (h *DocumentHandler) GenerateDocumentQuiz(c *fiber.Ctx) error {
	doc, status, lerr := h.loadDocumentForStudy(c)
	if lerr != nil {
		return c.Status(status).JSON(fiber.Map{"error": lerr.Error()})
	}
	if cached, _ := h.cache.GetDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "quiz"); cached != "" {
		var quiz []models.QuizQuestion
		if jerr := json.Unmarshal([]byte(cached), &quiz); jerr == nil {
			return c.JSON(fiber.Map{"quiz": quiz, "from_cache": true})
		}
	}
	quiz, err := h.bedrock.GenerateDocumentQuiz(doc.Blocks, doc.TargetLang)
	if err != nil {
		log.Printf("[Document] Quiz error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Quiz generation failed"})
	}
	if data, jerr := json.Marshal(quiz); jerr == nil {
		go h.cache.SaveDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "quiz", string(data))
	}
	return c.JSON(fiber.Map{"quiz": quiz, "from_cache": false})
}

// ExtractDocumentVocab handles POST /api/document/vocab.
func (h *DocumentHandler) ExtractDocumentVocab(c *fiber.Ctx) error {
	doc, status, lerr := h.loadDocumentForStudy(c)
	if lerr != nil {
		return c.Status(status).JSON(fiber.Map{"error": lerr.Error()})
	}
	if cached, _ := h.cache.GetDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "vocab"); cached != "" {
		var vocab []models.VocabEntry
		if jerr := json.Unmarshal([]byte(cached), &vocab); jerr == nil {
			return c.JSON(fiber.Map{"vocab": vocab, "from_cache": true})
		}
	}
	vocab, err := h.bedrock.ExtractDocumentVocab(doc.Blocks, doc.TargetLang)
	if err != nil {
		log.Printf("[Document] Vocab error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Vocab generation failed"})
	}
	if data, jerr := json.Marshal(vocab); jerr == nil {
		go h.cache.SaveDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "vocab", string(data))
	}
	return c.JSON(fiber.Map{"vocab": vocab, "from_cache": false})
}

// GenerateDocumentLesson handles POST /api/document/lesson — returns
// 3-7 chapter cards with per-chapter summary + mini-quiz + citations.
func (h *DocumentHandler) GenerateDocumentLesson(c *fiber.Ctx) error {
	doc, status, lerr := h.loadDocumentForStudy(c)
	if lerr != nil {
		return c.Status(status).JSON(fiber.Map{"error": lerr.Error()})
	}
	if cached, _ := h.cache.GetDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "lesson"); cached != "" {
		var lessons []models.DocumentLesson
		if err := json.Unmarshal([]byte(cached), &lessons); err == nil {
			return c.JSON(fiber.Map{"lessons": lessons, "from_cache": true})
		}
	}
	lessons, err := h.bedrock.GenerateDocumentLesson(doc.Blocks, doc.TargetLang)
	if err != nil {
		log.Printf("[Document] Lesson error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Lesson generation failed: " + err.Error(),
		})
	}
	if data, jerr := json.Marshal(lessons); jerr == nil {
		go h.cache.SaveDocumentStudyMaterial(doc.DocumentID, doc.TargetLang, "lesson", string(data))
	}
	return c.JSON(fiber.Map{"lessons": lessons, "from_cache": false})
}

// ChatWithDocument handles POST /api/document/chat.
func (h *DocumentHandler) ChatWithDocument(c *fiber.Ctx) error {
	var req models.DocumentChatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.DocumentID == "" || req.TargetLang == "" || len(req.Messages) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "document_id, target_lang, and messages required"})
	}
	cached, err := h.cache.GetCachedDocument(req.DocumentID, req.TargetLang)
	if err != nil || cached == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "document not found"})
	}

	history := make([]services.ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		history[i] = services.ChatMessage{Role: m.Role, Content: m.Content}
	}

	answer, citations, err := h.bedrock.ChatWithDocument(cached.Blocks, req.TargetLang, history)
	if err != nil {
		log.Printf("[Document] Chat error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "chat failed"})
	}
	return c.JSON(fiber.Map{"answer": answer, "citations": citations})
}

// loadDocumentForStudy parses a DocumentStudyRequest, looks up the document,
// and returns either (doc, 0, nil) on success or (nil, statusCode, error)
// on failure. The caller is responsible for writing the response — this
// helper deliberately does NOT call c.JSON since Fiber returns nil on a
// successful write, which makes "wrote error" indistinguishable from
// "wrote success" at the call site.
func (h *DocumentHandler) loadDocumentForStudy(c *fiber.Ctx) (*models.Document, int, error) {
	var req models.DocumentStudyRequest
	if err := c.BodyParser(&req); err != nil {
		return nil, fiber.StatusBadRequest, fmt.Errorf("invalid request body")
	}
	if req.DocumentID == "" || req.TargetLang == "" {
		return nil, fiber.StatusBadRequest, fmt.Errorf("document_id and target_lang required")
	}
	cached, err := h.cache.GetCachedDocument(req.DocumentID, req.TargetLang)
	if err != nil {
		log.Printf("[Document] cache lookup error: %v", err)
		return nil, fiber.StatusInternalServerError, fmt.Errorf("cache error")
	}
	if cached == nil {
		return nil, fiber.StatusNotFound, fmt.Errorf("document not found")
	}
	return cached, 0, nil
}

// generateDocumentStudyMaterials runs summary, quiz, vocab, and lesson in
// parallel and writes each to the document study cache. Mirrors the lecture
// pipeline.
func (h *DocumentHandler) generateDocumentStudyMaterials(documentID, targetLang string, blocks []models.DocumentBlock) {
	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		summary, citations, err := h.bedrock.SummarizeDocument(blocks, targetLang)
		if err != nil {
			log.Printf("[Document] auto-summary error: %v", err)
			return
		}
		data, jerr := json.Marshal(documentSummaryEnvelope{Summary: summary, Citations: citations})
		if jerr != nil {
			log.Printf("[Document] auto-summary marshal error: %v", jerr)
			return
		}
		if err := h.cache.SaveDocumentStudyMaterial(documentID, targetLang, "summary", string(data)); err != nil {
			log.Printf("[Document] auto-summary cache error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		quiz, err := h.bedrock.GenerateDocumentQuiz(blocks, targetLang)
		if err != nil {
			log.Printf("[Document] auto-quiz error: %v", err)
			return
		}
		raw, _ := json.Marshal(quiz)
		if err := h.cache.SaveDocumentStudyMaterial(documentID, targetLang, "quiz", string(raw)); err != nil {
			log.Printf("[Document] auto-quiz cache error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		vocab, err := h.bedrock.ExtractDocumentVocab(blocks, targetLang)
		if err != nil {
			log.Printf("[Document] auto-vocab error: %v", err)
			return
		}
		raw, _ := json.Marshal(vocab)
		if err := h.cache.SaveDocumentStudyMaterial(documentID, targetLang, "vocab", string(raw)); err != nil {
			log.Printf("[Document] auto-vocab cache error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		lessons, err := h.bedrock.GenerateDocumentLesson(blocks, targetLang)
		if err != nil {
			log.Printf("[Document] auto-lesson error: %v", err)
			return
		}
		raw, _ := json.Marshal(lessons)
		if err := h.cache.SaveDocumentStudyMaterial(documentID, targetLang, "lesson", string(raw)); err != nil {
			log.Printf("[Document] auto-lesson cache error: %v", err)
		}
	}()

	wg.Wait()
	log.Printf("[Document] Study materials generated for %s/%s", documentID, targetLang)
}

// ensureDocumentStudyMaterials backfills any missing summary/quiz/vocab/lesson
// on GetDocument so older cached documents pick up new study materials over
// time.
func (h *DocumentHandler) ensureDocumentStudyMaterials(documentID, targetLang string, blocks []models.DocumentBlock) {
	summary, _ := h.cache.GetDocumentStudyMaterial(documentID, targetLang, "summary")
	quiz, _ := h.cache.GetDocumentStudyMaterial(documentID, targetLang, "quiz")
	vocab, _ := h.cache.GetDocumentStudyMaterial(documentID, targetLang, "vocab")
	lesson, _ := h.cache.GetDocumentStudyMaterial(documentID, targetLang, "lesson")
	if summary != "" && quiz != "" && vocab != "" && lesson != "" {
		return
	}
	log.Printf("[Document] Backfilling study materials for %s/%s (summary=%v quiz=%v vocab=%v lesson=%v)",
		documentID, targetLang, summary == "", quiz == "", vocab == "", lesson == "")
	h.generateDocumentStudyMaterials(documentID, targetLang, blocks)
}

func documentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
