package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	mcppkg "github.com/adrianliechti/wingman/pkg/mcp"
	"github.com/adrianliechti/wingman/pkg/tool"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var _ mcppkg.Provider = (*Server)(nil)

type Server struct {
	impl *mcp.Implementation
	opts *mcp.ServerOptions

	tools []tool.Provider

	handler http.Handler
}

func New(name string, tools []tool.Provider) (*Server, error) {
	s := &Server{
		impl: &mcp.Implementation{
			Name: name,
		},

		opts: &mcp.ServerOptions{
			KeepAlive: time.Second * 30,
		},

		tools: tools,
	}

	go s.refresh()

	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.handler == nil {
		http.Error(w, "MCP not ready", http.StatusPreconditionFailed)
		return
	}

	s.handler.ServeHTTP(w, r)
}

func (s *Server) refresh() {
	for {
		srv, err := s.createServer(context.Background())

		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		opts := &mcp.StreamableHTTPOptions{
			Stateless: true,
		}

		h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return srv
		}, opts)

		s.handler = h

		time.Sleep(time.Minute * 5)
	}
}

func (s *Server) createServer(ctx context.Context) (*mcp.Server, error) {
	server := mcp.NewServer(s.impl, s.opts)

	for _, p := range s.tools {
		tool, err := p.Tools(ctx)

		if err != nil {
			return nil, err
		}

		for _, t := range tool {
			data, _ := json.Marshal(t.Parameters)

			schema := new(jsonschema.Schema)

			if err := schema.UnmarshalJSON(data); err != nil {
				return nil, err
			}

			handler := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args map[string]any

				if r, ok := req.Params.Arguments.(json.RawMessage); ok {
					json.Unmarshal(r, &args)
				}

				result, err := p.Execute(ctx, t.Name, args)

				if err != nil {
					return nil, err
				}

				switch v := result.(type) {
				case *mcp.CallToolResult:
					return v, nil

				case string:
					return &mcp.CallToolResult{
						Content: []mcp.Content{
							&mcp.TextContent{
								Text: v,
							},
						},
					}, nil

				default:
					data, _ := json.Marshal(v)

					return &mcp.CallToolResult{
						Content: []mcp.Content{
							&mcp.TextContent{
								Text: string(data),
							},
						},
					}, nil
				}
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
