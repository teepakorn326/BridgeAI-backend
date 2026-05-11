package models

// Document is an uploaded study artifact (PDF, slide deck, Word doc, plain
// text, or image) translated into the user's native language. Lives in a
// separate pipeline from lectures because there is no time dimension —
// navigation is by page and reading order, not timestamps.
type Document struct {
	DocumentID string          `json:"document_id" dynamodbav:"document_id"`
	Source     string          `json:"source" dynamodbav:"source"` // pdf|docx|pptx|txt|image
	SourceURL  string          `json:"source_url" dynamodbav:"source_url"`
	Title      string          `json:"title" dynamodbav:"title"`
	TargetLang string          `json:"target_lang" dynamodbav:"target_lang"`
	Blocks     []DocumentBlock `json:"blocks" dynamodbav:"blocks"`
	FromCache  bool            `json:"from_cache" dynamodbav:"-"`
}

// DocumentBlock is one paragraph, slide bullet, list item, or image-caption —
// the unit at which we translate, render, and cite. Page is 1-based for
// paginated sources (PDF/PPTX); 0 for sources with no concept of pages (TXT,
// single-image upload, DOCX).
type DocumentBlock struct {
	Page           int    `json:"page" dynamodbav:"page"`
	Order          int    `json:"order" dynamodbav:"order"`
	TextEN         string `json:"text_en" dynamodbav:"text_en"`
	TextTranslated string `json:"text_translated" dynamodbav:"text_translated"`
}

// DocumentSummary is a lightweight listing entry (no blocks).
type DocumentSummary struct {
	DocumentID string `json:"document_id"`
	SourceURL  string `json:"source_url"`
	Source     string `json:"source"`
	Title      string `json:"title"`
	TargetLang string `json:"target_lang"`
}

// DocumentChatRequest asks the document chatbot a question grounded in a
// stored document.
type DocumentChatRequest struct {
	DocumentID string     `json:"document_id"`
	TargetLang string     `json:"target_lang"`
	Messages   []ChatTurn `json:"messages"`
}

// DocumentStudyRequest is the shared shape for /document/:id/{summary,quiz,vocab}.
type DocumentStudyRequest struct {
	DocumentID string `json:"document_id"`
	TargetLang string `json:"target_lang"`
}

// DocumentChatCitation maps an inline [N] marker in a chat answer to the
// block it references. Unlike lectures, citations carry a page number for
// "jump to page" UX rather than a timestamp.
type DocumentChatCitation struct {
	N      int    `json:"n"`
	Page   int    `json:"page"`
	Order  int    `json:"order"`
	TextEN string `json:"text_en"`
}

// DocumentLesson is one chapter of a generated lesson — a self-contained
// study unit with its own bilingual title, citation-rich mini-summary, and
// 2-3 multiple-choice questions. The frontend renders these as collapsible
// chapter cards.
type DocumentLesson struct {
	TitleEN          string                 `json:"title_en"`
	TitleTranslated  string                 `json:"title_translated"`
	Summary          string                 `json:"summary"`
	SummaryCitations []DocumentChatCitation `json:"summary_citations"`
	Quiz             []QuizQuestion         `json:"quiz"`
}
