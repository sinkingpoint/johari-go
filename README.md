# Johari

This is a simple wrapper library around the OpenTelemetry client lib that is designed to strip the complexity of orchestrating tracing down to its bare minimum.

## Example

A minimal example requires two function calls:

```
johari.InitTracing(johari.JohariConfig{
  ServiceName:  "alertmanager-bouncer",
  CollectorURL: "http://jaeger:14268/api/traces",
  SamplingRate: 1,
})

johari.NewHTTPServerWrapper(proxy)
```

where `proxy` is your HTTP Handler (anything with a `ServeHTTP` method). That gives you some minimal orchestration that creates a span for every request coming in to the server. If your server also makes requests out, then you should substitute `http.NewRequest` with `johari.NewChildRequest` and wrap your client in `NewHTTPClientWrapper`:

```
request, err := johari.NewChildRequest(
  req.Context(),
  method,
  url,
  body,
)

johari.NewHTTPClientWrapper(http.DefaultClient).Do(request)
```

Where `req` is the incoming request that spawned this subrequest. That'll give you child spans with all the information about the outgoing request, and the response. If you want to do your own custom spans within a request, you can generate new ones with `NewChildSpan`, e.g.:

```
bctx, bspan := johari.NewChildSpan(req.Context(), "bouncer")
defer bspan.End()
```

Where `req` is the incoming request. You can also pass the context from another span to nest the request under that instead