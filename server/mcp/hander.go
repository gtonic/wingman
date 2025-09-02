package mcp

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/adrianliechti/wingman/config"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Handler struct {
	*config.Config

	mu sync.Mutex

	cache     map[string]*mcp.Server
	cacheTime map[string]time.Time
}

func (h *Handler) getServer(ctx context.Context, id string) (*mcp.Server, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if server, ok := h.cache[id]; ok {
		if t, ok := h.cacheTime[id]; ok {
			fresh := time.Since(t) < time.Minute*5
			empty := len(slices.Collect(server.Sessions())) == 0

			if fresh || !empty {
				return server, nil
			}
		}
	}

	p, err := h.MCP(id)

	if err != nil {
		return nil, err
	}

	s, err := p.Server(ctx)

	if err != nil {
		return nil, err
	}

	h.cache[id] = s
	h.cacheTime[id] = time.Now()

	return s, nil
}

func New(cfg *config.Config) (*Handler, error) {
	h := &Handler{
		Config: cfg,

		cache:     make(map[string]*mcp.Server),
		cacheTime: make(map[string]time.Time),
	}

	return h, nil
}

func (h *Handler) Attach(r chi.Router) {
	r.HandleFunc("/mcp/{id}", h.handleMCP)
}

func (h *Handler) handleMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	getServer := func(request *http.Request) *mcp.Server {
		s, err := h.getServer(request.Context(), id)

		if err != nil {
			return nil
		}

		return s
	}

	handler := mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{
		Stateless: true,
	})

	handler.ServeHTTP(w, r)
}
