package api

import "net/http"

func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if s.spec == nil {
		writeError(w, http.StatusNotFound, "not_found", "spec unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(s.spec)
}

// handleDocs serves a Scalar API reference UI that loads /openapi.yaml.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	const page = `<!doctype html>
<html>
  <head>
    <title>bank0 API</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/openapi.yaml"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}
