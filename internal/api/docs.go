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
//
// The script is version-pinned with an SRI digest and crossorigin=anonymous: this
// page is served on the PORTAL origin too, where the operator's session cookie
// lives and csrfGuard treats same-origin as trusted — an unpinned CDN script would
// run with all of that. Same reasoning that keeps htmx vendored
// (web/template/components.templ). It is 3.7 MB, so it is pinned rather than
// vendored; the browser refuses it if the bytes ever change.
//
// To bump: pick a version, then
//
//	curl -sL https://cdn.jsdelivr.net/npm/@scalar/api-reference@VERSION/dist/browser/standalone.js |
//	  openssl dgst -sha384 -binary | openssl base64 -A
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
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.64.1/dist/browser/standalone.js"
            integrity="sha384-yNQdqLDpE2fst+aUqSHXcquVibo90vCkT+zBMLgYfCejLv85GXAR3tFg9lXDUJAd"
            crossorigin="anonymous"></script>
  </body>
</html>`
	// securityHeaders sets script-src 'self' on every surface, which blocks the
	// very script above: /docs returned 200 with a blank page on every
	// deployment, including localhost. Nothing caught it because the HTML is
	// fine - only the browser refuses to run it.
	//
	// This route is the one place the CDN is allowed, and only because the
	// script is SRI-pinned: a tampered or swapped bundle is refused by the
	// browser, so the exception costs availability (jsdelivr reachable) rather
	// than integrity. Everything else in the policy stays as securityHeaders
	// set it. Widen this no further - see TestDocsCSPAllowsItsOwnScripts.
	w.Header().Set("Content-Security-Policy",
		"script-src 'self' https://cdn.jsdelivr.net; frame-ancestors 'none'; base-uri 'self'; object-src 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}
