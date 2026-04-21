// Command seed creates or updates the primary demo account in DynamoDB.
//
// Usage:
//
//	SEED_EMAIL=you@example.com SEED_PASSWORD=secret go run ./cmd/seed
//
// Required env: SEED_EMAIL, SEED_PASSWORD. Optional: SEED_FIRST_NAME, SEED_LAST_NAME.
// AWS credentials are read from the default chain (env, profile, or IAM role).
package main

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"

	"studymind-backend/internal/database"
	"studymind-backend/internal/models"
)

func main() {
	_ = godotenv.Load()

	email := strings.ToLower(strings.TrimSpace(os.Getenv("SEED_EMAIL")))
	password := os.Getenv("SEED_PASSWORD")
	firstName := os.Getenv("SEED_FIRST_NAME")
	lastName := os.Getenv("SEED_LAST_NAME")

	if email == "" || password == "" {
		log.Fatal("[Seed] SEED_EMAIL and SEED_PASSWORD are required")
	}
	if len(password) < 10 || len(password) > 72 {
		log.Fatal("[Seed] SEED_PASSWORD must be 10–72 characters")
	}

	db, err := database.NewCacheService()
	if err != nil {
		log.Fatalf("[Seed] DynamoDB init failed: %v", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("[Seed] bcrypt failed: %v", err)
	}

	existing, err := db.GetUserByEmail(email)
	if err != nil {
		log.Fatalf("[Seed] lookup failed: %v", err)
	}

	var user *models.User
	if existing != nil {
		existing.PasswordHash = string(hash)
		if firstName != "" {
			existing.FirstName = firstName
		}
		if lastName != "" {
			existing.LastName = lastName
		}
		if existing.Provider == "" {
			existing.Provider = "password"
		}
		user = existing
		log.Printf("[Seed] Updating existing user %s (id=%s)", email, user.ID)
	} else {
		user = &models.User{
			ID:           uuid.New().String(),
			Email:        email,
			FirstName:    firstName,
			LastName:     lastName,
			PasswordHash: string(hash),
			Provider:     "password",
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		log.Printf("[Seed] Creating new user %s (id=%s)", email, user.ID)
	}

	if err := db.PutUser(user); err != nil {
		log.Fatalf("[Seed] PutUser failed: %v", err)
	}
	log.Printf("[Seed] Done. User ID: %s", user.ID)
}
