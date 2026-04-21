package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"studymind-backend/internal/database"
	"studymind-backend/internal/models"
	"studymind-backend/internal/services"
)

// fetchYouTubeTitle fetches the real video title via YouTube's oEmbed API.
// Free, no API key required. Returns empty string on failure.
func fetchYouTubeTitle(videoID string) string {
	endpoint := fmt.Sprintf("https://www.youtube.com/oembed?url=https://www.youtube.com/watch?v=%s&format=json", videoID)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(endpoint)
	if err != nil {
		log.Printf("[Handler] oEmbed fetch failed for %s: %v", videoID, err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[Handler] oEmbed returned %d for %s", resp.StatusCode, videoID)
		return ""
	}

	var data struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("[Handler] oEmbed decode failed: %v", err)
		return ""
	}
	return data.Title
}

// hashSegments returns a deterministic SHA-256 hash of segment text content.
// Used as a cache key so identical transcripts hit the cache regardless of
// minor timestamp differences.
func hashSegments(segments []models.SegmentInput) string {
	h := sha256.New()
	for _, s := range segments {
		h.Write([]byte(s.Text))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CourseHandler holds dependencies for course-related endpoints.
type CourseHandler struct {
	cache      *database.CacheService
	bedrock    *services.BedrockService
	transcript *services.TranscriptClient
}

// NewCourseHandler creates a new handler with injected dependencies.
func NewCourseHandler(cache *database.CacheService, bedrock *services.BedrockService, transcript *services.TranscriptClient) *CourseHandler {
	return &CourseHandler{
		cache:      cache,
		bedrock:    bedrock,
		transcript: transcript,
	}
}

// loadCachedSegments loads translated segments for a course from cache,
// or returns an error suitable for sending to the client.
func (h *CourseHandler) loadCachedSegments(videoURL, targetLang string) ([]services.TranscriptSegment, error) {
	videoID := extractVideoID(videoURL)
	cached, err := h.cache.GetCachedCourse(videoID, targetLang)
	if err != nil {
		return nil, fmt.Errorf("cache lookup: %w", err)
	}
	if cached == nil {
		return nil, fmt.Errorf("course not found — translate it first")
	}
	segs := make([]services.TranscriptSegment, len(cached.Subtitles))
	for i, s := range cached.Subtitles {
		segs[i] = services.TranscriptSegment{
			StartSeconds: s.StartSeconds,
			EndSeconds:   s.EndSeconds,
			Text:         s.TextEN,
		}
	}
	return segs, nil
}

// Summarize handles POST /api/summarize.
func (h *CourseHandler) Summarize(c *fiber.Ctx) error {
	var req models.StudyMaterialRequest
	if err := c.BodyParser(&req); err != nil || req.VideoURL == "" || req.TargetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "video_url and target_lang required"})
	}
	videoID := extractVideoID(req.VideoURL)

	if cached, _ := h.cache.GetStudyMaterial(videoID, req.TargetLang, "summary"); cached != "" {
		log.Printf("[Handler] Summary cache HIT for %s/%s", videoID, req.TargetLang)
		return c.JSON(fiber.Map{"summary": cached, "from_cache": true})
	}

	segs, err := h.loadCachedSegments(req.VideoURL, req.TargetLang)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	summary, err := h.bedrock.Summarize(segs, req.TargetLang)
	if err != nil {
		log.Printf("[Handler] Summarize error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Summary generation failed"})
	}

	go h.cache.SaveStudyMaterial(videoID, req.TargetLang, "summary", summary)
	return c.JSON(fiber.Map{"summary": summary, "from_cache": false})
}

// GenerateQuiz handles POST /api/quiz.
func (h *CourseHandler) GenerateQuiz(c *fiber.Ctx) error {
	var req models.StudyMaterialRequest
	if err := c.BodyParser(&req); err != nil || req.VideoURL == "" || req.TargetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "video_url and target_lang required"})
	}
	videoID := extractVideoID(req.VideoURL)

	if cached, _ := h.cache.GetStudyMaterial(videoID, req.TargetLang, "quiz"); cached != "" {
		log.Printf("[Handler] Quiz cache HIT for %s/%s", videoID, req.TargetLang)
		var quiz []models.QuizQuestion
		if err := json.Unmarshal([]byte(cached), &quiz); err == nil {
			return c.JSON(fiber.Map{"quiz": quiz, "from_cache": true})
		}
	}

	segs, err := h.loadCachedSegments(req.VideoURL, req.TargetLang)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	quiz, err := h.bedrock.GenerateQuiz(segs, req.TargetLang)
	if err != nil {
		log.Printf("[Handler] GenerateQuiz error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Quiz generation failed"})
	}

	if data, err := json.Marshal(quiz); err == nil {
		go h.cache.SaveStudyMaterial(videoID, req.TargetLang, "quiz", string(data))
	}
	return c.JSON(fiber.Map{"quiz": quiz, "from_cache": false})
}

// ExtractVocab handles POST /api/vocab.
func (h *CourseHandler) ExtractVocab(c *fiber.Ctx) error {
	var req models.StudyMaterialRequest
	if err := c.BodyParser(&req); err != nil || req.VideoURL == "" || req.TargetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "video_url and target_lang required"})
	}
	videoID := extractVideoID(req.VideoURL)

	if cached, _ := h.cache.GetStudyMaterial(videoID, req.TargetLang, "vocab"); cached != "" {
		log.Printf("[Handler] Vocab cache HIT for %s/%s", videoID, req.TargetLang)
		var vocab []models.VocabEntry
		if err := json.Unmarshal([]byte(cached), &vocab); err == nil {
			return c.JSON(fiber.Map{"vocab": vocab, "from_cache": true})
		}
	}

	segs, err := h.loadCachedSegments(req.VideoURL, req.TargetLang)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}

	vocab, err := h.bedrock.ExtractVocab(segs, req.TargetLang)
	if err != nil {
		log.Printf("[Handler] ExtractVocab error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "Vocab generation failed"})
	}

	if data, err := json.Marshal(vocab); err == nil {
		go h.cache.SaveStudyMaterial(videoID, req.TargetLang, "vocab", string(data))
	}
	return c.JSON(fiber.Map{"vocab": vocab, "from_cache": false})
}

// ListCourses handles GET /api/courses — returns all cached courses.
// Backfills missing/fallback titles by fetching from YouTube oEmbed.
func (h *CourseHandler) ListCourses(c *fiber.Ctx) error {
	summaries, err := h.cache.ListCachedCourses()
	if err != nil {
		log.Printf("[Handler] ListCourses error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to fetch courses",
		})
	}

	// Backfill bad titles (old "Video XXX — Lang" pattern). Cache real
	// titles by video_id so we only fetch once per unique video.
	titleCache := make(map[string]string)
	for i, s := range summaries {
		if !strings.Contains(s.Title, " — ") && s.Title != "" && !strings.HasPrefix(s.Title, "Video ") {
			continue // already a real title
		}
		if cached, ok := titleCache[s.VideoID]; ok {
			summaries[i].Title = cached
			continue
		}
		real := fetchYouTubeTitle(s.VideoID)
		if real != "" {
			summaries[i].Title = real
			titleCache[s.VideoID] = real
		}
	}

	return c.JSON(fiber.Map{"courses": summaries})
}

// TranslateSegments handles POST /api/translate-segments.
func (h *CourseHandler) TranslateSegments(c *fiber.Ctx) error {
	var req models.TranslateSegmentsRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body. Required: segments, target_lang",
		})
	}

	if len(req.Segments) == 0 || req.TargetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "segments (non-empty) and target_lang are required",
		})
	}

	log.Printf("[Handler] TranslateSegments: %d segments, lang=%s", len(req.Segments), req.TargetLang)

	// Check cache by content hash
	hash := hashSegments(req.Segments)
	if cached, err := h.cache.GetCachedSegments(hash, req.TargetLang); err != nil {
		log.Printf("[Handler] Segments cache lookup error (non-fatal): %v", err)
	} else if cached != nil {
		log.Printf("[Handler] Cache HIT for hash=%s lang=%s (%d segments)", hash[:8], req.TargetLang, len(cached))
		return c.JSON(models.TranslateSegmentsResponse{Segments: cached})
	}

	log.Printf("[Handler] Cache MISS for hash=%s — translating", hash[:8])

	// Map request segments to services.TranscriptSegment
	transcriptSegs := make([]services.TranscriptSegment, len(req.Segments))
	for i, s := range req.Segments {
		transcriptSegs[i] = services.TranscriptSegment{
			StartSeconds: s.Start,
			EndSeconds:   s.End,
			Text:         s.Text,
		}
	}

	// Translate using existing Bedrock service
	subtitles, err := h.bedrock.TranslateSegments(transcriptSegs, req.TargetLang)
	if err != nil {
		log.Printf("[Handler] TranslateSegments error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to generate translation. Please try again.",
		})
	}

	// Map back to response format
	out := make([]models.TranslatedSegment, len(subtitles))
	for i, s := range subtitles {
		out[i] = models.TranslatedSegment{
			Start:       s.StartSeconds,
			End:         s.EndSeconds,
			Text:        s.TextEN,
			Translation: s.TextTranslated,
		}
	}

	// Cache result (fire-and-forget)
	go func() {
		if err := h.cache.SaveSegmentsToCache(hash, req.TargetLang, out); err != nil {
			log.Printf("[Handler] Failed to cache segments: %v", err)
		}
	}()

	return c.JSON(models.TranslateSegmentsResponse{Segments: out})
}

// IngestCourse handles POST /api/ingest-course — called by the Chrome
// extension after a transcript is captured from Udemy / Coursera / Echo360.
// Translates, caches, and kicks off summary/quiz/vocab generation in the
// background so they're ready by the time the user opens the course page.
func (h *CourseHandler) IngestCourse(c *fiber.Ctx) error {
	var req models.IngestRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.SourceURL == "" || req.TargetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "source_url and target_lang are required",
		})
	}
	if len(req.Subtitles) == 0 && len(req.Segments) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "segments or subtitles are required",
		})
	}

	videoID := extractVideoID(req.SourceURL)
	log.Printf("[Handler] Ingest source=%s id=%s lang=%s segments=%d subtitles=%d",
		req.Source, videoID, req.TargetLang, len(req.Segments), len(req.Subtitles))

	if cached, _ := h.cache.GetCachedCourse(videoID, req.TargetLang); cached != nil {
		log.Printf("[Handler] Ingest cache HIT — returning existing")
		return c.JSON(cached)
	}

	// Build English segments for study-material generation.
	var segs []services.TranscriptSegment
	var subtitles []models.SubtitleLine

	if len(req.Subtitles) > 0 {
		// Pre-translated from the extension — skip Bedrock translation.
		log.Printf("[Handler] Using %d pre-translated subtitles (skipping translation)", len(req.Subtitles))
		subtitles = req.Subtitles
		segs = make([]services.TranscriptSegment, len(req.Subtitles))
		for i, s := range req.Subtitles {
			segs[i] = services.TranscriptSegment{
				StartSeconds: s.StartSeconds,
				EndSeconds:   s.EndSeconds,
				Text:         s.TextEN,
			}
		}
	} else {
		// Raw segments — translate via Bedrock.
		segs = make([]services.TranscriptSegment, len(req.Segments))
		for i, s := range req.Segments {
			segs[i] = services.TranscriptSegment{
				StartSeconds: s.Start,
				EndSeconds:   s.End,
				Text:         s.Text,
			}
		}
		var err error
		subtitles, err = h.bedrock.TranslateSegments(segs, req.TargetLang)
		if err != nil {
			log.Printf("[Handler] Ingest translation error: %v", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "translation failed"})
		}
	}

	title := req.Title
	if title == "" {
		title = fmt.Sprintf("%s lecture", req.Source)
	}

	response := &models.ProcessResponse{
		VideoID:    videoID,
		VideoURL:   req.SourceURL,
		Source:     req.Source,
		TargetLang: req.TargetLang,
		Title:      title,
		Subtitles:  subtitles,
		FromCache:  false,
	}

	if err := h.cache.SaveToCache(videoID, req.TargetLang, response); err != nil {
		log.Printf("[Handler] Ingest cache save error: %v", err)
	}

	// Kick off study-material generation in the background.
	go h.generateStudyMaterials(videoID, req.TargetLang, segs)

	log.Printf("[Handler] Ingest complete: %d subtitles", len(subtitles))
	return c.JSON(response)
}

// generateStudyMaterials runs Summarize, GenerateQuiz, and ExtractVocab
// in parallel and writes each to the DynamoDB cache. Used by IngestCourse.
func (h *CourseHandler) generateStudyMaterials(videoID, targetLang string, segs []services.TranscriptSegment) {
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		summary, err := h.bedrock.Summarize(segs, targetLang)
		if err != nil {
			log.Printf("[Handler] auto-summary error: %v", err)
			return
		}
		if err := h.cache.SaveStudyMaterial(videoID, targetLang, "summary", summary); err != nil {
			log.Printf("[Handler] auto-summary cache error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		quiz, err := h.bedrock.GenerateQuiz(segs, targetLang)
		if err != nil {
			log.Printf("[Handler] auto-quiz error: %v", err)
			return
		}
		raw, _ := json.Marshal(quiz)
		if err := h.cache.SaveStudyMaterial(videoID, targetLang, "quiz", string(raw)); err != nil {
			log.Printf("[Handler] auto-quiz cache error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		vocab, err := h.bedrock.ExtractVocab(segs, targetLang)
		if err != nil {
			log.Printf("[Handler] auto-vocab error: %v", err)
			return
		}
		raw, _ := json.Marshal(vocab)
		if err := h.cache.SaveStudyMaterial(videoID, targetLang, "vocab", string(raw)); err != nil {
			log.Printf("[Handler] auto-vocab cache error: %v", err)
		}
	}()

	wg.Wait()
	log.Printf("[Handler] Study materials generated for %s/%s", videoID, targetLang)
}

// GetCourse handles GET /api/course?id=...&lang=... (or ?url=...&lang=...).
// Returns a cached course without triggering any transcript fetch or translation.
func (h *CourseHandler) GetCourse(c *fiber.Ctx) error {
	videoID := c.Query("id")
	if videoID == "" {
		videoURL := c.Query("url")
		if videoURL != "" {
			videoID = extractVideoID(videoURL)
		}
	}
	targetLang := c.Query("lang")
	if videoID == "" || targetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "id or url, and lang required"})
	}
	cached, err := h.cache.GetCachedCourse(videoID, targetLang)
	if err != nil {
		log.Printf("[Handler] GetCourse cache error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "cache error"})
	}
	if cached == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "course not found — capture it with the extension first"})
	}

	// Backfill study materials for older courses cached before the
	// auto-generation pipeline existed. Fire-and-forget — the UI still
	// gets the translated course immediately.
	go h.ensureStudyMaterials(videoID, targetLang, cached.Subtitles)

	return c.JSON(cached)
}

// ensureStudyMaterials triggers background generation for any of
// summary/quiz/vocab that aren't already cached. Safe to call on every
// GetCourse — skips quickly if all three exist.
func (h *CourseHandler) ensureStudyMaterials(videoID, targetLang string, subtitles []models.SubtitleLine) {
	summary, _ := h.cache.GetStudyMaterial(videoID, targetLang, "summary")
	quiz, _ := h.cache.GetStudyMaterial(videoID, targetLang, "quiz")
	vocab, _ := h.cache.GetStudyMaterial(videoID, targetLang, "vocab")
	if summary != "" && quiz != "" && vocab != "" {
		return
	}

	segs := make([]services.TranscriptSegment, len(subtitles))
	for i, s := range subtitles {
		segs[i] = services.TranscriptSegment{
			StartSeconds: s.StartSeconds,
			EndSeconds:   s.EndSeconds,
			Text:         s.TextEN,
		}
	}

	log.Printf("[Handler] Backfilling study materials for %s/%s (summary=%v quiz=%v vocab=%v)",
		videoID, targetLang, summary == "", quiz == "", vocab == "")
	h.generateStudyMaterials(videoID, targetLang, segs)
}

// Chat handles POST /api/chat — answer a question about a cached transcript.
func (h *CourseHandler) Chat(c *fiber.Ctx) error {
	var req models.ChatRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	if req.TargetLang == "" || len(req.Messages) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "target_lang and messages required"})
	}

	videoID := req.VideoID
	if videoID == "" && req.VideoURL != "" {
		videoID = extractVideoID(req.VideoURL)
	}
	if videoID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "video_id or video_url required"})
	}

	cached, err := h.cache.GetCachedCourse(videoID, req.TargetLang)
	if err != nil || cached == nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "course not found"})
	}

	segs := make([]services.TranscriptSegment, len(cached.Subtitles))
	for i, s := range cached.Subtitles {
		segs[i] = services.TranscriptSegment{
			StartSeconds: s.StartSeconds,
			EndSeconds:   s.EndSeconds,
			Text:         s.TextEN,
		}
	}

	history := make([]services.ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		history[i] = services.ChatMessage{Role: m.Role, Content: m.Content}
	}

	answer, err := h.bedrock.ChatWithTranscript(segs, req.TargetLang, history)
	if err != nil {
		log.Printf("[Handler] Chat error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "chat failed"})
	}

	return c.JSON(fiber.Map{"answer": answer})
}

// extractVideoID returns a stable cache key from a video URL.
// For YouTube: extracts the v= parameter.
// For other platforms: uses host + path (stripped of query params and
// fragments so the same lecture from different browsers gets the same key).
func extractVideoID(videoURL string) string {
	parsed, err := url.Parse(videoURL)
	if err != nil {
		return strings.ReplaceAll(videoURL, "/", "_")
	}

	// Handle youtu.be short links
	if strings.Contains(parsed.Host, "youtu.be") {
		return strings.TrimPrefix(parsed.Path, "/")
	}

	// Handle youtube.com/watch?v=xxx
	if v := parsed.Query().Get("v"); v != "" {
		return v
	}

	// Handle youtube.com/embed/xxx
	parts := strings.Split(parsed.Path, "/")
	for i, p := range parts {
		if p == "embed" && i+1 < len(parts) {
			return parts[i+1]
		}
	}

	// Non-YouTube: use host + path only (ignore query params, fragments,
	// session tokens) so the same lecture from different browsers or
	// sessions produces the same cache key.
	stable := parsed.Host + parsed.Path
	stable = strings.TrimRight(stable, "/")
	return strings.ReplaceAll(stable, "/", "_")
}

// ProcessCourse handles POST /api/process-course.
func (h *CourseHandler) ProcessCourse(c *fiber.Ctx) error {
	var req models.ProcessRequest
	if err := c.BodyParser(&req); err != nil {
		log.Printf("[Handler] Bad request body: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid request body. Required: video_url, target_lang",
		})
	}

	if req.VideoURL == "" || req.TargetLang == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Both video_url and target_lang are required",
		})
	}

	videoID := extractVideoID(req.VideoURL)
	log.Printf("[Handler] Processing video=%s lang=%s", videoID, req.TargetLang)

	// Step 1: Check DynamoDB cache
	cached, err := h.cache.GetCachedCourse(videoID, req.TargetLang)
	if err != nil {
		log.Printf("[Handler] Cache lookup error (non-fatal): %v", err)
		// Continue without cache — don't fail the request
	}

	if cached != nil {
		log.Printf("[Handler] Cache HIT for video=%s lang=%s", videoID, req.TargetLang)
		return c.JSON(cached)
	}

	log.Printf("[Handler] Cache MISS — fetching real transcript")

	// Step 2: Fetch real YouTube transcript
	transcriptResp, err := h.transcript.FetchTranscript(videoID)
	if err != nil {
		log.Printf("[Handler] Transcript fetch error: %v", err)
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Could not fetch transcript for this video. Make sure the video has captions available.",
		})
	}

	// Step 3: Call Bedrock for translation
	subtitles, err := h.bedrock.TranslateSegments(transcriptResp.Segments, req.TargetLang)
	if err != nil {
		log.Printf("[Handler] Bedrock translation error: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Failed to generate translation. Please try again.",
		})
	}

	// Fetch real YouTube title (falls back to ID if oEmbed fails)
	title := fetchYouTubeTitle(videoID)
	if title == "" {
		title = fmt.Sprintf("Video %s", videoID)
	}

	// Step 4: Build response
	response := &models.ProcessResponse{
		VideoID:    videoID,
		VideoURL:   req.VideoURL,
		TargetLang: req.TargetLang,
		Title:      title,
		Subtitles:  subtitles,
		FromCache:  false,
	}

	// Step 4: Cache result in DynamoDB (fire-and-forget for speed)
	go func() {
		if err := h.cache.SaveToCache(videoID, req.TargetLang, response); err != nil {
			log.Printf("[Handler] Failed to cache result: %v", err)
		}
	}()

	log.Printf("[Handler] Returning %d subtitle lines", len(subtitles))
	return c.JSON(response)
}
