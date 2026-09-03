package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/ratelimit"
	"github.com/ubilolxon/qrmenu/internal/service"
)

type Auth struct {
	svc     *service.Auth
	cfg     config.Auth
	limiter *ratelimit.Limiter
}

// limiter may be nil, which means no limiting — the same shape the guest
// ordering handler uses.
func NewAuth(svc *service.Auth, cfg config.Auth, limiter *ratelimit.Limiter) Auth {
	return Auth{svc: svc, cfg: cfg, limiter: limiter}
}

type registerRequest struct {
	TenantName string `json:"tenant_name"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Password   string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// sessionResponse deliberately omits the refresh token: it travels only in an
// HttpOnly cookie, so JavaScript — including anything injected by an XSS — can
// never read it.
type sessionResponse struct {
	AccessToken string       `json:"access_token"`
	TokenType   string       `json:"token_type"`
	ExpiresAt   time.Time    `json:"expires_at"`
	User        userResponse `json:"user"`
}

type userResponse struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	IsActive    bool       `json:"is_active"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

func toUserResponse(u domain.User) userResponse {
	return userResponse{
		ID:          u.ID.String(),
		Email:       u.Email,
		Name:        u.Name,
		Role:        string(u.Role),
		IsActive:    u.IsActive,
		LastLoginAt: u.LastLoginAt,
		CreatedAt:   u.CreatedAt,
	}
}

// Register creates a tenant with its first owner and logs them straight in.
func (h Auth) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	session, err := h.svc.Register(r.Context(), service.RegisterInput{
		TenantName: req.TenantName,
		Email:      req.Email,
		Password:   req.Password,
		Name:       req.Name,
	}, sessionMeta(r))
	if err != nil {
		RenderError(w, r, err)
		return
	}

	h.writeSession(w, session, http.StatusCreated)
}

func (h Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	// Two keys, and the pair is the point: per IP alone does nothing against
	// credential stuffing from a botnet, and per email alone does nothing
	// against one host walking a list of addresses.
	//
	// The email is normalised first so "A@B.com" and "a@b.com" share a budget,
	// and hashed so a Redis dump is not a list of who has an account here.
	keys := []struct {
		key   string
		limit int
	}{
		{"login:ip:" + middleware.ClientIP(r), ratelimit.LoginPerIPPerMinute},
		{"login:email:" + hashKey(domain.NormalizeEmail(req.Email)), ratelimit.LoginPerEmailPerMinute},
	}

	for _, k := range keys {
		if result := h.limiter.Peek(r.Context(), k.key, k.limit); !result.Allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds()+0.999)))
			httperr.Write(w, http.StatusTooManyRequests, httperr.CodeRateLimited,
				"Too many sign-in attempts. Please wait a moment and try again.", nil)
			return
		}
	}

	session, err := h.svc.Login(r.Context(), req.Email, req.Password, sessionMeta(r))
	if err != nil {
		// Only failures are counted. Charging a successful sign-in against the
		// budget would mean a user logging in on their phone, their laptop and
		// then their phone again has spent part of an allowance meant for an
		// attacker.
		for _, k := range keys {
			h.limiter.Hit(r.Context(), k.key)
		}
		RenderError(w, r, err)
		return
	}

	h.writeSession(w, session, http.StatusOK)
}

// hashKey keeps an email address out of Redis. The limiter only ever needs to
// know that two attempts targeted the same account, not which account, and a
// cache dump should not double as a list of registered addresses.
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// Refresh exchanges the refresh cookie for a new access token and a rotated
// refresh cookie. It takes no request body — the credential is the cookie.
func (h Auth) Refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(h.cfg.CookieName)
	if err != nil {
		httperr.Write(w, http.StatusUnauthorized, httperr.CodeUnauthorized,
			"No active session.", nil)
		return
	}

	session, err := h.svc.Refresh(r.Context(), cookie.Value, sessionMeta(r))
	if err != nil {
		// The old cookie is now worthless either way: it was consumed, revoked,
		// or never valid. Clearing it stops the SPA from retrying forever.
		h.clearRefreshCookie(w)
		RenderError(w, r, err)
		return
	}

	h.writeSession(w, session, http.StatusOK)
}

func (h Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(h.cfg.CookieName); err == nil {
		if err := h.svc.Logout(r.Context(), cookie.Value); err != nil {
			RenderError(w, r, err)
			return
		}
	}
	// Always clear, even when there was no cookie, so the response is identical
	// whether or not a session existed.
	h.clearRefreshCookie(w)
	httperr.NoContent(w)
}

// Me returns the authenticated user, scoped to their tenant.
func (h Auth) Me(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	user, err := h.svc.CurrentUser(r.Context(), identity.TenantID, identity.UserID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toUserResponse(user))
}

func (h Auth) writeSession(w http.ResponseWriter, session *service.Session, status int) {
	h.setRefreshCookie(w, session.RefreshToken, session.RefreshExpiresAt)
	httperr.JSON(w, status, sessionResponse{
		AccessToken: session.AccessToken,
		TokenType:   "Bearer",
		ExpiresAt:   session.AccessExpiresAt,
		User:        toUserResponse(session.User),
	})
}

// setRefreshCookie writes the rotating refresh credential.
//
// HttpOnly keeps it away from JavaScript. SameSite=Strict means a request
// triggered by another site never carries it, which is what makes CSRF against
// the refresh endpoint a non-issue. Path scopes it to the auth routes, so it is
// not attached to ordinary API calls that have no use for it.
func (h Auth) setRefreshCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CookieName,
		Value:    token,
		Path:     h.cfg.CookiePath,
		Domain:   h.cfg.CookieDomain,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (h Auth) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CookieName,
		Value:    "",
		Path:     h.cfg.CookiePath,
		Domain:   h.cfg.CookieDomain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func sessionMeta(r *http.Request) service.SessionMeta {
	return service.SessionMeta{
		UserAgent: truncate(r.UserAgent(), 400),
		IP:        middleware.ClientIP(r),
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
