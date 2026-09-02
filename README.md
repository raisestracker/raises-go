# raises-go

The Go client for [Raises](https://raises.dev), error reporting operated by coding agents.

```sh
go get github.com/raisestracker/raises-go@v0.1.0
```

Set `RAISES_TOKEN` to the ingestion credential created for the project. `RAISES_URL`, `RAISES_ENV`, and `RAISES_REVISION` are optional.

## Report an error

```go
client, err := raises.New(raises.Config{})
if err != nil {
	log.Fatal(err)
}

if err := runJob(ctx); err != nil {
	_ = client.Report(ctx, err, raises.ReportOptions{
		Handled: true,
		Context: map[string]any{"job_id": jobID},
	})
	return err
}
```

Go has no global error subscriber. Report returned errors where the application handles them.

## Report panics

Capture a panic at a process or goroutine boundary. Raises reports it and then panics again.

```go
defer client.Recover(context.Background())
```

Each goroutine needs its own recovery boundary:

```go
go func() {
	defer client.Recover(context.Background())
	runWorker()
}()
```

For `net/http`, wrap the handler:

```go
http.ListenAndServe(":8080", client.Handler(mux))
```

The middleware sends only the request method and URL path. It does not send headers, query strings, cookies, or request bodies.

## Send an operational event

```go
_ = client.Notify(ctx, "Import finished", raises.Event{
	Level:   raises.Info,
	Source:  "nightly-import",
	Context: map[string]any{"imported": 412, "skipped": 3},
})
```

Valid levels are `raises.Info`, `raises.Warning`, and `raises.Error`. Events do not create error groups or GitHub issues.

## Delivery

The client sends synchronously with a three-second default timeout. Delivery failures are returned to the caller and never replace the application error. A missing token disables delivery.

The client has no third-party dependencies.

## Licensing

This module is MIT-licensed; see [LICENSE](LICENSE). The Raises server and embedded web UI are separately AGPL-3.0-only.
