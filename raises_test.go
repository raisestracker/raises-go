package raises_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	raises "github.com/raisestracker/raises-go"
)

type capturedRequest struct {
	Path          string
	Authorization string
	ContentType   string
	UserAgent     string
	Body          map[string]any
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func captureServer(t *testing.T, status int) (*httptest.Server, <-chan capturedRequest) {
	t.Helper()
	requests := make(chan capturedRequest, 10)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests <- capturedRequest{
			Path:          request.URL.Path,
			Authorization: request.Header.Get("Authorization"),
			ContentType:   request.Header.Get("Content-Type"),
			UserAgent:     request.Header.Get("User-Agent"),
			Body:          body,
		}
		writer.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func newClient(t *testing.T, baseURL string) *raises.Client {
	t.Helper()
	client, err := raises.New(raises.Config{
		Token:       "test-token",
		BaseURL:     baseURL,
		Environment: "test",
		Revision:    "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestReportSendsStableNotice(t *testing.T) {
	server, requests := captureServer(t, http.StatusCreated)
	client := newClient(t, server.URL+"/prefix")

	if err := client.Report(context.Background(), errors.New("boom"), raises.ReportOptions{
		Handled: true,
		Source:  "nightly-job",
		Context: map[string]any{"job_id": 7},
	}); err != nil {
		t.Fatal(err)
	}

	request := <-requests
	if request.Path != "/prefix/v1/notices" || request.Authorization != "Bearer test-token" {
		t.Fatalf("path=%q authorization=%q", request.Path, request.Authorization)
	}
	if request.ContentType != "application/json" || request.UserAgent != "raises-go/"+raises.Version {
		t.Fatalf("content_type=%q user_agent=%q", request.ContentType, request.UserAgent)
	}
	if request.Body["env"] != "test" || request.Body["revision"] != "abc123" || request.Body["handled"] != true {
		t.Fatalf("body=%#v", request.Body)
	}
	errorBody := request.Body["error"].(map[string]any)
	if errorBody["class"] != "*errors.errorString" || errorBody["message"] != "boom" {
		t.Fatalf("error=%#v", errorBody)
	}
	frames := errorBody["backtrace"].([]any)
	joined := strings.Join(anyStrings(frames), "\n")
	if !strings.Contains(joined, "app/raises_test.go:") {
		t.Fatalf("missing application frame:\n%s", joined)
	}
	if strings.Contains(joined, "/Users/") || strings.Contains(joined, "/home/") {
		t.Fatalf("absolute path leaked:\n%s", joined)
	}
	contextBody := request.Body["context"].(map[string]any)
	if contextBody["job_id"] != float64(7) || contextBody["source"] != "nightly-job" {
		t.Fatalf("context=%#v", contextBody)
	}
}

func TestNotifyAndValidation(t *testing.T) {
	server, requests := captureServer(t, http.StatusCreated)
	client := newClient(t, server.URL)

	if err := client.Notify(context.Background(), "Import finished", raises.Event{
		Source:  "nightly-import",
		Context: map[string]any{"imported": 412},
	}); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request.Path != "/v1/events" || request.Body["level"] != "info" || request.Body["message"] != "Import finished" {
		t.Fatalf("request=%#v", request)
	}
	if err := client.Notify(context.Background(), "event", raises.Event{Level: "debug"}); err == nil {
		t.Fatal("expected invalid level error")
	}
	if err := client.Notify(context.Background(), " ", raises.Event{}); err == nil {
		t.Fatal("expected missing message error")
	}
}

func TestDisabledClientDoesNotSend(t *testing.T) {
	server, requests := captureServer(t, http.StatusCreated)
	t.Setenv("RAISES_TOKEN", "")
	client, err := raises.New(raises.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Report(context.Background(), errors.New("boom"), raises.ReportOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		t.Fatalf("unexpected request: %#v", request)
	default:
	}
}

func TestReportReturnsHTTPError(t *testing.T) {
	server, _ := captureServer(t, http.StatusServiceUnavailable)
	client := newClient(t, server.URL)
	if err := client.Report(context.Background(), errors.New("boom"), raises.ReportOptions{}); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("error=%v", err)
	}
}

func TestRecoverReportsAndRepanics(t *testing.T) {
	server, requests := captureServer(t, http.StatusCreated)
	client := newClient(t, server.URL)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		panicWithRaises(client)
	}()
	if recovered != "boom" {
		t.Fatalf("recovered=%#v", recovered)
	}
	request := <-requests
	errorBody := request.Body["error"].(map[string]any)
	if errorBody["class"] != "panic:string" || errorBody["message"] != "boom" {
		t.Fatalf("error=%#v", errorBody)
	}
	if request.Body["handled"] != false {
		t.Fatalf("body=%#v", request.Body)
	}
}

func TestHandlerReportsRedactedRequestAndRepanics(t *testing.T) {
	server, requests := captureServer(t, http.StatusCreated)
	client := newClient(t, server.URL)
	handler := client.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler broke")
	}))
	request := httptest.NewRequest(http.MethodPost, "https://example.test/users?token=secret", nil)
	request.Header.Set("Authorization", "secret")
	request.AddCookie(&http.Cookie{Name: "session", Value: "secret"})

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	if recovered != "handler broke" {
		t.Fatalf("recovered=%#v", recovered)
	}
	captured := <-requests
	requestBody := captured.Body["request"].(map[string]any)
	if len(requestBody) != 2 || requestBody["method"] != "POST" || requestBody["path"] != "/users" {
		t.Fatalf("request=%#v", requestBody)
	}
	raw, _ := json.Marshal(captured.Body)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "token") || strings.Contains(string(raw), "session") {
		t.Fatalf("sensitive request data leaked: %s", raw)
	}
}

func TestRecoverPreservesPanicWhenTransportPanics(t *testing.T) {
	client, err := raises.New(raises.Config{
		Token:   "test-token",
		BaseURL: "https://raises.dev",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			panic("transport broke")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		panicWithRaises(client)
	}()
	if recovered != "boom" {
		t.Fatalf("recovered=%#v", recovered)
	}
}

func panicWithRaises(client *raises.Client) {
	defer client.Recover(context.Background())
	panic("boom")
}

func anyStrings(values []any) []string {
	strings := make([]string, len(values))
	for index, value := range values {
		strings[index] = value.(string)
	}
	return strings
}
