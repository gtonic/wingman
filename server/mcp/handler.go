package mcp

import (
	"net/http"

	"github.com/adrianliechti/wingman/config"

	"github.com/go-chi/chi/v5"
)

type Handler struct {
	*config.Config
}

func New(cfg *config.Config) (*Handler, error) {
	h := &Handler{
		Config: cfg,
	}

	return h, nil
}

func (h *Handler) Attach(r chi.Router) {
	r.HandleFunc("/mcp/{id}", h.handleMCP)
}

func (h *Handler) handleMCP(w http.ResponseWriter, r *http.Request) {
	handler, err := h.MCP(r.PathValue("id"))

	if err != nil {
		http.Error(w, "MCP not found", http.StatusNotFound)
		return
	}

	handler.ServeHTTP(w, r)
}
