package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/example/ds-technical-assessment/graph"
	"github.com/example/ds-technical-assessment/internal/auth"
	"github.com/gorilla/websocket"
)

// Run initializes and starts the GraphQL server
func Run(ctx context.Context, db *sql.DB, addr string) error {
	graphqlHandler := newGraphQLHandler(db)
	healthHandler := newHealthHandler(db)

	http.Handle("/graphql", graphqlHandler)
	http.Handle("/health", healthHandler)
	http.Handle("/", playground.Handler("GraphQL Playground", "/graphql"))

	log.Printf("GraphQL endpoint available at http://localhost%s/graphql", addr)

	server := &http.Server{
		Addr: addr,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return server.Shutdown(shutdownCtx)
}

func BuildHandler(db *sql.DB) http.Handler {
	return newGraphQLHandler(db)
}

func BuildHealthHandler(db *sql.DB) http.HandlerFunc {
	return newHealthHandler(db)
}

func newGraphQLHandler(db *sql.DB) http.Handler {
	resolver := graph.NewResolver(db)
	schema := graph.NewExecutableSchema(graph.Config{
		Resolvers: resolver,
	})
	srv := handler.New(schema)
	srv.AddTransport(transport.Websocket{
    KeepAlivePingInterval: 10 * time.Second,
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		InitFunc: func(ctx context.Context, initPayload transport.InitPayload) (context.Context, *transport.InitPayload, error) {
			userID, _ := initPayload["X-User-ID"].(string)
			ctx = auth.SetUserID(ctx, userID)
			return ctx, &initPayload, nil
		},
	})
	srv.AddTransport(transport.POST{})
	return authMiddleware(srv)
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			next.ServeHTTP(w, r)
            return
        }
		userID := r.Header.Get("X-User-ID")
		if userID == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "X-User-ID header is required",
			})
			return
		}
		ctx := auth.SetUserID(r.Context(), userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}


func newHealthHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status":   "unhealthy",
				"postgres": "disconnected",
				"error":    err.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"status":   "healthy",
			"postgres": "connected",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
