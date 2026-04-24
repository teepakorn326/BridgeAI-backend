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
	authHandler := handlers.NewAuthHandler(cache)

	// Initialize Fiber
	app := fiber.New(fiber.Config{
		AppName: "StudyMind AI API v1.0",
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
