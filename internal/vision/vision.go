package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// DefaultVisionPort is the default port for the standalone vision microservice.
const DefaultVisionPort = 18095

// DescribeRequest is the JSON payload for visual analysis.
type DescribeRequest struct {
	Image  string `json:"image"`            // base64 data URL or HTTP URL
	Prompt string `json:"prompt,omitempty"` // custom prompt for description
}

// DescribeResponse is the JSON result of visual analysis.
type DescribeResponse struct {
	Status      string `json:"status"`
	Description string `json:"description"`
	Tokens      int    `json:"tokens,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	Error       string `json:"error,omitempty"`
}

// HealthResponse is the service health check.
type HealthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Model   string `json:"model,omitempty"`
	CPU     bool   `json:"cpu"`
	Port    int    `json:"port"`
}

// Client communicates with a running vision microservice.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient creates a client pointing to a vision microservice.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = fmt.Sprintf("http://127.0.0.1:%d", DefaultVisionPort)
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// Ping checks if the vision service is responsive and healthy.
func (c *Client) Ping(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Describe sends an image to the vision service and returns a textual description.
func (c *Client) Describe(ctx context.Context, imgURL, prompt string) (string, error) {
	if prompt == "" {
		prompt = "Describe this image in detail, including key objects, scene composition, and any visible text."
	}
	payload := DescribeRequest{
		Image:  imgURL,
		Prompt: prompt,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/vision/describe", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("vision service returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var res DescribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if res.Error != "" {
		return "", fmt.Errorf("vision analysis error: %s", res.Error)
	}
	return res.Description, nil
}

// ProcessMessages inspects message content for image_url items.
// If found and the vision service is responsive, it replaces them with visual descriptions.
func ProcessMessages(ctx context.Context, client *Client, msgs []any) ([]any, bool, error) {
	if client == nil || len(msgs) == 0 {
		return msgs, false, nil
	}
	hasImages := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := mm["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if ok && (pm["type"] == "image_url" || pm["type"] == "image") {
				hasImages = true
				break
			}
		}
		if hasImages {
			break
		}
	}
	if !hasImages {
		return msgs, false, nil
	}

	out := make([]any, len(msgs))
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out[i] = m
			continue
		}
		parts, ok := mm["content"].([]any)
		if !ok {
			out[i] = m
			continue
		}
		newParts := make([]any, 0, len(parts))
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				newParts = append(newParts, p)
				continue
			}
			t, _ := pm["type"].(string)
			if t == "image_url" || t == "image" {
				imgURL := ""
				if iu, ok := pm["image_url"].(map[string]any); ok {
					imgURL, _ = iu["url"].(string)
				} else if s, ok := pm["image_url"].(string); ok {
					imgURL = s
				} else if s, ok := pm["image"].(string); ok {
					imgURL = s
				}
				desc, err := client.Describe(ctx, imgURL, "")
				if err != nil {
					return nil, false, fmt.Errorf("vision pre-flight failed: %w", err)
				}
				newParts = append(newParts, map[string]any{
					"type": "text",
					"text": fmt.Sprintf("[Visual Context: %s]", desc),
				})
			} else {
				newParts = append(newParts, p)
			}
		}
		newMsg := make(map[string]any, len(mm))
		for k, v := range mm {
			newMsg[k] = v
		}
		newMsg["content"] = newParts
		out[i] = newMsg
	}
	return out, true, nil
}

// ServerOptions configures the vision microservice server.
type ServerOptions struct {
	Port       int
	Host       string
	ModelPath  string
	CPUOnly    bool
	Threads    int
	BackendURL string // optional proxy target to a running multimodal engine
}

// Server provides the HTTP endpoints for standalone vision processing.
type Server struct {
	opts       ServerOptions
	httpServer *http.Server
	cmd        *exec.Cmd
}

// NewServer initializes a new vision microservice.
func NewServer(opts ServerOptions) *Server {
	if opts.Port <= 0 {
		opts.Port = DefaultVisionPort
	}
	if opts.Host == "" {
		opts.Host = "0.0.0.0"
	}
	return &Server{opts: opts}
}

// Start runs the HTTP server.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/vision/describe", s.handleDescribe)

	addr := fmt.Sprintf("%s:%d", s.opts.Host, s.opts.Port)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return s.httpServer.ListenAndServe()
}

// Close terminates the server.
func (s *Server) Close() error {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	if s.httpServer != nil {
		return s.httpServer.Close()
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(HealthResponse{
		Status:  "ok",
		Service: "fleet-vision",
		Model:   s.opts.ModelPath,
		CPU:     s.opts.CPUOnly,
		Port:    s.opts.Port,
	})
}

func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	var req DescribeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Image == "" {
		http.Error(w, "image required", http.StatusBadRequest)
		return
	}

	// If a backend multimodal runner (e.g. llama-server with mmproj) is running, proxy to it.
	desc := ""
	if s.opts.BackendURL != "" {
		d, err := s.forwardToBackend(r.Context(), req)
		if err != nil {
			// Not a made-up description: the caller would hand it to a model as "[Visual Context: …]" and the model
			// would answer about an image nobody looked at.
			http.Error(w, "vision backend failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		desc = d
	}
	if desc == "" {
		// No backend (or it described nothing): say so plainly. This text goes to a model as the image's content.
		format := "unknown"
		if strings.HasPrefix(req.Image, "data:image/") {
			parts := strings.SplitN(req.Image, ";", 2)
			format = strings.TrimPrefix(parts[0], "data:image/")
		}
		desc = fmt.Sprintf("an image (format %s, %d bytes) is attached but could not be analyzed: no vision backend answered, so its content is unknown", format, len(req.Image))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(DescribeResponse{
		Status:      "ok",
		Description: desc,
		DurationMS:  time.Since(start).Milliseconds(),
	})
}

func (s *Server) forwardToBackend(ctx context.Context, req DescribeRequest) (string, error) {
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": req.Prompt},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": req.Image}},
				},
			},
		},
		"max_tokens": 500,
	}
	b, err := json.Marshal(chatReq)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", s.opts.BackendURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var res map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	if choices, ok := res["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			if msg, ok := c0["message"].(map[string]any); ok {
				if content, ok := msg["content"].(string); ok {
					return content, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no content in backend response")
}
