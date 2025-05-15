package exa

type ContentsRequest struct {
	URLs []string `json:"urls"`
}

type ContentsResponse struct {
	Results []ContentsResult `json:"results"`
}

type ContentsResult struct {
	ID string `json:"id"`

	URL   string `json:"url"`
	Title string `json:"title"`

	Text string `json:"text"`
}
