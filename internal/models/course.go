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

// TranslateSegmentsRequest is the request body for POST /api/translate-segments.
type TranslateSegmentsRequest struct {
	Segments   []SegmentInput `json:"segments"`
	TargetLang string         `json:"target_lang"`
}

// SegmentInput represents a single transcript segment to translate.
type SegmentInput struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// TranslateSegmentsResponse is the response for POST /api/translate-segments.
type TranslateSegmentsResponse struct {
	Segments []TranslatedSegment `json:"segments"`
}

// TranslatedSegment is a segment with its translation.
type TranslatedSegment struct {
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Text        string  `json:"text"`
	Translation string  `json:"translation"`
}

// StudyMaterialRequest requests AI-generated study materials for a cached course.
type StudyMaterialRequest struct {
	VideoURL   string `json:"video_url"`
	TargetLang string `json:"target_lang"`
}

// QuizQuestion is a single multiple-choice question.
type QuizQuestion struct {
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	Correct     int      `json:"correct"`
	Explanation string   `json:"explanation"`
}

// VocabEntry is a vocabulary term with translation and definition.
type VocabEntry struct {
	Term        string `json:"term"`
	Translation string `json:"translation"`
	Definition  string `json:"definition"`
}

// ChatTurn is a single message in a chat conversation.
type ChatTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest asks the study assistant about a cached course transcript.
type ChatRequest struct {
	VideoURL   string     `json:"video_url"`
	VideoID    string     `json:"video_id"`
	TargetLang string     `json:"target_lang"`
	Messages   []ChatTurn `json:"messages"`
}

// IngestRequest is sent by the Chrome extension after capturing a transcript.
// If Subtitles is provided (pre-translated), translation is skipped.
type IngestRequest struct {
	Source     string         `json:"source"`
	SourceURL  string         `json:"source_url"`
	Title      string         `json:"title"`
	TargetLang string         `json:"target_lang"`
	Segments   []SegmentInput `json:"segments"`
	Subtitles  []SubtitleLine `json:"subtitles,omitempty"`
}

// ProcessResponse is the full response sent back to the frontend.
type ProcessResponse struct {
	VideoID    string         `json:"video_id"`
	VideoURL   string         `json:"video_url"`
	Source     string         `json:"source,omitempty"`
	TargetLang string         `json:"target_lang"`
	Title      string         `json:"title"`
	Subtitles  []SubtitleLine `json:"subtitles"`
	FromCache  bool           `json:"from_cache"`
}
