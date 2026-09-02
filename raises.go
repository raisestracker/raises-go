// Package raises reports Go errors, panics, and operational events to Raises.
package raises

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
)

// Version is the client version sent in the User-Agent header.
const Version = "0.1.0"

const (
	defaultURL    = "https://raises.dev"
	sdkModulePath = "github.com/raisestracker/raises-go"
)

// Config overrides the RAISES_TOKEN, RAISES_URL, RAISES_ENV, and
// RAISES_REVISION environment variables.
type Config struct {
	Token       string
	BaseURL     string
	Environment string
	Revision    string
	HTTPClient  *http.Client
}

// Client reports errors and events. It is safe for concurrent use.
type Client struct {
	token       string
	baseURL     *url.URL
	environment string
	revision    string
	httpClient  *http.Client
	modulePath  string
}

// ReportOptions describes an explicitly reported error.
type ReportOptions struct {
	Handled bool
	Context map[string]any
	Source  string
}

// Level is an operational event level.
type Level string

// Supported operational event levels.
const (
	Info    Level = "info"
	Warning Level = "warning"
	Error   Level = "error"
)

// Event describes an operational event that does not create an error group.
type Event struct {
	Level   Level
	Source  string
	Context map[string]any
}

type noticePayload struct {
	Environment string         `json:"env"`
	Revision    string         `json:"revision,omitempty"`
	Handled     bool           `json:"handled"`
	Error       errorPayload   `json:"error"`
	Context     map[string]any `json:"context,omitempty"`
	Request     map[string]any `json:"request,omitempty"`
}

type errorPayload struct {
	Class     string   `json:"class"`
	Message   string   `json:"message"`
	Backtrace []string `json:"backtrace"`
}

type eventPayload struct {
	Environment string         `json:"env"`
	Revision    string         `json:"revision,omitempty"`
	Level       Level          `json:"level"`
	Message     string         `json:"message"`
	Source      string         `json:"source,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
}

// New creates a client. Empty configuration fields use environment variables.
func New(config Config) (*Client, error) {
	baseURL := firstPresent(config.BaseURL, os.Getenv("RAISES_URL"), defaultURL)
	parsedURL, err := url.Parse(baseURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return nil, fmt.Errorf("raises: invalid URL %q", baseURL)
	}
	parsedURL.RawQuery = ""
	parsedURL.Fragment = ""

	modulePath, buildRevision := buildDetails()
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 3 * time.Second}
	}

	return &Client{
		token:       firstPresent(config.Token, os.Getenv("RAISES_TOKEN")),
		baseURL:     parsedURL,
		environment: firstPresent(config.Environment, os.Getenv("RAISES_ENV"), "production"),
		revision:    firstPresent(config.Revision, os.Getenv("RAISES_REVISION"), os.Getenv("KAMAL_VERSION"), buildRevision),
		httpClient:  httpClient,
		modulePath:  modulePath,
	}, nil
}

// Report sends an error and its current call stack.
func (c *Client) Report(ctx context.Context, err error, options ReportOptions) error {
	if err == nil || c.token == "" {
		return nil
	}
	return c.send(ctx, "v1/notices", noticePayload{
		Environment: c.environment,
		Revision:    c.revision,
		Handled:     options.Handled,
		Error: errorPayload{
			Class:     concreteType(err),
			Message:   err.Error(),
			Backtrace: c.stack(),
		},
		Context: safeContext(options.Context, options.Source),
	})
}

// Notify sends an operational event without creating an error group.
func (c *Client) Notify(ctx context.Context, message string, event Event) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return fmt.Errorf("raises: message is required")
	}
	if utf8.RuneCountInString(message) > 2_000 {
		return fmt.Errorf("raises: message is too long")
	}
	if utf8.RuneCountInString(event.Source) > 120 {
		return fmt.Errorf("raises: source is too long")
	}
	if event.Level == "" {
		event.Level = Info
	}
	if event.Level != Info && event.Level != Warning && event.Level != Error {
		return fmt.Errorf("raises: level must be info, warning, or error")
	}
	if c.token == "" {
		return nil
	}
	return c.send(ctx, "v1/events", eventPayload{
		Environment: c.environment,
		Revision:    c.revision,
		Level:       event.Level,
		Message:     message,
		Source:      event.Source,
		Context:     safeContext(event.Context, ""),
	})
}

// Recover reports a panic and then panics again with the original value.
// Call it directly from defer at a process or goroutine boundary.
func (c *Client) Recover(ctx context.Context) {
	recovered := recover()
	if recovered == nil {
		return
	}
	c.reportPanic(ctx, recovered, nil)
	panic(recovered)
}

func (c *Client) reportPanic(ctx context.Context, recovered any, request map[string]any) {
	defer func() { _ = recover() }()
	if c == nil || c.token == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = c.send(ctx, "v1/notices", noticePayload{
		Environment: c.environment,
		Revision:    c.revision,
		Error: errorPayload{
			Class:     "panic:" + concreteType(recovered),
			Message:   fmt.Sprint(recovered),
			Backtrace: c.stack(),
		},
		Request: request,
	})
}

func (c *Client) send(ctx context.Context, endpointPath string, payload any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("raises: encode payload: %w", err)
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + endpointPath
	endpoint.RawPath = ""

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("raises: create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "raises-go/"+Version)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("raises: send: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("raises: HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *Client) stack() []string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	stack := make([]string, 0, n)
	for {
		frame, more := frames.Next()
		packagePath := framePackage(frame.Function)
		if packagePath != sdkModulePath {
			stack = append(stack, formatFrame(frame, packagePath, c.modulePath))
		}
		if !more {
			break
		}
	}
	return stack
}

func formatFrame(frame runtime.Frame, packagePath, modulePath string) string {
	file := filepath.Base(frame.File)
	location := path.Join(packagePath, file)
	if isApplicationPackage(packagePath, modulePath) {
		relativePackage := strings.TrimPrefix(strings.TrimPrefix(packagePath, modulePath), "/")
		if packagePath == modulePath+"_test" || packagePath == "main" {
			relativePackage = ""
		}
		location = path.Join("app", relativePackage, file)
	}
	if location == "." || location == "" {
		location = file
	}
	return fmt.Sprintf("%s:%d in %s", location, frame.Line, frame.Function)
}

func framePackage(function string) string {
	slash := strings.LastIndex(function, "/")
	dot := strings.Index(function[slash+1:], ".")
	if dot < 0 {
		return ""
	}
	return function[:slash+1+dot]
}

func isApplicationPackage(packagePath, modulePath string) bool {
	return packagePath == "main" || packagePath == modulePath || packagePath == modulePath+"_test" ||
		(modulePath != "" && strings.HasPrefix(packagePath, modulePath+"/"))
}

func safeContext(contextValues map[string]any, source string) map[string]any {
	if len(contextValues) == 0 && source == "" {
		return nil
	}
	values := make(map[string]any, len(contextValues)+1)
	for key, value := range contextValues {
		values[key] = value
	}
	if source != "" {
		values["source"] = source
	}
	raw, err := json.Marshal(values)
	if err != nil {
		fallback := map[string]any{"unserializable": "context"}
		if source != "" {
			fallback["source"] = source
		}
		return fallback
	}
	var safe map[string]any
	if json.Unmarshal(raw, &safe) != nil {
		return map[string]any{"unserializable": "context"}
	}
	return safe
}

func concreteType(value any) string {
	if value == nil {
		return "unknown"
	}
	return reflect.TypeOf(value).String()
}

func firstPresent(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func buildDetails() (modulePath, revision string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			revision = setting.Value
			break
		}
	}
	return info.Main.Path, revision
}
