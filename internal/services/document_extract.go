package services

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"studymind-backend/internal/models"
)

// DocumentExtractor turns an uploaded file (PDF, DOCX, PPTX, TXT, image)
// into ordered, page-aware blocks ready for translation.
//
// PDFs and images go through Claude vision (delegated to BedrockService).
// DOCX and PPTX are parsed in-process with stdlib zip + XML — they are just
// zipped Open Office XML, so no external dependency is needed for plain-text
// extraction. Legacy .doc/.ppt are rejected because Go has no first-party
// reader and the right user fix is "save as .docx/.pptx".
type DocumentExtractor struct {
	bedrock *BedrockService
}

func NewDocumentExtractor(bedrock *BedrockService) *DocumentExtractor {
	return &DocumentExtractor{bedrock: bedrock}
}

// ErrUnsupportedDocument is returned for legacy formats and unknown types.
type ErrUnsupportedDocument struct {
	Detail string
}

func (e *ErrUnsupportedDocument) Error() string { return e.Detail }

// Extract dispatches on filename extension and returns reading-order blocks.
// The caller is responsible for translating block.TextEN.
func (d *DocumentExtractor) Extract(filename string, raw []byte) ([]models.DocumentBlock, string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		blocks, err := d.bedrock.ExtractDocumentPDF(raw)
		return blocks, "pdf", err
	case ".docx":
		blocks, err := extractDOCX(raw)
		return blocks, "docx", err
	case ".pptx":
		blocks, err := extractPPTX(raw)
		return blocks, "pptx", err
	case ".txt", ".md":
		blocks, err := extractTXT(raw)
		return blocks, "txt", err
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		mediaType := imageMediaType(ext)
		blocks, err := d.bedrock.ExtractDocumentImage(raw, mediaType)
		return blocks, "image", err
	case ".doc", ".ppt":
		return nil, "", &ErrUnsupportedDocument{
			Detail: "legacy " + strings.TrimPrefix(ext, ".") + " files are not supported — please re-save as " + strings.TrimPrefix(ext, ".") + "x and re-upload",
		}
	default:
		return nil, "", &ErrUnsupportedDocument{
			Detail: "unsupported file type: " + ext,
		}
	}
}

func imageMediaType(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

// ── DOCX ───────────────────────────────────────────────────────────────────
//
// A .docx is a zip archive whose word/document.xml holds the body. Text is
// inside <w:t> elements nested under <w:p> paragraphs. We don't care about
// styling — just collect the text per paragraph.

type docxParagraph struct {
	XMLName xml.Name    `xml:"http://schemas.openxmlformats.org/wordprocessingml/2006/main p"`
	Runs    []docxRun   `xml:"r"`
	Hyper   []docxHyper `xml:"hyperlink"`
}

type docxRun struct {
	Texts []docxText `xml:"t"`
}

type docxHyper struct {
	Runs []docxRun `xml:"r"`
}

type docxText struct {
	Value string `xml:",chardata"`
}

func extractDOCX(raw []byte) ([]models.DocumentBlock, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("docx is not a valid zip archive: %w", err)
	}

	var docXML *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML = f
			break
		}
	}
	if docXML == nil {
		return nil, fmt.Errorf("docx missing word/document.xml — is this a real .docx?")
	}

	rc, err := docXML.Open()
	if err != nil {
		return nil, fmt.Errorf("open docx body: %w", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read docx body: %w", err)
	}

	dec := xml.NewDecoder(bytes.NewReader(body))
	var blocks []models.DocumentBlock
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode docx xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local != "p" {
			continue
		}
		var p docxParagraph
		if err := dec.DecodeElement(&p, &se); err != nil {
			continue
		}
		text := paragraphText(p)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		blocks = append(blocks, models.DocumentBlock{
			Page:   0,
			Order:  len(blocks),
			TextEN: text,
		})
	}
	return blocks, nil
}

func paragraphText(p docxParagraph) string {
	var sb strings.Builder
	for _, r := range p.Runs {
		for _, t := range r.Texts {
			sb.WriteString(t.Value)
		}
	}
	for _, h := range p.Hyper {
		for _, r := range h.Runs {
			for _, t := range r.Texts {
				sb.WriteString(t.Value)
			}
		}
	}
	return sb.String()
}

// ── PPTX ───────────────────────────────────────────────────────────────────
//
// A .pptx zip contains ppt/slides/slideN.xml — one file per slide. Text is
// in <a:t> elements. We page = slide number, in numeric order.

type pptxText struct {
	Value string `xml:",chardata"`
}

func extractPPTX(raw []byte) ([]models.DocumentBlock, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("pptx is not a valid zip archive: %w", err)
	}

	type slideEntry struct {
		num  int
		file *zip.File
	}
	var slides []slideEntry
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "ppt/slides/slide") || !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		// Extract the numeric suffix from "ppt/slides/slide<N>.xml"
		base := strings.TrimSuffix(strings.TrimPrefix(f.Name, "ppt/slides/slide"), ".xml")
		n, err := strconv.Atoi(base)
		if err != nil {
			continue
		}
		slides = append(slides, slideEntry{num: n, file: f})
	}
	if len(slides) == 0 {
		return nil, fmt.Errorf("pptx contained no slides")
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].num < slides[j].num })

	var blocks []models.DocumentBlock
	for _, s := range slides {
		texts, err := pptxSlideTexts(s.file)
		if err != nil {
			// Skip the slide rather than failing the whole upload.
			continue
		}
		for _, t := range texts {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			blocks = append(blocks, models.DocumentBlock{
				Page:   s.num,
				Order:  len(blocks),
				TextEN: t,
			})
		}
	}
	return blocks, nil
}

func pptxSlideTexts(f *zip.File) ([]string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	dec := xml.NewDecoder(bytes.NewReader(body))
	var texts []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local != "t" {
			continue
		}
		var t pptxText
		if err := dec.DecodeElement(&t, &se); err != nil {
			continue
		}
		texts = append(texts, t.Value)
	}
	// Group runs back into bullet-sized blocks by joining with spaces. PPTX
	// text is highly fragmented (each style run is its own <a:t>), so without
	// joining we'd produce hundreds of single-word blocks per slide.
	joined := strings.Join(texts, " ")
	// Split on newlines (slide bullets often use <a:br/> -> newline in text).
	parts := strings.Split(joined, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 && strings.TrimSpace(joined) != "" {
		out = []string{strings.TrimSpace(joined)}
	}
	return out, nil
}

// ── TXT ────────────────────────────────────────────────────────────────────
//
// Plain text has no structure. Split on blank lines (paragraph boundaries).
// If a paragraph runs over ~80 words it gets sub-split at sentence boundaries
// so the translation pipeline doesn't have to chew on giant blocks.

const txtMaxWordsPerBlock = 80

func extractTXT(raw []byte) ([]models.DocumentBlock, error) {
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("text file is empty")
	}

	// Normalize CRLF → LF for consistent paragraph splitting on Windows-saved files.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	paragraphs := strings.Split(text, "\n\n")

	var blocks []models.DocumentBlock
	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for _, chunk := range splitLongParagraph(p, txtMaxWordsPerBlock) {
			blocks = append(blocks, models.DocumentBlock{
				Page:   0,
				Order:  len(blocks),
				TextEN: chunk,
			})
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("text file produced no non-empty blocks")
	}
	return blocks, nil
}

// splitLongParagraph keeps a paragraph as one block if it's short, otherwise
// splits at sentence boundaries until each chunk is under maxWords.
func splitLongParagraph(p string, maxWords int) []string {
	words := strings.Fields(p)
	if len(words) <= maxWords {
		return []string{p}
	}
	// Sentence-aware split — prefer ". " / "! " / "? " boundaries.
	var out []string
	var current strings.Builder
	currentWords := 0
	flush := func() {
		s := strings.TrimSpace(current.String())
		if s != "" {
			out = append(out, s)
		}
		current.Reset()
		currentWords = 0
	}
	sentences := splitSentences(p)
	for _, s := range sentences {
		sw := len(strings.Fields(s))
		if currentWords+sw > maxWords && currentWords > 0 {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString(" ")
		}
		current.WriteString(s)
		currentWords += sw
	}
	flush()
	if len(out) == 0 {
		return []string{p}
	}
	return out
}

// splitSentences is a deliberately dumb sentence splitter — it splits on
// ". ", "! ", "? " and keeps the punctuation. Good enough for English study
// material; will mis-split things like "Mr. Smith" but that's fine for
// translation chunking.
func splitSentences(p string) []string {
	var out []string
	var current strings.Builder
	for i := 0; i < len(p); i++ {
		current.WriteByte(p[i])
		if p[i] == '.' || p[i] == '!' || p[i] == '?' {
			if i+1 < len(p) && (p[i+1] == ' ' || p[i+1] == '\n') {
				out = append(out, strings.TrimSpace(current.String()))
				current.Reset()
			}
		}
	}
	if rest := strings.TrimSpace(current.String()); rest != "" {
		out = append(out, rest)
	}
	if len(out) == 0 {
		return []string{p}
	}
	return out
}
