// Command backfill links every existing cached VIDEO# course to a given user.
//
// Usage:
//
//	BACKFILL_EMAIL=you@example.com go run ./cmd/backfill
//
// Pre-change history: ListCachedCourses returned every course ever processed,
// system-wide. After switching the dashboard to a per-user library, older
// entries have no user link. This command assigns them to a single seed user
// so they keep showing up in that user's dashboard.
//
// Safe to re-run: LinkUserToCourse is idempotent (PutItem overwrites).
package main

import (
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"studymind-backend/internal/database"
)

func main() {
	_ = godotenv.Load()

	email := strings.ToLower(strings.TrimSpace(os.Getenv("BACKFILL_EMAIL")))
	if email == "" {
		log.Fatal("[Backfill] BACKFILL_EMAIL is required")
	}

	db, err := database.NewCacheService()
	if err != nil {
		log.Fatalf("[Backfill] DB init: %v", err)
	}

	user, err := db.GetUserByEmail(email)
	if err != nil {
		log.Fatalf("[Backfill] user lookup: %v", err)
	}
	if user == nil {
		log.Fatalf("[Backfill] no user with email %s", email)
	}

	courses, err := db.ListCachedCourses()
	if err != nil {
		log.Fatalf("[Backfill] list courses: %v", err)
	}
	log.Printf("[Backfill] found %d courses; linking to %s (id=%s)", len(courses), user.Email, user.ID)

	ok, fail := 0, 0
	for _, s := range courses {
		if err := db.LinkUserToCourse(user.ID, s.VideoID, s.VideoURL, s.Title, s.TargetLang); err != nil {
			log.Printf("[Backfill] failed %s/%s: %v", s.VideoID, s.TargetLang, err)
			fail++
			continue
		}
		ok++
	}
	log.Printf("[Backfill] done: %d linked, %d failed", ok, fail)
}
