package proxy

import (
	"net/http"
	"net/http/httputil"
	neturl "net/url"

	"github.com/adrianliechti/wingman/pkg/mcp"
)

var _ mcp.Provider = (*Server)(nil)

type Server struct {
	url *neturl.URL
}

func New(url string) (*Server, error) {
	u, err := neturl.Parse(url)

	if err != nil {
		return nil, err
	}

	s := &Server{
		url: u,
	}

	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(s.url)
			//r.Out.Host = r.In.Host
		},
	}

	proxy.ServeHTTP(w, r)
}
