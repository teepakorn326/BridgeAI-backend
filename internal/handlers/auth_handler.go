package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"studymind-backend/internal/database"
	"studymind-backend/internal/models"
)

type AuthHandler struct {
	db           *database.CacheService // Consider renaming to UserRepository in the future
	jwtSecret    []byte
	googleOAuth  *oauth2.Config
	wechatAppID  string
	wechatSecret string
	frontendURL  string
	httpClient   *http.Client
}

func NewAuthHandler(db *database.CacheService) *AuthHandler {
	frontendURL := os.Getenv("FRONTEND_URL")
	if frontendURL == "" {
		frontendURL = "http://localhost:3000"
	}

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		// Fail fast in production instead of silently using a weak default
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal("[Auth] FATAL: JWT_SECRET environment variable is required in production")
		}
		jwtSecret = "dev-secret-change-in-production"
		log.Println("[Auth] WARNING: using default JWT secret")
	}

	h := &AuthHandler{
		db:          db,
		jwtSecret:   []byte(jwtSecret),
		frontendURL: frontendURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second, // Prevent hung goroutines on external API calls
		},
	}

	googleID := os.Getenv("GOOGLE_CLIENT_ID")
	googleSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if googleID != "" && googleSecret != "" {
		h.googleOAuth = &oauth2.Config{
			ClientID:     googleID,
			ClientSecret: googleSecret,
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     google.Endpoint,
			RedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
		}
		log.Println("[Auth] Google OAuth configured")
	}

	h.wechatAppID = os.Getenv("WECHAT_APP_ID")
	h.wechatSecret = os.Getenv("WECHAT_APP_SECRET")
	if h.wechatAppID != "" {
		log.Println("[Auth] WeChat OAuth configured")
	}

	return h
}

// ── Helpers ──────────────────────────────────────────────────────

// cookieAttrs picks SameSite + Secure based on APP_ENV.
// Production (Vercel frontend + Render backend are cross-site) requires
// SameSite=None + Secure=true or the browser drops the cookie on fetch().
// Local dev keeps Lax to avoid the HTTPS requirement.
func cookieAttrs() (sameSite string, secure bool) {
	if os.Getenv("APP_ENV") == "production" {
		return "None", true
	}
	return "Lax", false
}

func (h *AuthHandler) setJWTCookie(c *fiber.Ctx, token string) {
	sameSite, secure := cookieAttrs()
	c.Cookie(&fiber.Cookie{
		Name:     "jwt",
		Value:    token,
		Expires:  time.Now().Add(7 * 24 * time.Hour),
		HTTPOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Path:     "/",
	})
}

func (h *AuthHandler) clearJWTCookie(c *fiber.Ctx) {
	sameSite, secure := cookieAttrs()
	c.Cookie(&fiber.Cookie{
		Name:     "jwt",
		Value:    "",
		Expires:  time.Now().Add(-1 * time.Hour),
		HTTPOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Path:     "/",
	})
}

func (h *AuthHandler) signToken(user *models.User) (string, error) {
	claims := jwt.MapClaims{
		"sub":   user.ID,
		"email": user.Email,
		"exp":   time.Now().Add(7 * 24 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(h.jwtSecret)
}

// generateCSRFState creates a secure random string and stores the 'next' URL in a cookie
func (h *AuthHandler) generateCSRFState(c *fiber.Ctx, next string) string {
	b := make([]byte, 32)
	rand.Read(b)
	state := base64.URLEncoding.EncodeToString(b)

	// Store state and next URL in an HttpOnly cookie for verification.
	// OAuth's redirect flow is a top-level navigation, so Lax is fine here —
	// but we mirror the JWT cookie's attrs so prod still gets Secure=true.
	sameSite, secure := cookieAttrs()
	cookieVal := fmt.Sprintf("%s|%s", state, next)
	c.Cookie(&fiber.Cookie{
		Name:     "oauth_state",
		Value:    cookieVal,
		Expires:  time.Now().Add(15 * time.Minute),
		HTTPOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})
	return state
}

// verifyCSRFState validates the state and returns the 'next' URL
func (h *AuthHandler) verifyCSRFState(c *fiber.Ctx, state string) (string, error) {
	cookieVal := c.Cookies("oauth_state")
	if cookieVal == "" {
		return "", fmt.Errorf("missing state cookie")
	}

	parts := strings.SplitN(cookieVal, "|", 2)
	if len(parts) != 2 || parts[0] != state {
		return "", fmt.Errorf("invalid state")
	}

	// Clear the cookie after use
	c.Cookie(&fiber.Cookie{
		Name:     "oauth_state",
		Value:    "",
		Expires:  time.Now().Add(-1 * time.Hour),
		HTTPOnly: true,
	})

	return parts[1], nil
}

// redirectWithAuth sets the JWT as an HttpOnly cookie to prevent URL leakage
func (h *AuthHandler) redirectWithAuth(c *fiber.Ctx, user *models.User, next string) error {
	token, err := h.signToken(user)
	if err != nil {
		return h.redirectWithError(c, "token_generation_failed")
	}

	h.setJWTCookie(c, token)

	// Validate 'next' to prevent Open Redirects
	if !strings.HasPrefix(next, "/") {
		next = "/"
	}

	dest := fmt.Sprintf("%s/auth/callback?success=true&next=%s", h.frontendURL, url.QueryEscape(next))
	return c.Redirect(dest, http.StatusTemporaryRedirect)
}

func (h *AuthHandler) redirectWithError(c *fiber.Ctx, errCode string) error {
	dest := fmt.Sprintf("%s/auth/callback?error=%s", h.frontendURL, url.QueryEscape(errCode))
	return c.Redirect(dest, http.StatusTemporaryRedirect)
}

// ── Email / Password ─────────────────────────────────────────────

func (h *AuthHandler) Register(c *fiber.Ctx) error {
	var req models.RegisterRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request body"})
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		return c.Status(400).JSON(fiber.Map{"error": "email and password required"})
	}
	if len(req.Password) < 10 || len(req.Password) > 72 {
		return c.Status(400).JSON(fiber.Map{"error": "password must be between 10 and 72 characters"})
	}

	existing, err := h.db.GetUserByEmail(req.Email)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "internal error"})
	}
	if existing != nil {
		return c.Status(409).JSON(fiber.Map{"error": "email already registered"})
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "internal error"})
	}

	user := &models.User{
		ID:           uuid.New().String(),
		Email:        req.Email,
		FirstName:    req.FirstName,
		LastName:     req.LastName,
		PasswordHash: string(hash),
		Provider:     "password",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	if err := h.db.PutUser(user); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "internal error"})
	}

	token, err := h.signToken(user)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "internal error"})
	}
	h.setJWTCookie(c, token)
	return c.JSON(fiber.Map{"user": user})
}

func (h *AuthHandler) Login(c *fiber.Ctx) error {
	var req models.LoginRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request body"})
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		return c.Status(400).JSON(fiber.Map{"error": "email and password required"})
	}

	user, err := h.db.GetUserByEmail(req.Email)
	if err != nil || user == nil {
		return c.Status(401).JSON(fiber.Map{"error": "invalid email or password"}) // Prevent user enumeration
	}

	if user.PasswordHash == "" {
		return c.Status(401).JSON(fiber.Map{"error": fmt.Sprintf("this account uses %s sign-in", user.Provider)})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "invalid email or password"})
	}

	token, err := h.signToken(user)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "internal error"})
	}
	h.setJWTCookie(c, token)
	return c.JSON(fiber.Map{"user": user})
}

// Logout clears the JWT cookie.
func (h *AuthHandler) Logout(c *fiber.Ctx) error {
	h.clearJWTCookie(c)
	return c.JSON(fiber.Map{"ok": true})
}

// ExtensionToken returns the JWT string so the web page can hand it
// to the Chrome extension (which can't read HttpOnly cookies).
// Protected by cookie auth.
func (h *AuthHandler) ExtensionToken(c *fiber.Ctx) error {
	userID, _ := c.Locals("userID").(string)
	user, err := h.db.GetUserByID(userID)
	if err != nil || user == nil {
		return c.Status(404).JSON(fiber.Map{"error": "user not found"})
	}
	token, err := h.signToken(user)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "internal error"})
	}
	return c.JSON(fiber.Map{"token": token, "user": user})
}

// ── Google OAuth ─────────────────────────────────────────────────

func (h *AuthHandler) GoogleRedirect(c *fiber.Ctx) error {
	if h.googleOAuth == nil {
		return c.Status(501).JSON(fiber.Map{"error": "Google OAuth not configured"})
	}
	next := c.Query("next", "/")
	state := h.generateCSRFState(c, next)

	authURL := h.googleOAuth.AuthCodeURL(state, oauth2.AccessTypeOffline)
	return c.Redirect(authURL, http.StatusTemporaryRedirect)
}

func (h *AuthHandler) GoogleCallback(c *fiber.Ctx) error {
	if h.googleOAuth == nil {
		return h.redirectWithError(c, "oauth_not_configured")
	}

	// 1. Verify CSRF State
	state := c.Query("state")
	next, err := h.verifyCSRFState(c, state)
	if err != nil {
		return h.redirectWithError(c, "invalid_csrf_state")
	}

	code := c.Query("code")
	if code == "" {
		return h.redirectWithError(c, "missing_code")
	}

	// 2. Exchange token with context timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := h.googleOAuth.Exchange(ctx, code)
	if err != nil {
		return h.redirectWithError(c, "google_exchange_failed")
	}

	// 3. Fetch user info
	client := h.googleOAuth.Client(ctx, token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return h.redirectWithError(c, "google_userinfo_failed")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Sub       string `json:"id"`
		Email     string `json:"email"`
		FirstName string `json:"given_name"`
		LastName  string `json:"family_name"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return h.redirectWithError(c, "google_parse_failed")
	}

	// 4. DB Operations
	user, err := h.db.GetUserByGoogleSub(info.Sub)
	if err != nil {
		return h.redirectWithError(c, "internal_error")
	}

	if user == nil {
		existing, _ := h.db.GetUserByEmail(info.Email)
		if existing != nil {
			existing.GoogleSub = info.Sub
			h.db.PutUser(existing)
			user = existing
		} else {
			user = &models.User{
				ID:        uuid.New().String(),
				Email:     info.Email,
				FirstName: info.FirstName,
				LastName:  info.LastName,
				GoogleSub: info.Sub,
				Provider:  "google",
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			h.db.PutUser(user)
		}
	}

	return h.redirectWithAuth(c, user, next)
}

// ── WeChat OAuth ─────────────────────────────────────────────────

func (h *AuthHandler) WechatRedirect(c *fiber.Ctx) error {
	if h.wechatAppID == "" {
		return c.Status(501).JSON(fiber.Map{"error": "WeChat OAuth not configured"})
	}
	next := c.Query("next", "/")
	state := h.generateCSRFState(c, next)

	redirectURI := os.Getenv("WECHAT_REDIRECT_URL")
	authURL := fmt.Sprintf(
		"https://open.weixin.qq.com/connect/qrconnect?appid=%s&redirect_uri=%s&response_type=code&scope=snsapi_login&state=%s#wechat_redirect",
		h.wechatAppID,
		url.QueryEscape(redirectURI),
		url.QueryEscape(state),
	)
	return c.Redirect(authURL, http.StatusTemporaryRedirect)
}

func (h *AuthHandler) WechatCallback(c *fiber.Ctx) error {
	if h.wechatAppID == "" {
		return h.redirectWithError(c, "oauth_not_configured")
	}

	// 1. Verify CSRF State
	state := c.Query("state")
	next, err := h.verifyCSRFState(c, state)
	if err != nil {
		return h.redirectWithError(c, "invalid_csrf_state")
	}

	code := c.Query("code")
	if code == "" {
		return h.redirectWithError(c, "missing_code")
	}

	// 2. Exchange token using internal httpClient with timeout
	tokenURL := fmt.Sprintf(
		"https://api.weixin.qq.com/sns/oauth2/access_token?appid=%s&secret=%s&code=%s&grant_type=authorization_code",
		h.wechatAppID, h.wechatSecret, code,
	)
	resp, err := h.httpClient.Get(tokenURL)
	if err != nil {
		return h.redirectWithError(c, "wechat_exchange_failed")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		OpenID      string `json:"openid"`
		ErrCode     int    `json:"errcode"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil || tokenResp.ErrCode != 0 {
		return h.redirectWithError(c, "wechat_token_error")
	}

	// 3. Get User Info
	infoURL := fmt.Sprintf(
		"https://api.weixin.qq.com/sns/userinfo?access_token=%s&openid=%s",
		tokenResp.AccessToken, tokenResp.OpenID,
	)
	infoResp, err := h.httpClient.Get(infoURL)
	if err != nil {
		return h.redirectWithError(c, "wechat_userinfo_failed")
	}
	defer infoResp.Body.Close()
	infoBody, _ := io.ReadAll(infoResp.Body)

	var info struct {
		OpenID   string `json:"openid"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(infoBody, &info); err != nil {
		return h.redirectWithError(c, "wechat_parse_failed")
	}

	// 4. DB Operations
	user, err := h.db.GetUserByWechatOpenID(info.OpenID)
	if err != nil {
		return h.redirectWithError(c, "internal_error")
	}

	if user == nil {
		user = &models.User{
			ID:           uuid.New().String(),
			FirstName:    info.Nickname,
			WechatOpenID: info.OpenID,
			Provider:     "wechat",
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		h.db.PutUser(user)
	}

	return h.redirectWithAuth(c, user, next)
}

// ── JWT Middleware ────────────────────────────────────────────────

func (h *AuthHandler) RequireAuth(c *fiber.Ctx) error {
	// Support both Authorization header and HttpOnly cookie
	var tokenStr string
	authHeader := c.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
	} else {
		tokenStr = c.Cookies("jwt")
	}

	if tokenStr == "" {
		return c.Status(401).JSON(fiber.Map{"error": "missing authentication token"})
	}

	parsed, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return h.jwtSecret, nil
	})

	if err != nil || !parsed.Valid {
		return c.Status(401).JSON(fiber.Map{"error": "invalid or expired token"})
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return c.Status(401).JSON(fiber.Map{"error": "invalid claims"})
	}

	c.Locals("userID", claims["sub"])
	c.Locals("email", claims["email"])
	return c.Next()
}

func (h *AuthHandler) GetMe(c *fiber.Ctx) error {
	userID, _ := c.Locals("userID").(string)
	user, err := h.db.GetUserByID(userID)
	if err != nil || user == nil {
		return c.Status(404).JSON(fiber.Map{"error": "user not found"})
	}
	return c.JSON(user)
}
