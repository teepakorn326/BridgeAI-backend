package handlers

import (
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v2"

	"studymind-backend/internal/database"
	"studymind-backend/internal/models"
	"studymind-backend/internal/services"
)

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

	// Step 4: Build response
	response := &models.ProcessResponse{
		VideoID:    videoID,
		VideoURL:   req.VideoURL,
		TargetLang: req.TargetLang,
		Title:      fmt.Sprintf("Video %s — %s", videoID, req.TargetLang),
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
