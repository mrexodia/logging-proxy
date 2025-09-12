package main

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

func main() {
	mux := http.NewServeMux()

	// Pattern documentation: https://pkg.go.dev/net/http#hdr-Patterns-ServeMux
	// Wildcards (except {$}) are not permitted
	wildcardRegex := regexp.MustCompile(`{[a-zA-Z0-9_.]+`)

	routes := map[string]string{
		"/lmstudio/":               "http://127.0.0.1:1234/",
		"/openrouter/":             "https://openrouter.ai/api/v1/",
		"GET /lmstudio/mockfile":   "http://127.0.0.1:8080/static/mockfile.txt",
		"GET example.com/test/{$}": "https://example.com/test/index.html",
		"POST example.com/test/":   "https://example.com/test",
	}

	registerCatchAll := true
	for pattern, destination := range routes {
		fmt.Printf("[route] %s -> %s\n", pattern, destination)
		if wildcardRegex.MatchString(pattern) {
			panic(fmt.Sprintf("Pattern %s contains a wildcard, which is not supported\n", pattern))
		}

		// If the user specifies a catch-all route, we don't need to register our own handler
		if pattern == "/" {
			registerCatchAll = false
		}

		// Append a named wildcard so we can extract the path from the request
		if strings.HasSuffix(pattern, "/") {
			pattern += "{path...}"
		}

		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			path := r.PathValue("path")
			destinationUrl := destination
			if len(path) > 0 {
				joined, err := url.JoinPath(destination, path)
				if err != nil {
					fmt.Printf("Error joining path: %v\n", err)
				}
				destinationUrl = joined
			}

			if len(r.URL.RawQuery) > 0 {
				destinationUrl += "?" + r.URL.RawQuery
			}

			fmt.Fprintf(w, "method=%s\nurl=%s\ndestination=%s\n",
				r.Method,
				r.URL.String(),
				destinationUrl,
			)
		})
	}

	if registerCatchAll {
		fmt.Printf("Registering catch-all handler\n")
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// respond with 404
			http.Error(w, "custom 404 page", http.StatusNotFound)
		})
	} else {
		fmt.Printf("Skipping catch-all handler\n")
	}

	server := http.Server{
		Addr:                         ":6969",
		Handler:                      mux,
		DisableGeneralOptionsHandler: true,
	}

	err := server.ListenAndServe()
	if err != nil {
		fmt.Printf("Error starting server: %v", err)
	}
}
