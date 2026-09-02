package raises

import "net/http"

// Handler reports panics with redacted request metadata and then panics again.
func (c *Client) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			path := "/"
			if request.URL != nil && request.URL.Path != "" {
				path = request.URL.Path
			}
			c.reportPanic(request.Context(), recovered, map[string]any{
				"method": request.Method,
				"path":   path,
			})
			panic(recovered)
		}()
		next.ServeHTTP(writer, request)
	})
}
