package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"

	"studymind-backend/internal/models"
	"studymind-backend/internal/services"
)

// UploadAudio handles POST /api/upload-audio — forwards the uploaded audio
// to the transcript service for Whisper transcription, then translates and
// stores the resulting course.
func (h *CourseHandler) UploadAudio(c *fiber.Ctx) error {
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

	segs, err := h.transcript.TranscribeUpload(fh.Filename, raw)
	if err != nil {
		log.Printf("[Upload] audio transcribe failed: %v", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "audio transcription failed: " + err.Error(),
		})
	}
	if len(segs) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "no speech detected in audio",
		})
	}

	hash := contentHash(raw)
	videoID := "audio-" + hash[:16]
	videoURL := "audio://" + hash[:16]

	title := titleOverride
	if title == "" {
		title = trimAudioExt(fh.Filename)
	}
	if title == "" {
		title = "Audio recording"
	}

	return h.finalizeAudioUpload(c, videoID, videoURL, title, targetLang, segs)
}

// finalizeAudioUpload runs the "translate → cache → link → kick off study
// materials" flow for audio uploads. Mirrors IngestCourse. PDF uploads
// previously shared this path; they now live in the document pipeline.
func (h *CourseHandler) finalizeAudioUpload(
	c *fiber.Ctx,
	videoID, videoURL, title, targetLang string,
	segs []services.TranscriptSegment,
) error {
	if cached, _ := h.cache.GetCachedCourse(videoID, targetLang); cached != nil {
		log.Printf("[Upload] cache HIT for %s/%s — returning existing", videoID, targetLang)
		if uid, _ := c.Locals("userID").(string); uid != "" {
			if err := h.cache.LinkUserToCourse(uid, videoID, cached.VideoURL, cached.Title, targetLang); err != nil {
				log.Printf("[Upload] LinkUserToCourse error (non-fatal): %v", err)
			}
		}
		return c.JSON(cached)
	}

	subtitles, err := h.bedrock.TranslateSegments(segs, targetLang)
	if err != nil {
		log.Printf("[Upload] translation failed: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "translation failed"})
	}

	response := &models.ProcessResponse{
		VideoID:    videoID,
		VideoURL:   videoURL,
		Source:     "audio",
		TargetLang: targetLang,
		Title:      title,
		Subtitles:  subtitles,
		FromCache:  false,
	}

	if err := h.cache.SaveToCache(videoID, targetLang, response); err != nil {
		log.Printf("[Upload] cache save error: %v", err)
	}
	if uid, _ := c.Locals("userID").(string); uid != "" {
		if err := h.cache.LinkUserToCourse(uid, videoID, videoURL, title, targetLang); err != nil {
			log.Printf("[Upload] LinkUserToCourse error (non-fatal): %v", err)
		}
	}

	go h.generateStudyMaterials(videoID, targetLang, segs)

	log.Printf("[Upload] audio ingest complete: id=%s subtitles=%d", videoID, len(subtitles))
	return c.JSON(response)
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func trimAudioExt(name string) string {
	for _, ext := range []string{".mp3", ".wav", ".m4a", ".ogg", ".webm", ".flac", ".aac", ".mp4"} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

