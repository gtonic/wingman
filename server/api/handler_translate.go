package api

import (
	"net/http"

	"github.com/adrianliechti/wingman/pkg/translator"
)

func (h *Handler) handleTranslate(w http.ResponseWriter, r *http.Request) {
	model := valueModel(r)
	language := valueLanguage(r)

	p, err := h.Translator(model)

	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	text, err := h.readText(r)

	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	options := &translator.TranslateOptions{
		Language: language,
	}

	input := translator.Input{
		Text: text,
	}

	result, err := p.Translate(r.Context(), input, options)

	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	contentType := result.ContentType

	if contentType != "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Write(result.Content)
}
