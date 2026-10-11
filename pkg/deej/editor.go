package deej

import (
	_ "embed" // needed for go:embed
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// sliderEditor serves a small web page (on localhost only) for mapping apps and devices to sliders,
// showing live slider positions and editing the rest of the config. it saves to config.yaml, which
// deej then reloads just like it would after editing the file by hand
type sliderEditor struct {
	deej   *Deej
	logger *zap.SugaredLogger

	server       *http.Server
	url          string
	allowedHosts map[string]bool
}

//go:embed editor/index.html
var editorPage []byte

const (

	// spells "deej" on a phone keypad, so it's easy to remember (http://localhost:3335)
	editorPreferredPort = 3335

	// all API requests must carry this header. browsers won't let other websites set custom headers on requests
	// to us (without a CORS preflight, which we never approve), so this protects against cross-site requests
	editorRequestHeader = "X-Deej-Editor"

	editorMaxRequestSize = 64 * 1024
)

func newSliderEditor(deej *Deej, logger *zap.SugaredLogger) *sliderEditor {
	return &sliderEditor{
		deej:   deej,
		logger: logger.Named("editor"),
	}
}

// start begins serving the editor in the background
func (e *sliderEditor) start() error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", editorPreferredPort))
	if err != nil {
		e.logger.Infow("Preferred port is taken, using a random one", "port", editorPreferredPort, "error", err)

		if listener, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			e.logger.Warnw("Failed to listen for slider editor connections", "error", err)
			return fmt.Errorf("listen for slider editor connections: %w", err)
		}
	}

	port := listener.Addr().(*net.TCPAddr).Port

	// only answer requests addressed to us by name, which protects against dns rebinding attacks
	e.allowedHosts = map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}

	e.url = fmt.Sprintf("http://127.0.0.1:%d/", port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", e.handlePage)
	mux.HandleFunc("/api/state", e.handleState)
	mux.HandleFunc("/api/config", e.handleConfig)
	mux.HandleFunc("/api/targets", e.handleTargets)
	mux.HandleFunc("/api/ports", e.handlePorts)

	e.server = &http.Server{
		Handler:           e.guard(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := e.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			e.logger.Warnw("Slider editor stopped unexpectedly", "error", err)
		}
	}()

	e.logger.Infow("Slider editor is available", "url", e.url)

	return nil
}

func (e *sliderEditor) stop() {
	if e.server != nil {
		e.server.Close()
	}
}

// open shows the editor in the default browser
func (e *sliderEditor) open() {
	if e.url == "" {
		e.deej.notifier.Notify("Slider editor unavailable", "Please check deej's logs for more details.")
		return
	}

	if err := util.OpenURL(e.logger, e.url); err != nil {
		e.logger.Warnw("Failed to open slider editor in browser", "error", err)
		e.deej.notifier.Notify("Can't open your browser", fmt.Sprintf("Open %s to use the slider editor.", e.url))
	}
}

func (e *sliderEditor) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !e.allowedHosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if r.URL.Path != "/" && r.Header.Get(editorRequestHeader) != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")

		next.ServeHTTP(w, r)
	})
}

func (e *sliderEditor) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'")

	w.Write(editorPage)
}

func (e *sliderEditor) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := struct {
		Serial     serialState `json:"serial"`
		Version    string      `json:"version"`
		Focused    []string    `json:"focused"`
		FocusError string      `json:"focusError,omitempty"`
	}{
		Serial:  e.deej.serial.state(),
		Version: e.deej.version,
		Focused: []string{},
	}

	// only look up the focused app when the page shows it, since that's not free on every desktop
	if r.URL.Query().Get("focus") == "1" {
		names, err := util.GetCurrentWindowProcessNames()
		if err != nil {
			response.FocusError = err.Error()
		} else if names != nil {
			response.Focused = uniqueStrings(names)
		}
	}

	e.writeJSON(w, http.StatusOK, response)
}

func (e *sliderEditor) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		e.writeJSON(w, http.StatusOK, e.deej.config.editableValues())

	case http.MethodPost:
		var values editableConfig

		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, editorMaxRequestSize))
		if err := decoder.Decode(&values); err != nil {
			e.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
			return
		}

		if err := e.deej.config.saveEditableValues(values); err != nil {
			e.writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		e.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (e *sliderEditor) handleTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	e.writeJSON(w, http.StatusOK, e.deej.sessions.availableTargets())
}

func (e *sliderEditor) handlePorts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ports, err := util.FindSerialPorts()
	if err != nil {
		e.logger.Debugw("Failed to list serial ports", "error", err)
		ports = []util.SerialPort{}
	}

	e.writeJSON(w, http.StatusOK, ports)
}

func (e *sliderEditor) writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		e.logger.Debugw("Failed to write response", "error", err)
	}
}

func (e *sliderEditor) writeError(w http.ResponseWriter, status int, message string) {
	e.writeJSON(w, status, map[string]string{"error": message})
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}

	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}

	return result
}
