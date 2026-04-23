package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
		return nil, fmt.Errorf("transcript service returned %d: %s", resp.StatusCode, string(body))
	}

	var result TranscriptResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse transcript response: %w", err)
	}

	log.Printf("[Transcript] Got %d segments for video=%s", len(result.Segments), videoID)
	return &result, nil
}
