package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	w.Write([]byte(text))
}

func readJSON(r *http.Request, dest any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 128*1024))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.Unmarshal(body, dest)
}

func queryParam(r *http.Request, key string) string {
	return r.URL.Query().Get(key)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func internalError(w http.ResponseWriter, err error) {
	log.Printf("Internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

// serviceError keeps infrastructure details, absolute paths, command output,
// and provider responses in controller logs while returning a stable
// operation-specific message to the client.
func serviceError(w http.ResponseWriter, status int, publicMessage string, err error) {
	log.Printf("%s: %v", publicMessage, err)
	writeError(w, status, publicMessage)
}
