package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/adrianliechti/wingman/config"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Handler struct {
	*config.Config
	http.Handler
}

func New(cfg *config.Config) (*Handler, error) {
	mux := chi.NewMux()

	h := &Handler{
		Config:  cfg,
		Handler: mux,
	}

	h.Attach(mux)
	return h, nil
}

func (h *Handler) Attach(r chi.Router) {
	var server *mcp.Server

	getServer := func(request *http.Request) *mcp.Server {
		if server == nil {
			server, _ = h.createServer()
		}

		return server
	}

	r.Handle("/mcp", mcp.NewStreamableHTTPHandler(getServer, &mcp.StreamableHTTPOptions{}))
}

func (h *Handler) createServer() (*mcp.Server, error) {
	impl := &mcp.Implementation{
		Name:    "wingman",
		Version: "0.1.0",
	}

	opts := &mcp.ServerOptions{
		KeepAlive: time.Second * 30,
	}

	server := mcp.NewServer(impl, opts)

	for _, p := range h.Tools() {
		tool, err := p.Tools(context.Background())

		if err != nil {
			return nil, err
		}

		for _, t := range tool {
			data, _ := json.Marshal(t.Parameters)

			schema := new(jsonschema.Schema)

			if err := schema.UnmarshalJSON(data); err != nil {
				return nil, err
			}

			handler := func(ctx context.Context, session *mcp.ServerSession, rparams *mcp.CallToolParamsFor[map[string]any]) (*mcp.CallToolResult, error) {
				args := rparams.Arguments

				result, err := p.Execute(ctx, t.Name, args)

				if err != nil {
					return &mcp.CallToolResult{
						Content: []mcp.Content{
							&mcp.TextContent{
								Text: err.Error(),
							},
						},

						IsError: true,
					}, nil
				}

				var content string

				switch v := result.(type) {
				case string:
					content = v
				default:
					data, _ := json.Marshal(v)
					content = string(data)
				}

				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{
							Text: content,
						},
					},
				}, nil
			}

			tool := &mcp.Tool{
				Name:        t.Name,
				Description: t.Description,

				InputSchema: schema,
			}

			server.AddTool(tool, handler)
		}
	}

	return server, nil
}
