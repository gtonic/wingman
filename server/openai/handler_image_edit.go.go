package openai

// import (
// 	"encoding/base64"
// 	"io"
// 	"mime"
// 	"net/http"
// 	"path"

// 	"github.com/adrianliechti/wingman/pkg/provider"
// )

// func (h *Handler) handleImageEdit(w http.ResponseWriter, r *http.Request) {
// 	if err := r.ParseMultipartForm(32 << 20); err != nil {
// 		writeError(w, http.StatusBadRequest, err)
// 		return
// 	}

// 	model := r.FormValue("model")
// 	prompt := r.FormValue("prompt")

// 	reader, header, err := r.FormFile("image[]")

// 	if err != nil {
// 		reader, header, err = r.FormFile("image")
// 	}

// 	if err != nil {
// 		writeError(w, http.StatusBadRequest, err)
// 		return
// 	}

// 	renderer, err := h.Renderer(model)

// 	if err != nil {
// 		writeError(w, http.StatusBadRequest, err)
// 		return
// 	}

// 	options := &provider.RenderOptions{
// 		Image: &provider.Image{
// 			Name:   header.Filename,
// 			Reader: reader,
// 		},
// 	}

// 	image, err := renderer.Render(r.Context(), prompt, options)

// 	if err != nil {
// 		writeError(w, http.StatusBadRequest, err)
// 		return
// 	}

// 	data, err := io.ReadAll(image.Reader)

// 	if err != nil {
// 		writeError(w, http.StatusBadRequest, err)
// 		return
// 	}

// 	result := ImageList{}

// 	if r.FormValue("response_format") == "url" {
// 		mime := mime.TypeByExtension(path.Ext(image.Name))

// 		if mime == "" {
// 			mime = "image/png"
// 		}

// 		result.Images = []Image{
// 			{
// 				URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
// 			},
// 		}
// 	} else {
// 		result.Images = []Image{
// 			{
// 				B64JSON: base64.StdEncoding.EncodeToString(data),
// 			},
// 		}
// 	}

// 	writeJson(w, result)
// }
