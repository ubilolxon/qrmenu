package handler

import (
	"net/http"
	"time"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/service"
)

type Table struct {
	svc *service.Table
	cfg config.Public
}

func NewTable(svc *service.Table, cfg config.Public) Table {
	return Table{svc: svc, cfg: cfg}
}

// The response includes the ready-made URLs so the SPA never has to know how a
// menu link or a QR endpoint is spelled.
type tableResponse struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	QRToken   string    `json:"qr_token"`
	MenuURL   string    `json:"menu_url"`
	QRPngURL  string    `json:"qr_png_url"`
	QRSvgURL  string    `json:"qr_svg_url"`
	QRCardURL string    `json:"qr_card_url"`
	Seats     *int32    `json:"seats"`
	SortOrder int32     `json:"sort_order"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

func (h Table) toResponse(t domain.Table) tableResponse {
	menuURL := h.cfg.BaseURL + "/t/" + t.QRToken
	return tableResponse{
		ID:        t.ID.String(),
		Label:     t.Label,
		QRToken:   t.QRToken,
		MenuURL:   menuURL,
		QRPngURL:  menuURL + "/qr.png",
		QRSvgURL:  menuURL + "/qr.svg",
		QRCardURL: menuURL + "/qr.card.svg",
		Seats:     t.Seats,
		SortOrder: t.SortOrder,
		IsActive:  t.IsActive,
		CreatedAt: t.CreatedAt,
	}
}

type tableRequest struct {
	Label     string `json:"label"`
	Seats     *int32 `json:"seats"`
	SortOrder int32  `json:"sort_order"`
}

type tablePatchRequest struct {
	Label      *string `json:"label"`
	Seats      *int32  `json:"seats"`
	ClearSeats bool    `json:"clear_seats"`
	SortOrder  *int32  `json:"sort_order"`
	IsActive   *bool   `json:"is_active"`
}

func (h Table) List(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	tables, err := h.svc.List(r.Context(), tenantID, restaurantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"tables": mapSlice(tables, h.toResponse)})
}

func (h Table) Create(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	var req tableRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	created, err := h.svc.Create(r.Context(), tenantID, restaurantID, domain.TableInput{
		Label: req.Label, Seats: req.Seats, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, h.toResponse(created))
}

func (h Table) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "tableID", "table_id")
	if !ok {
		return
	}
	table, err := h.svc.Get(r.Context(), tenantID, restaurantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, h.toResponse(table))
}

func (h Table) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "tableID", "table_id")
	if !ok {
		return
	}
	var req tablePatchRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	updated, err := h.svc.Update(r.Context(), tenantID, restaurantID, id, service.TablePatch{
		Label: req.Label, Seats: req.Seats, ClearSeats: req.ClearSeats,
		SortOrder: req.SortOrder, IsActive: req.IsActive,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, h.toResponse(updated))
}

// RotateToken invalidates every QR code already printed for this table.
func (h Table) RotateToken(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "tableID", "table_id")
	if !ok {
		return
	}
	rotated, err := h.svc.RotateToken(r.Context(), tenantID, restaurantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, h.toResponse(rotated))
}

func (h Table) Delete(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "tableID", "table_id")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}
