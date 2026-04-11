package models

// ProcessRequest represents the incoming request from the frontend.
type ProcessRequest struct {
	VideoURL   string `json:"video_url"`
	TargetLang string `json:"target_lang"`
}

// SubtitleLine represents a single line of translated subtitle.
type SubtitleLine struct {
	StartSeconds   float64 `json:"start_seconds" dynamodbav:"start_seconds"`
	EndSeconds     float64 `json:"end_seconds" dynamodbav:"end_seconds"`
	TextEN         string  `json:"text_en" dynamodbav:"text_en"`
	TextTranslated string  `json:"text_translated" dynamodbav:"text_translated"`
}

// ProcessResponse is the full response sent back to the frontend.
type ProcessResponse struct {
	VideoID    string         `json:"video_id"`
	VideoURL   string         `json:"video_url"`
	TargetLang string         `json:"target_lang"`
	Title      string         `json:"title"`
	Subtitles  []SubtitleLine `json:"subtitles"`
	FromCache  bool           `json:"from_cache"`
}
