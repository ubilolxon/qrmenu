package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/http/middleware"
	"github.com/ubilolxon/qrmenu/internal/httperr"
	"github.com/ubilolxon/qrmenu/internal/i18n"
	"github.com/ubilolxon/qrmenu/internal/service"
)

type Menu struct {
	svc *service.Menu
}

func NewMenu(svc *service.Menu) Menu {
	return Menu{svc: svc}
}

// scope pulls the two values every handler in this file needs: who is calling
// and which restaurant they named. The tenant comes from the verified token;
// the restaurant is only a path parameter, which is why the service still
// checks that the tenant owns it.
func scope(w http.ResponseWriter, r *http.Request) (tenantID, restaurantID uuid.UUID, ok bool) {
	identity := middleware.MustIdentity(r.Context())
	restaurantID, err := PathUUID(r, "restaurant_id", chi.URLParam(r, "restaurantID"))
	if err != nil {
		RenderError(w, r, err)
		return uuid.Nil, uuid.Nil, false
	}
	return identity.TenantID, restaurantID, true
}

// pathID reads a child id from the URL.
func pathID(w http.ResponseWriter, r *http.Request, param, field string) (uuid.UUID, bool) {
	id, err := PathUUID(r, field, chi.URLParam(r, param))
	if err != nil {
		RenderError(w, r, err)
		return uuid.Nil, false
	}
	return id, true
}

// ---------------------------------------------------------------- responses

type menuResponse struct {
	ID               string    `json:"id"`
	Name             i18n.Text `json:"name"`
	Description      i18n.Text `json:"description"`
	IsActive         bool      `json:"is_active"`
	SortOrder        int32     `json:"sort_order"`
	AvailableFromMin *int32    `json:"available_from_min"`
	AvailableToMin   *int32    `json:"available_to_min"`
	Days             []int16   `json:"days"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func toMenuResponse(m domain.Menu) menuResponse {
	days := m.Days
	if days == nil {
		days = []int16{}
	}
	return menuResponse{
		ID: m.ID.String(), Name: m.Name, Description: m.Description,
		IsActive: m.IsActive, SortOrder: m.SortOrder,
		AvailableFromMin: m.AvailableFromMin, AvailableToMin: m.AvailableToMin,
		Days: days, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

type categoryResponse struct {
	ID          string    `json:"id"`
	MenuID      string    `json:"menu_id"`
	Name        i18n.Text `json:"name"`
	Description i18n.Text `json:"description"`
	IsActive    bool      `json:"is_active"`
	SortOrder   int32     `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toCategoryResponse(c domain.Category) categoryResponse {
	return categoryResponse{
		ID: c.ID.String(), MenuID: c.MenuID.String(),
		Name: c.Name, Description: c.Description,
		IsActive: c.IsActive, SortOrder: c.SortOrder,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// PriceMinor is an integer count of minor units. The SPA formats it using the
// restaurant's currency; sending a pre-formatted string here would make the
// value useless for arithmetic.
type itemResponse struct {
	ID          string    `json:"id"`
	CategoryID  string    `json:"category_id"`
	Name        i18n.Text `json:"name"`
	Description i18n.Text `json:"description"`
	PriceMinor  int64     `json:"price_minor"`
	IsAvailable bool      `json:"is_available"`
	SortOrder   int32     `json:"sort_order"`
	Tags        []string  `json:"tags"`
	Allergens   []string  `json:"allergens"`
	Calories    *int32    `json:"calories"`
	PrepMinutes *int32    `json:"prep_minutes"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toItemResponse(i domain.Item) itemResponse {
	return itemResponse{
		ID: i.ID.String(), CategoryID: i.CategoryID.String(),
		Name: i.Name, Description: i.Description,
		PriceMinor: i.PriceMinor, IsAvailable: i.IsAvailable, SortOrder: i.SortOrder,
		Tags: orEmpty(i.Tags), Allergens: orEmpty(i.Allergens),
		Calories: i.Calories, PrepMinutes: i.PrepMinutes,
		CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
	}
}

type variantResponse struct {
	ID              string    `json:"id"`
	ItemID          string    `json:"item_id"`
	Name            i18n.Text `json:"name"`
	PriceDeltaMinor int64     `json:"price_delta_minor"`
	IsDefault       bool      `json:"is_default"`
	IsAvailable     bool      `json:"is_available"`
	SortOrder       int32     `json:"sort_order"`
}

func toVariantResponse(v domain.Variant) variantResponse {
	return variantResponse{
		ID: v.ID.String(), ItemID: v.ItemID.String(), Name: v.Name,
		PriceDeltaMinor: v.PriceDeltaMinor, IsDefault: v.IsDefault,
		IsAvailable: v.IsAvailable, SortOrder: v.SortOrder,
	}
}

type modifierGroupResponse struct {
	ID        string    `json:"id"`
	ItemID    string    `json:"item_id"`
	Name      i18n.Text `json:"name"`
	MinSelect int32     `json:"min_select"`
	MaxSelect int32     `json:"max_select"`
	SortOrder int32     `json:"sort_order"`
}

func toModifierGroupResponse(g domain.ModifierGroup) modifierGroupResponse {
	return modifierGroupResponse{
		ID: g.ID.String(), ItemID: g.ItemID.String(), Name: g.Name,
		MinSelect: g.MinSelect, MaxSelect: g.MaxSelect, SortOrder: g.SortOrder,
	}
}

type modifierResponse struct {
	ID              string    `json:"id"`
	GroupID         string    `json:"group_id"`
	Name            i18n.Text `json:"name"`
	PriceDeltaMinor int64     `json:"price_delta_minor"`
	IsAvailable     bool      `json:"is_available"`
	SortOrder       int32     `json:"sort_order"`
}

func toModifierResponse(m domain.Modifier) modifierResponse {
	return modifierResponse{
		ID: m.ID.String(), GroupID: m.GroupID.String(), Name: m.Name,
		PriceDeltaMinor: m.PriceDeltaMinor, IsAvailable: m.IsAvailable,
		SortOrder: m.SortOrder,
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// mapSlice keeps the list handlers to one line each and guarantees [] rather
// than null for an empty collection.
func mapSlice[T any, R any](in []T, fn func(T) R) []R {
	out := make([]R, len(in))
	for i, v := range in {
		out[i] = fn(v)
	}
	return out
}

// ---------------------------------------------------------------- requests

type menuRequest struct {
	Name             i18n.Text `json:"name"`
	Description      i18n.Text `json:"description"`
	SortOrder        int32     `json:"sort_order"`
	AvailableFromMin *int32    `json:"available_from_min"`
	AvailableToMin   *int32    `json:"available_to_min"`
	Days             []int16   `json:"days"`
}

type menuPatchRequest struct {
	Name             *i18n.Text `json:"name"`
	Description      *i18n.Text `json:"description"`
	IsActive         *bool      `json:"is_active"`
	SortOrder        *int32     `json:"sort_order"`
	AvailableFromMin *int32     `json:"available_from_min"`
	AvailableToMin   *int32     `json:"available_to_min"`
	ClearWindow      bool       `json:"clear_window"`
	Days             *[]int16   `json:"days"`
}

// menu_id and category_id are not fields here: they come from the URL. Leaving
// them in the body would let a client send one value in the path and another in
// the payload, and quietly ignoring the mismatch is how data ends up in the
// wrong place.
type categoryRequest struct {
	Name        i18n.Text `json:"name"`
	Description i18n.Text `json:"description"`
	SortOrder   int32     `json:"sort_order"`
}

type categoryPatchRequest struct {
	MenuID      *string    `json:"menu_id"`
	Name        *i18n.Text `json:"name"`
	Description *i18n.Text `json:"description"`
	IsActive    *bool      `json:"is_active"`
	SortOrder   *int32     `json:"sort_order"`
}

type itemRequest struct {
	Name        i18n.Text `json:"name"`
	Description i18n.Text `json:"description"`
	PriceMinor  int64     `json:"price_minor"`
	SortOrder   int32     `json:"sort_order"`
	Tags        []string  `json:"tags"`
	Allergens   []string  `json:"allergens"`
	Calories    *int32    `json:"calories"`
	PrepMinutes *int32    `json:"prep_minutes"`
}

type itemPatchRequest struct {
	CategoryID       *string    `json:"category_id"`
	Name             *i18n.Text `json:"name"`
	Description      *i18n.Text `json:"description"`
	PriceMinor       *int64     `json:"price_minor"`
	IsAvailable      *bool      `json:"is_available"`
	SortOrder        *int32     `json:"sort_order"`
	Tags             *[]string  `json:"tags"`
	Allergens        *[]string  `json:"allergens"`
	Calories         *int32     `json:"calories"`
	ClearCalories    bool       `json:"clear_calories"`
	PrepMinutes      *int32     `json:"prep_minutes"`
	ClearPrepMinutes bool       `json:"clear_prep_minutes"`
}

type variantRequest struct {
	Name            i18n.Text `json:"name"`
	PriceDeltaMinor int64     `json:"price_delta_minor"`
	IsDefault       bool      `json:"is_default"`
	SortOrder       int32     `json:"sort_order"`
}

type variantPatchRequest struct {
	Name            *i18n.Text `json:"name"`
	PriceDeltaMinor *int64     `json:"price_delta_minor"`
	IsDefault       *bool      `json:"is_default"`
	IsAvailable     *bool      `json:"is_available"`
	SortOrder       *int32     `json:"sort_order"`
}

type modifierGroupRequest struct {
	Name      i18n.Text `json:"name"`
	MinSelect int32     `json:"min_select"`
	MaxSelect int32     `json:"max_select"`
	SortOrder int32     `json:"sort_order"`
}

type modifierGroupPatchRequest struct {
	Name      *i18n.Text `json:"name"`
	MinSelect *int32     `json:"min_select"`
	MaxSelect *int32     `json:"max_select"`
	SortOrder *int32     `json:"sort_order"`
}

type modifierRequest struct {
	Name            i18n.Text `json:"name"`
	PriceDeltaMinor int64     `json:"price_delta_minor"`
	SortOrder       int32     `json:"sort_order"`
}

type modifierPatchRequest struct {
	Name            *i18n.Text `json:"name"`
	PriceDeltaMinor *int64     `json:"price_delta_minor"`
	IsAvailable     *bool      `json:"is_available"`
	SortOrder       *int32     `json:"sort_order"`
}

// reorderRequest carries ids in their new order. Position in the array is the
// new sort_order, so there is no second array to keep in sync.
type reorderRequest struct {
	IDs []string `json:"ids"`
}

type availabilityRequest struct {
	IsAvailable bool `json:"is_available"`
}

// parseIDs converts the reorder payload, reporting which entry was malformed
// rather than a bare "invalid uuid".
func parseIDs(raw []string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(raw))
	var v domain.Validator
	for i, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			v.Add("ids", "entry %d (%q) is not a valid UUID", i, s)
			continue
		}
		ids = append(ids, id)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ---------------------------------------------------------------- menus

func (h Menu) ListMenus(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	menus, err := h.svc.ListMenus(r.Context(), tenantID, restaurantID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"menus": mapSlice(menus, toMenuResponse)})
}

func (h Menu) CreateMenu(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	var req menuRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	created, err := h.svc.CreateMenu(r.Context(), tenantID, restaurantID, domain.MenuInput{
		Name: req.Name, Description: req.Description, SortOrder: req.SortOrder,
		AvailableFromMin: req.AvailableFromMin, AvailableToMin: req.AvailableToMin,
		Days: req.Days,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toMenuResponse(created))
}

func (h Menu) GetMenu(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "menuID", "menu_id")
	if !ok {
		return
	}
	menu, err := h.svc.GetMenu(r.Context(), tenantID, restaurantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toMenuResponse(menu))
}

func (h Menu) UpdateMenu(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "menuID", "menu_id")
	if !ok {
		return
	}
	var req menuPatchRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	updated, err := h.svc.UpdateMenu(r.Context(), tenantID, restaurantID, id, service.MenuPatch{
		Name: req.Name, Description: req.Description, IsActive: req.IsActive,
		SortOrder: req.SortOrder, AvailableFromMin: req.AvailableFromMin,
		AvailableToMin: req.AvailableToMin, ClearWindow: req.ClearWindow, Days: req.Days,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toMenuResponse(updated))
}

func (h Menu) ReorderMenus(w http.ResponseWriter, r *http.Request) {
	h.reorder(w, r, h.svc.ReorderMenus)
}

func (h Menu) DeleteMenu(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "menuID", "menu_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteMenu(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// reorder is shared by menus, categories and items: the payload and the tenant
// handling are identical, only the service call differs.
func (h Menu) reorder(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, tenantID, restaurantID uuid.UUID, ids []uuid.UUID) error) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	var req reorderRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	ids, err := parseIDs(req.IDs)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	if err := fn(r.Context(), tenantID, restaurantID, ids); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// ---------------------------------------------------------------- categories

func (h Menu) ListCategories(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	menuID, ok := pathID(w, r, "menuID", "menu_id")
	if !ok {
		return
	}
	categories, err := h.svc.ListCategories(r.Context(), tenantID, restaurantID, menuID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"categories": mapSlice(categories, toCategoryResponse)})
}

func (h Menu) CreateCategory(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	menuID, ok := pathID(w, r, "menuID", "menu_id")
	if !ok {
		return
	}
	var req categoryRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	created, err := h.svc.CreateCategory(r.Context(), tenantID, restaurantID, domain.CategoryInput{
		MenuID: menuID, Name: req.Name, Description: req.Description, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toCategoryResponse(created))
}

func (h Menu) GetCategory(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "categoryID", "category_id")
	if !ok {
		return
	}
	category, err := h.svc.GetCategory(r.Context(), tenantID, restaurantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toCategoryResponse(category))
}

func (h Menu) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "categoryID", "category_id")
	if !ok {
		return
	}
	var req categoryPatchRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	patch := service.CategoryPatch{
		Name: req.Name, Description: req.Description,
		IsActive: req.IsActive, SortOrder: req.SortOrder,
	}
	if req.MenuID != nil {
		menuID, err := uuid.Parse(*req.MenuID)
		if err != nil {
			RenderError(w, r, domain.Errorf(errBadRequest, "menu_id must be a valid UUID."))
			return
		}
		patch.MenuID = &menuID
	}

	updated, err := h.svc.UpdateCategory(r.Context(), tenantID, restaurantID, id, patch)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toCategoryResponse(updated))
}

func (h Menu) ReorderCategories(w http.ResponseWriter, r *http.Request) {
	h.reorder(w, r, h.svc.ReorderCategories)
}

func (h Menu) DeleteCategory(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "categoryID", "category_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteCategory(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// ---------------------------------------------------------------- items

func (h Menu) ListItems(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	categoryID, ok := pathID(w, r, "categoryID", "category_id")
	if !ok {
		return
	}
	items, err := h.svc.ListItems(r.Context(), tenantID, restaurantID, categoryID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"items": mapSlice(items, toItemResponse)})
}

func (h Menu) CreateItem(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	categoryID, ok := pathID(w, r, "categoryID", "category_id")
	if !ok {
		return
	}
	var req itemRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	created, err := h.svc.CreateItem(r.Context(), tenantID, restaurantID, domain.ItemInput{
		CategoryID: categoryID, Name: req.Name, Description: req.Description,
		PriceMinor: req.PriceMinor, SortOrder: req.SortOrder,
		Tags: req.Tags, Allergens: req.Allergens,
		Calories: req.Calories, PrepMinutes: req.PrepMinutes,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toItemResponse(created))
}

func (h Menu) GetItem(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	item, err := h.svc.GetItem(r.Context(), tenantID, restaurantID, id)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toItemResponse(item))
}

func (h Menu) UpdateItem(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	var req itemPatchRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}

	patch := service.ItemPatch{
		Name: req.Name, Description: req.Description, PriceMinor: req.PriceMinor,
		IsAvailable: req.IsAvailable, SortOrder: req.SortOrder,
		Tags: req.Tags, Allergens: req.Allergens,
		Calories: req.Calories, ClearCalories: req.ClearCalories,
		PrepMinutes: req.PrepMinutes, ClearPrepMinutes: req.ClearPrepMinutes,
	}
	if req.CategoryID != nil {
		categoryID, err := uuid.Parse(*req.CategoryID)
		if err != nil {
			RenderError(w, r, domain.Errorf(errBadRequest, "category_id must be a valid UUID."))
			return
		}
		patch.CategoryID = &categoryID
	}

	updated, err := h.svc.UpdateItem(r.Context(), tenantID, restaurantID, id, patch)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toItemResponse(updated))
}

// SetAvailability is the mid-service "we ran out" toggle, which is why kitchen
// staff can call it even though they cannot edit the rest of the menu.
func (h Menu) SetAvailability(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	var req availabilityRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	updated, err := h.svc.SetItemAvailability(r.Context(), tenantID, restaurantID, id, req.IsAvailable)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toItemResponse(updated))
}

func (h Menu) ReorderItems(w http.ResponseWriter, r *http.Request) {
	h.reorder(w, r, h.svc.ReorderItems)
}

func (h Menu) DeleteItem(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteItem(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// ---------------------------------------------------------------- variants

func (h Menu) ListVariants(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	variants, err := h.svc.ListVariants(r.Context(), tenantID, restaurantID, itemID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"variants": mapSlice(variants, toVariantResponse)})
}

func (h Menu) CreateVariant(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	var req variantRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	created, err := h.svc.CreateVariant(r.Context(), tenantID, restaurantID, domain.VariantInput{
		ItemID: itemID, Name: req.Name, PriceDeltaMinor: req.PriceDeltaMinor,
		IsDefault: req.IsDefault, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toVariantResponse(created))
}

func (h Menu) UpdateVariant(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "variantID", "variant_id")
	if !ok {
		return
	}
	var req variantPatchRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	updated, err := h.svc.UpdateVariant(r.Context(), tenantID, restaurantID, id, service.VariantPatch{
		Name: req.Name, PriceDeltaMinor: req.PriceDeltaMinor,
		IsDefault: req.IsDefault, IsAvailable: req.IsAvailable, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toVariantResponse(updated))
}

func (h Menu) DeleteVariant(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "variantID", "variant_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteVariant(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

// ---------------------------------------------------------------- modifiers

func (h Menu) ListModifierGroups(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	groups, err := h.svc.ListModifierGroups(r.Context(), tenantID, restaurantID, itemID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"modifier_groups": mapSlice(groups, toModifierGroupResponse)})
}

func (h Menu) CreateModifierGroup(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	itemID, ok := pathID(w, r, "itemID", "item_id")
	if !ok {
		return
	}
	var req modifierGroupRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	if req.MaxSelect == 0 {
		req.MaxSelect = 1
	}
	created, err := h.svc.CreateModifierGroup(r.Context(), tenantID, restaurantID, domain.ModifierGroupInput{
		ItemID: itemID, Name: req.Name, MinSelect: req.MinSelect,
		MaxSelect: req.MaxSelect, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toModifierGroupResponse(created))
}

func (h Menu) UpdateModifierGroup(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "groupID", "group_id")
	if !ok {
		return
	}
	var req modifierGroupPatchRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	updated, err := h.svc.UpdateModifierGroup(r.Context(), tenantID, restaurantID, id, service.ModifierGroupPatch{
		Name: req.Name, MinSelect: req.MinSelect, MaxSelect: req.MaxSelect, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toModifierGroupResponse(updated))
}

func (h Menu) DeleteModifierGroup(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "groupID", "group_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteModifierGroup(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}

func (h Menu) ListModifiers(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	groupID, ok := pathID(w, r, "groupID", "group_id")
	if !ok {
		return
	}
	modifiers, err := h.svc.ListModifiers(r.Context(), tenantID, restaurantID, groupID)
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, map[string]any{"modifiers": mapSlice(modifiers, toModifierResponse)})
}

func (h Menu) CreateModifier(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	groupID, ok := pathID(w, r, "groupID", "group_id")
	if !ok {
		return
	}
	var req modifierRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	created, err := h.svc.CreateModifier(r.Context(), tenantID, restaurantID, domain.ModifierInput{
		GroupID: groupID, Name: req.Name,
		PriceDeltaMinor: req.PriceDeltaMinor, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusCreated, toModifierResponse(created))
}

func (h Menu) UpdateModifier(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "modifierID", "modifier_id")
	if !ok {
		return
	}
	var req modifierPatchRequest
	if err := DecodeJSON(w, r, &req); err != nil {
		RenderError(w, r, err)
		return
	}
	updated, err := h.svc.UpdateModifier(r.Context(), tenantID, restaurantID, id, service.ModifierPatch{
		Name: req.Name, PriceDeltaMinor: req.PriceDeltaMinor,
		IsAvailable: req.IsAvailable, SortOrder: req.SortOrder,
	})
	if err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.JSON(w, http.StatusOK, toModifierResponse(updated))
}

func (h Menu) DeleteModifier(w http.ResponseWriter, r *http.Request) {
	tenantID, restaurantID, ok := scope(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "modifierID", "modifier_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteModifier(r.Context(), tenantID, restaurantID, id); err != nil {
		RenderError(w, r, err)
		return
	}
	httperr.NoContent(w)
}
