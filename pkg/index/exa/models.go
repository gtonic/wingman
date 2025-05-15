package exa

type SearchRequest struct {
	Query string `json:"query"`

	Contents SearchContents `json:"contents,omitempty"`
}

type SearchContents struct {
	Text bool `json:"text,omitempty"`
}

type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

type SearchResult struct {
	ID string `json:"id"`

	URL   string `json:"url"`
	Title string `json:"title"`

	Text string `json:"text"`
}
