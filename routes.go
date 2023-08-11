package main

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"net/url"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Route struct defines a route in the application with a URL and a matching word for label enforcement.
type Route struct {
	Url       string
	MatchWord string
}

// contextKey is a string type that represents a context key.
type contextKey string

// KeycloakCtxToken are the context keys used in the application.
const (
	KeycloakCtxToken contextKey = "keycloakToken"
)

func (a *App) NewRoutes() (*mux.Router, *mux.Router, error) {
	lokiUrl, err := url.Parse(a.Cfg.Loki.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing Loki URL: %v", err)
	}

	thanosUrl, err := url.Parse(a.Cfg.Thanos.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing Thanos URL: %v", err)
	}

	i := mux.NewRouter()
	i.HandleFunc("/health", HealthCheckHandler)
	i.HandleFunc("/debug/pprof/", pprof.Index)
	i.Handle("/metrics", promhttp.Handler())

	routes := []Route{
		{Url: "/api/v1/query", MatchWord: "query"},
		{Url: "/api/v1/query_range", MatchWord: "query"},
		{Url: "/api/v1/label/{name}/values", MatchWord: "query"},
		{Url: "/api/v1/series", MatchWord: "match[]"},
		{Url: "/api/v1/tail", MatchWord: "query"},
		{Url: "/api/v1/index/stats", MatchWord: "query"},
		{Url: "/api/v1/format_query", MatchWord: "query"},
		{Url: "/api/v1/labels", MatchWord: "match[]"},
		{Url: "/api/v1/label/{label}/values", MatchWord: "match[]"},
		{Url: "/api/v1/query_exemplars", MatchWord: "query"},
		{Url: "/api/v1/status/buildinfo", MatchWord: "query"},
	}

	e := mux.NewRouter()

	lokiRouter := e.PathPrefix("/loki").Subrouter()
	thanosRouter := e.PathPrefix("").Subrouter()

	e.Use(a.loggingMiddleware)
	e.Use(authMiddleware)

	for _, route := range routes {

		lokiRouter.HandleFunc(route.Url, func(w http.ResponseWriter, r *http.Request) {
			req := Request{route.MatchWord, w, r, LogQLEnforcer{}}
			err := req.enforce(ConfigMapProvider{
				Users:  nil,
				Groups: nil,
			})
			if err != nil {
				return
			}
			req.callUpstream(thanosUrl, Cfg.Thanos.UseMutualTLS)
		})

		thanosRouter.HandleFunc(route.Url, func(w http.ResponseWriter, r *http.Request) {
			req := Request{route.MatchWord, w, r, PromQLRequest{}}
			err := req.enforce(ConfigMapProvider{
				Users:  nil,
				Groups: nil,
			})
			if err != nil {
				return
			}
			req.callUpstream(lokiUrl, Cfg.Loki.UseMutualTLS)
		})
	}

	e.SkipClean(true)
	return e, i, nil
}

// HealthCheckHandler is a HTTP handler function that always responds with
// HTTP status code 200 and body "Ok". It is typically used for health check endpoints.
func HealthCheckHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Ok"))
}
