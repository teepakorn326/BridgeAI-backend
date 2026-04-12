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

// extractVideoID attempts to parse a YouTube video ID from a URL.
func extractVideoID(videoURL string) string {
	parsed, err := url.Parse(videoURL)
	if err != nil {
		// fallback: use the whole URL as an ID
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

	return strings.ReplaceAll(videoURL, "/", "_")
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
