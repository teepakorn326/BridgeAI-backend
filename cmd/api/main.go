package main

import (
	"log"
	"os"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/joho/godotenv"

	"studymind-backend/internal/database"
	"studymind-backend/internal/handlers"
	"studymind-backend/internal/services"
)

func main() {
	// Load .env file (optional — will use env vars in production)
	if err := godotenv.Load(); err != nil {
		log.Println("[Main] No .env file found, using environment variables")
	}

	// Initialize AWS services
	cache, err := database.NewCacheService()
	if err != nil {
		log.Printf("[Main] WARNING: DynamoDB cache unavailable: %v", err)
		log.Println("[Main] Server will start but caching will fail at runtime")
	}

	bedrock, err := services.NewBedrockService()
	if err != nil {
		log.Fatalf("[Main] FATAL: Cannot initialize Bedrock service: %v", err)
	}

	// Initialize transcript client
	transcript := services.NewTranscriptClient()

	// Create handlers with dependencies
	courseHandler := handlers.NewCourseHandler(cache, bedrock, transcript)
	documentHandler := handlers.NewDocumentHandler(cache, bedrock)
	authHandler := handlers.NewAuthHandler(cache)

	// Initialize Fiber. BodyLimit is bumped above the default 4MB so PDF
	// and audio uploads up to ~150MB go through (Whisper itself caps the
	// usable duration via MAX_DURATION_WHISPER on the transcript service).
	app := fiber.New(fiber.Config{
		AppName:   "StudyMind AI API v1.0",
		BodyLimit: 200 * 1024 * 1024,
	})

	// CORS middleware — allow frontend origin
	allowedOrigins := os.Getenv("CORS_ORIGINS")
	if allowedOrigins == "" {
		allowedOrigins = "http://localhost:3000"
	}

	app.Use(cors.New(cors.Config{
		AllowOrigins: allowedOrigins,
		AllowOriginsFunc: func(origin string) bool {
			return strings.HasPrefix(origin, "chrome-extension://")
		},
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization",
		AllowMethods:     "GET, POST, OPTIONS",
		AllowCredentials: true,
	}))

	// Health check
	app.Get("/api/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":  "healthy",
			"service": "studymind-api",
		})
	})

	// Auth routes
	app.Post("/auth/register", authHandler.Register)
	app.Post("/auth/login", authHandler.Login)
	app.Post("/auth/logout", authHandler.Logout)
	app.Get("/auth/google", authHandler.GoogleRedirect)
	app.Get("/auth/google/callback", authHandler.GoogleCallback)
	app.Get("/auth/wechat", authHandler.WechatRedirect)
	app.Get("/auth/wechat/callback", authHandler.WechatCallback)
	app.Get("/auth/me", authHandler.RequireAuth, authHandler.GetMe)
	app.Post("/auth/extension-token", authHandler.RequireAuth, authHandler.ExtensionToken)

	// API routes (auth required)
	api := app.Group("/api", authHandler.RequireAuth)
	api.Post("/process-course", courseHandler.ProcessCourse)
	api.Post("/fetch-transcript", courseHandler.FetchTranscript)
	api.Post("/ingest-course", courseHandler.IngestCourse)
	api.Get("/course", courseHandler.GetCourse)
	api.Post("/translate-segments", courseHandler.TranslateSegments)
	api.Get("/courses", courseHandler.ListCourses)
	api.Post("/summarize", courseHandler.Summarize)
	api.Post("/quiz", courseHandler.GenerateQuiz)
	api.Post("/vocab", courseHandler.ExtractVocab)
	api.Post("/chat", courseHandler.Chat)
	api.Post("/chapters", courseHandler.GenerateChapters)
	api.Post("/lesson", courseHandler.GenerateLesson)
	api.Post("/upload-audio", courseHandler.UploadAudio)

	// Document pipeline (PDF/DOCX/PPTX/TXT/image) — page-aware, separate
	// from lectures. See backend/internal/handlers/document_handler.go.
	api.Post("/upload-document", documentHandler.UploadDocument)
	api.Get("/document", documentHandler.GetDocument)
	api.Get("/documents", documentHandler.ListDocuments)
	api.Post("/document/summary", documentHandler.SummarizeDocument)
	api.Post("/document/quiz", documentHandler.GenerateDocumentQuiz)
	api.Post("/document/vocab", documentHandler.ExtractDocumentVocab)
	api.Post("/document/lesson", documentHandler.GenerateDocumentLesson)
	api.Post("/document/chat", documentHandler.ChatWithDocument)

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("[Main] 🚀 StudyMind AI API starting on :%s", port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatalf("[Main] Server failed: %v", err)
	}
}
