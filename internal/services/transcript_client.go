package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"time"
)

// TranscriptSegment is a single segment from the YouTube transcript.
type TranscriptSegment struct {
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	Text         string  `json:"text"`
}

// TranscriptResponse is the response from the transcript service.
type TranscriptResponse struct {
	VideoID  string              `json:"video_id"`
	Segments []TranscriptSegment `json:"segments"`
}

// TranscriptClient calls the Python transcript sidecar.
type TranscriptClient struct {
	baseURL string
}

// NewTranscriptClient creates a new client.
func NewTranscriptClient() *TranscriptClient {
	baseURL := os.Getenv("TRANSCRIPT_SERVICE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8081"
	}
	log.Printf("[Transcript] Client initialized, base URL: %s", baseURL)
	return &TranscriptClient{baseURL: baseURL}
}

// FetchTranscript retrieves the YouTube transcript for a video.
func (t *TranscriptClient) FetchTranscript(videoID, lang string) (*TranscriptResponse, error) {
	url := fmt.Sprintf("%s/transcript/%s?lang=%s", t.baseURL, videoID, lang)
	log.Printf("[Transcript] Fetching transcript for video=%s lang=%s", videoID, lang)

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("transcript service request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// FastAPI HTTPExceptions come back as {"detail": "..."}. Surface
		// that to the caller so users see the real reason (bot blocking,
		// no captions, proxy missing, etc.) instead of a generic message.
		return nil, fmt.Errorf("%s", parseDetail(body, resp.StatusCode))
	}

	var result TranscriptResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse transcript response: %w", err)
	}

	log.Printf("[Transcript] Got %d segments for video=%s", len(result.Segments), videoID)
	return &result, nil
}

// parseDetail extracts the FastAPI {"detail": "..."} message; falls back
// to the raw body if the JSON shape is unexpected.
func parseDetail(body []byte, status int) string {
	var payload struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Detail != "" {
		return payload.Detail
	}
	if len(body) > 0 {
		return fmt.Sprintf("transcript service returned %d: %s", status, string(body))
	}
	return fmt.Sprintf("transcript service returned %d", status)
}

// TranscribeUpload sends an audio file to the transcript service for
// Whisper transcription, and returns timestamped segments.
func (t *TranscriptClient) TranscribeUpload(filename string, data []byte) ([]TranscriptSegment, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("write form file: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}

	url := fmt.Sprintf("%s/transcribe-upload", t.baseURL)
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Whisper transcription on the upload can take a while for long audio.
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transcribe upload: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read transcribe response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transcribe service returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Segments []TranscriptSegment `json:"segments"`
		Source   string              `json:"source"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse transcribe response: %w", err)
	}
	log.Printf("[Transcript] Got %d segments from upload (%s)", len(result.Segments), result.Source)
	return result.Segments, nil
}
