package mcp

import (
	"context"
	"crypto/tls"
	"net/http"
	"os/exec"
	"time"

	"github.com/adrianliechti/wingman/pkg/tool"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var _ tool.Provider = (*Client)(nil)

type Client struct {
	transportFn func() (mcp.Transport, error)
}

func NewCommand(command string, env, args []string) (*Client, error) {
	return &Client{
		transportFn: func() (mcp.Transport, error) {
			cmd := exec.Command(command, args...)
			return mcp.NewCommandTransport(cmd), nil
		},
	}, nil
}

func NewStreamable(url string, headers map[string]string) (*Client, error) {
	client := &http.Client{
		Transport: &rt{
			headers: headers,
		},
	}

	return &Client{
		transportFn: func() (mcp.Transport, error) {
			return mcp.NewStreamableClientTransport(url, &mcp.StreamableClientTransportOptions{
				HTTPClient: client,
			}), nil
		},
	}, nil
}

func NewSSE(url string, headers map[string]string) (*Client, error) {
	client := &http.Client{
		Transport: &rt{
			headers: headers,
		},
	}

	return &Client{
		transportFn: func() (mcp.Transport, error) {
			return mcp.NewSSEClientTransport(url, &mcp.SSEClientTransportOptions{
				HTTPClient: client,
			}), nil
		},
	}, nil
}

func (c *Client) createSession(ctx context.Context) (*mcp.ClientSession, error) {
	transport, err := c.transportFn()
	if err != nil {
		return nil, err
	}

	impl := &mcp.Implementation{
		Name:    "wingman",
		Version: "1.0.0",
	}

	opts := &mcp.ClientOptions{
		KeepAlive: time.Second * 30,
	}

	client := mcp.NewClient(impl, opts)
	return client.Connect(ctx, transport, nil)
}

func (c *Client) Tools(ctx context.Context) ([]tool.Tool, error) {
	session, err := c.createSession(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	resp, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}

	var result []tool.Tool
	for _, t := range resp.Tools {
		input, _ := t.InputSchema.MarshalJSON()

		tool := tool.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  tool.ParseNormalizedSchema(input),
		}
		result = append(result, tool)
	}

	return result, nil
}

func (c *Client) Execute(ctx context.Context, name string, parameters map[string]any) (any, error) {
	session, err := c.createSession(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: parameters,
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

type rt struct {
	headers map[string]string
}

func (rt *rt) RoundTrip(req *http.Request) (*http.Response, error) {
	for key, value := range rt.headers {
		if req.Header.Get(key) != "" {
			continue // already set
		}
		req.Header.Set(key, value)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, // TODO: make configurable
	}

	return tr.RoundTrip(req)
}
