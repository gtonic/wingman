package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/adrianliechti/wingman/pkg/tool"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

var _ tool.Provider = (*Client)(nil)

type Client struct {
	client *client.Client

	serverInfo *mcp.Implementation
}

func NewStdio(command string, env, args []string) (*Client, error) {
	tr := transport.NewStdio(command, env, args...)

	client := client.NewClient(tr)

	return &Client{
		client: client,
	}, nil
}

func NewHttp(url string, headers map[string]string) (*Client, error) {
	var options []transport.StreamableHTTPCOption

	if len(headers) > 0 {
		options = append(options, transport.WithHTTPHeaders(headers))
	}

	tr, err := transport.NewStreamableHTTP(url, options...)

	if err != nil {
		return nil, err
	}

	return &Client{
		client: client.NewClient(tr),
	}, nil
}

func (c *Client) Close() error {
	return c.client.Close()
}

func (c *Client) Tools(ctx context.Context) ([]tool.Tool, error) {
	if err := c.ensureInit(ctx); err != nil {
		return nil, err
	}

	req := mcp.ListToolsRequest{}

	resp, err := c.client.ListTools(ctx, req)

	if err != nil {
		return nil, err
	}

	var result []tool.Tool

	for _, t := range resp.Tools {
		var schema map[string]any

		input, _ := json.Marshal(t.InputSchema)

		if err := json.Unmarshal([]byte(input), &schema); err != nil {
			return nil, err
		}

		if len(t.InputSchema.Properties) == 0 {
			schema = map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			}
		}

		tool := tool.Tool{
			Name:        t.Name,
			Description: t.Description,

			Parameters: schema,
		}

		result = append(result, tool)
	}

	return result, nil
}

func (c *Client) Execute(ctx context.Context, name string, parameters map[string]any) (any, error) {
	if err := c.ensureInit(ctx); err != nil {
		return nil, err
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = parameters

	resp, err := c.client.CallTool(ctx, req)

	if err != nil {
		return nil, err
	}

	if len(resp.Content) > 1 {
		return nil, errors.New("multiple content types not supported")
	}

	for _, content := range resp.Content {
		switch content := content.(type) {
		case mcp.TextContent:
			text := strings.TrimSpace(content.Text)
			return text, nil

		case mcp.ImageContent:
			return nil, errors.New("image content not supported")

		case mcp.EmbeddedResource:
			return nil, errors.New("embedded resource not supported")

		default:
			return nil, errors.New("unknown content type")
		}
	}

	return nil, errors.New("no content returned")
}

func (c *Client) ensureInit(ctx context.Context) error {
	if c.serverInfo != nil {
		return nil
	}

	if err := c.client.Start(ctx); err != nil {
		return err
	}

	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{
		Name:    "wingman",
		Version: "1.0.0",
	}

	resp, err := c.client.Initialize(ctx, req)

	if err != nil {
		return err
	}

	c.serverInfo = &resp.ServerInfo

	return nil
}
