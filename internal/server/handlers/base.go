package handlers

import "net/http"

// Base collects Emby catalog endpoints that we always answer with an empty list.
// Emby clients probe these at various points and bail out if they 404.
type Base struct{}

// NewBase constructs the handler.
func NewBase() *Base { return &Base{} }

// Empty returns {Items:[], TotalRecordCount:0}.
func (b *Base) Empty(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, EmptyItemResponse())
}
