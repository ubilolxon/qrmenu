package handler

import (
	"net/http"
	"strings"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/service"
)

// User is staff management: who works here and what they may do.
type User struct {
	svc *service.User
}

func NewUser(svc *service.User) User {
	return User{svc: svc}
}

// ------------------------------------------------------------------ requests

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

type updateUserRequest struct {
	Name *string `json:"name"`
	Role *string `json:"role"`
}

type setActiveRequest struct {
	IsActive bool `json:"is_active"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type resetPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

// ------------------------------------------------------------------ handlers

func (h User) List(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	users, err := h.svc.List(r.Context(), identity.TenantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}

	out := make([]userResponse, len(users))
	for i, u := range users {
		out[i] = toUserResponse(u)
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"users": out})
}

func (h User) Get(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, ok := pathID(w, r, "userID", "user_id")
	if !ok {
		return
	}
	user, err := h.svc.Get(r.Context(), identity.TenantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toUserResponse(user))
}

// Create adds a member of staff. The password is set by whoever creates the
// account and handed over; there is no email invitation, because there is no
// mail infrastructure. They change it with ChangePassword.
func (h User) Create(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	var req createUserRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	created, err := h.svc.Create(r.Context(), identity.TenantID, identity.Role, service.CreateUserInput{
		Email:    req.Email,
		Password: req.Password,
		Name:     req.Name,
		Role:     domain.Role(strings.TrimSpace(req.Role)),
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toUserResponse(created))
}

func (h User) Update(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, ok := pathID(w, r, "userID", "user_id")
	if !ok {
		return
	}

	var req updateUserRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	patch := service.UserPatch{Name: req.Name}
	if req.Role != nil {
		role := domain.Role(strings.TrimSpace(*req.Role))
		patch.Role = &role
	}

	updated, err := h.svc.Update(r.Context(), identity.TenantID, identity.UserID, id, identity.Role, patch)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toUserResponse(updated))
}

// SetActive is how someone stops working here. Deactivating rather than
// deleting keeps their name on the orders they handled.
func (h User) SetActive(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, ok := pathID(w, r, "userID", "user_id")
	if !ok {
		return
	}

	var req setActiveRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	updated, err := h.svc.SetActive(r.Context(), identity.TenantID, identity.UserID, id, identity.Role, req.IsActive)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toUserResponse(updated))
}

func (h User) Delete(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, ok := pathID(w, r, "userID", "user_id")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), identity.TenantID, identity.UserID, id, identity.Role); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// ChangePassword replaces the caller's own password and ends every other
// session they hold.
func (h User) ChangePassword(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	var req changePasswordRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	if err := h.svc.ChangePassword(r.Context(), identity.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// ResetPassword is the forgotten-password path, performed by an owner. With no
// mail infrastructure the alternative is an account nobody can get back into.
func (h User) ResetPassword(w http.ResponseWriter, r *http.Request) {
	identity := middleware.MustIdentity(r.Context())

	id, ok := pathID(w, r, "userID", "user_id")
	if !ok {
		return
	}

	var req resetPasswordRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	if err := h.svc.ResetPassword(r.Context(), identity.TenantID, identity.UserID, id, identity.Role, req.NewPassword); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}
