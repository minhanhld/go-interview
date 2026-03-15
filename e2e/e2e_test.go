// Package e2e contains end-to-end tests for the GraphQL API.
//
// =============================================================================
// WHAT IS AN END-TO-END TEST IN GO?
// =============================================================================
// Go's testing system is built into the standard library. Any file ending in
// `_test.go` is a test file. Functions named `TestXxx(t *testing.T)` are test
// cases that `go test` will discover and run automatically.
//
// An E2E test is different from a unit test:
//   - Unit test: tests one function in isolation, mocking dependencies
//   - E2E test: tests the WHOLE system running for real (real HTTP, real DB)
//
// Our E2E tests will:
//   1. Start the real server (with a real DB connection)
//   2. Make real HTTP requests with the GraphQL protocol
//   3. Assert the responses are correct
//
// HOW TO RUN:
//   go test ./e2e/... -v
//   (or, if this file is at the root: go test . -run TestE2E -v)
//
// The -v flag shows verbose output (prints t.Log messages).
// The DATABASE_URL environment variable must point to a running PostgreSQL.

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	// "strings"
	"testing"
	"time"

	"github.com/example/ds-technical-assessment/src/server"
	// "github.com/gorilla/websocket"
	_ "github.com/lib/pq"
)

// =============================================================================
// TEST SETUP HELPERS
// =============================================================================

// testServer starts the real HTTP server using Go's httptest.NewServer.
//
// httptest.NewServer is part of Go's standard library (net/http/httptest).
// Instead of binding to a real port on your machine, it creates a temporary
// server on a random available port. This is perfect for tests: no port
// conflicts, no cleanup needed for ports.
//
// It returns:
//   - *httptest.Server: call ts.Close() when done to shut it down
//   - The base URL to use for requests (e.g., "http://127.0.0.1:54321")
func testServer(t *testing.T, db *sql.DB) *httptest.Server {
	t.Helper() // marks this as a helper so failures show the caller's line number

	// We need to set up the HTTP mux (router) the same way server.Run does,
	// but using httptest instead of a real server.
	//
	// http.NewServeMux creates a fresh mux (as opposed to http.DefaultServeMux
	// which is the global one used by http.Handle). Using a fresh mux prevents
	// test routes from leaking into other tests.
	mux := http.NewServeMux()

	// We call the exported setup functions from the server package.
	// But since those are unexported in server.go (lowercase), we need to
	// call server.Run indirectly... OR we refactor server.go to expose
	// the handler setup.
	//
	// Instead, we'll call the server package's handler builders directly.
	// Since we can't call unexported functions from another package, we'll
	// use a helper exported from server.go — or we can replicate the setup
	// minimally here for the test.
	//
	// The cleanest approach: export a BuildHandler function from server package.
	// But since we want to keep the test self-contained, we'll use the
	// approach of running the server in a background goroutine and using
	// a real HTTP client.
	//
	// Here we use BuildHandler which we'll add to server.go (see note below).
	mux.Handle("/graphql", server.BuildHandler(db))
	mux.Handle("/health", server.BuildHealthHandler(db))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close) // t.Cleanup registers a function to run when the test ends
	return ts
}

// openTestDB opens a connection to the test database.
// It uses the same default connection string as the main app.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	connStr := "postgres://postgres:postgres@localhost:5432/technical_assessment?sslmode=disable"
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("opening test DB: %v", err)
		// t.Fatalf logs the message and immediately stops this test.
		// It's like t.Errorf (marks the test as failed) + return.
	}

	// Give the DB 5 seconds to become available (useful in CI environments).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("pinging test DB: %v", err)
	}

	t.Cleanup(func() { db.Close() })
	return db
}

// =============================================================================
// GraphQL HTTP CLIENT HELPER
// =============================================================================
// graphqlRequest sends a GraphQL query/mutation over HTTP POST to the server.
//
// The GraphQL HTTP protocol is simple:
//
//	POST /graphql
//	Content-Type: application/json
//	X-User-ID: <user>
//	Body: {"query": "...", "variables": {...}}
//
// The response is:
//
//	{"data": {...}, "errors": [...]}
//
// Parameters:
//
//	t        - the test context
//	baseURL  - e.g., "http://127.0.0.1:12345"
//	userID   - the value to put in X-User-ID header
//	query    - the GraphQL query string
//	variables - optional variables map (can be nil)
//	result   - pointer to a struct that the `data` field will be decoded into
func graphqlRequest(t *testing.T, baseURL, userID, query string, variables map[string]any, result any) {
	t.Helper()

	// Build the JSON request body.
	// map[string]any{"query": ..., "variables": ...} is a Go map literal.
	// json.Marshal converts it to a JSON []byte.
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}

	// bytes.NewReader wraps []byte in an io.Reader (needed by http.NewRequest).
	req, err := http.NewRequest("POST", baseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)

	// http.DefaultClient is Go's built-in HTTP client. Fine for tests.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("executing request: %v", err)
	}
	defer resp.Body.Close()

	// Read the full response body.
	// io.ReadAll reads until EOF and returns the complete []byte.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}

	// Check HTTP status code (should be 200 for GraphQL, even for errors —
	// GraphQL errors go in the "errors" field, not via HTTP status codes).
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, respBody)
	}

	// Parse the GraphQL response envelope.
	// We decode into this struct that matches the GraphQL response format.
	var envelope struct {
		Data json.RawMessage `json:"data"`
		// json.RawMessage stores the JSON as-is ([]byte) without decoding.
		// We'll decode the data field separately into `result`.
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		t.Fatalf("decoding response envelope: %v\nBody: %s", err, respBody)
	}

	// If there are GraphQL errors, fail the test with the messages.
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		t.Fatalf("GraphQL errors: %v\nFull response: %s", msgs, respBody)
	}

	// Decode the `data` field into the caller's result struct.
	if result != nil && envelope.Data != nil {
		if err := json.Unmarshal(envelope.Data, result); err != nil {
			t.Fatalf("decoding response data: %v\nData: %s", err, envelope.Data)
		}
	}
}

func TestHealthCheck(t *testing.T) {
	db := openTestDB(t)
	ts := testServer(t, db)

	type HealthResponse struct {
		Status   string `json:"status"`
		Postgres string `json:"postgres"`
	}

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("health check request failed: %v", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json content-type, got %s", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result HealthResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse JSON body: %v -- raw: %s", err, body)
	}

	if result.Status != "healthy" {
		t.Errorf("expected status=healthy, got %s", result.Status)
	}

	if result.Postgres != "connected" {
		t.Errorf("expected postgres=connected, got %s", result.Postgres)
	}
}

// =============================================================================
// TEST 2: Missing X-User-ID header returns 401
// =============================================================================
// Validates the auth middleware rejects requests without the header.
func TestAuthMiddleware_MissingHeader(t *testing.T) {
	db := openTestDB(t)
	ts := testServer(t, db)

	// Make a request WITHOUT the X-User-ID header
	body, err := json.Marshal(map[string]any{
		"query": `{ elements(first: 1) { edges { node { uri } } } }`,
	})
	if err != nil {
	t.Fatalf("failed to marshal request: %v", err)
}
	resp, err := http.Post(ts.URL+"/graphql", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}
	t.Log("Auth middleware correctly rejected missing header")
}

// =============================================================================
// TEST 3: Query elements — basic pagination
// =============================================================================
// This tests the elements query with a valid user and pagination.
// user:alice has access to space:acme-projects and space:acme-hr.
func TestQueryElements_BasicPagination(t *testing.T) {
	db := openTestDB(t)
	ts := testServer(t, db)

	// This is the GraphQL query string. It follows the GraphQL syntax:
	// - `query` keyword (optional but explicit)
	// - `elements(first: 5)` calls our resolver with first=5
	// - The nested fields describe what we want back
	query := `
		query {
			elements(first: 5) {
				pageInfo {
					hasNextPage
					endCursor
				}
				edges {
					cursor
					node {
						uri
						title
						type_uri
						space_uri
						creation_date
						author
					}
				}
			}
		}
	`

	// result will be decoded from the GraphQL `data` field.
	// The struct fields must match the GraphQL response shape.
	// json struct tags (the `json:"..."` parts) tell the JSON decoder
	// which JSON key maps to which Go field.
	var result struct {
		Elements struct {
			PageInfo struct {
				HasNextPage bool    `json:"hasNextPage"`
				EndCursor   *string `json:"endCursor"`
			} `json:"pageInfo"`
			Edges []struct {
				Cursor string `json:"cursor"`
				Node   struct {
					URI          string `json:"uri"`
					Title        string `json:"title"`
					TypeURI      string `json:"type_uri"`
					SpaceURI     string `json:"space_uri"`
					CreationDate string `json:"creation_date"`
					Author       string `json:"author"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}

	graphqlRequest(t, ts.URL, "user:alice", query, nil, &result)

	// Assertions
	// t.Errorf marks the test as failed but continues running.
	// (Use t.Fatalf if you want to stop immediately.)
	if len(result.Elements.Edges) == 0 {
		t.Error("expected at least one element, got none")
	}
	if len(result.Elements.Edges) > 5 {
		t.Errorf("expected at most 5 elements, got %d", len(result.Elements.Edges))
	}

	// All elements must belong to spaces alice has access to
	aliceSpaces := map[string]bool{
		"space:acme-projects": true,
		"space:acme-hr":       true,
	}
	for _, edge := range result.Elements.Edges {
		if !aliceSpaces[edge.Node.SpaceURI] {
			t.Errorf("element %s is in space %s which alice should not access",
				edge.Node.URI, edge.Node.SpaceURI)
		}
	}

	t.Logf("Got %d elements, hasNextPage=%v", len(result.Elements.Edges), result.Elements.PageInfo.HasNextPage)
}

// =============================================================================
// TEST 4: Query elements — cursor pagination (second page)
// =============================================================================
// Fetches the first page, then uses its endCursor to fetch the second page.
func TestQueryElements_CursorPagination(t *testing.T) {
	db := openTestDB(t)
	ts := testServer(t, db)

	// First page
	firstPageQuery := `
		query {
			elements(first: 3) {
				pageInfo { hasNextPage endCursor }
				edges { node { uri } }
			}
		}
	`
	var page1 struct {
		Elements struct {
			PageInfo struct {
				HasNextPage bool    `json:"hasNextPage"`
				EndCursor   *string `json:"endCursor"`
			} `json:"pageInfo"`
			Edges []struct {
				Node struct {
					URI string `json:"uri"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}
	graphqlRequest(t, ts.URL, "user:alice", firstPageQuery, nil, &page1)

	if !page1.Elements.PageInfo.HasNextPage {
		t.Skip("not enough elements to test pagination (need > 3)")
	}
	if page1.Elements.PageInfo.EndCursor == nil {
		t.Fatal("endCursor should not be nil when hasNextPage is true")
	}

	// Second page — using the cursor from the first page as `after`
	// GraphQL variables let you pass dynamic values without string interpolation.
	// This is safer and cleaner than building query strings.
	secondPageQuery := `
		query($after: String) {
			elements(first: 3, after: $after) {
				pageInfo { hasNextPage endCursor }
				edges { node { uri } }
			}
		}
	`
	var page2 struct {
		Elements struct {
			Edges []struct {
				Node struct {
					URI string `json:"uri"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}
	graphqlRequest(t, ts.URL, "user:alice", secondPageQuery, map[string]any{
		"after": *page1.Elements.PageInfo.EndCursor,
	}, &page2)

	if len(page2.Elements.Edges) == 0 {
		t.Error("second page should have elements")
	}

	// Verify no overlap between page 1 and page 2
	page1URIs := make(map[string]bool)
	for _, e := range page1.Elements.Edges {
		page1URIs[e.Node.URI] = true
	}
	for _, e := range page2.Elements.Edges {
		if page1URIs[e.Node.URI] {
			t.Errorf("element %s appeared on both page 1 and page 2", e.Node.URI)
		}
	}

	t.Logf("Page 1: %d elements, Page 2: %d elements — no overlap",
		len(page1.Elements.Edges), len(page2.Elements.Edges))
}

// =============================================================================
// TEST 5: Query elements — with field value filter
// =============================================================================
// Tests filtering elements by a field value.
func TestQueryElements_WithFilter(t *testing.T) {
	db := openTestDB(t)
	ts := testServer(t, db)

	// user:alice is in acme-projects where task elements exist with a status field.
	// We filter by field:task-status = "Done"
	query := `
		query($filter: FieldValueFilter) {
			elements(first: 10, filter: $filter) {
				edges {
					node {
						uri
						title
						field_values {
							uri
							value
							field {
								uri
								name
								data_type
							}
						}
					}
				}
			}
		}
	`

	var result struct {
		Elements struct {
			Edges []struct {
				Node struct {
					URI         string `json:"uri"`
					Title       string `json:"title"`
					FieldValues []struct {
						URI   string `json:"uri"`
						Value any    `json:"value"`
						Field struct {
							URI      string `json:"uri"`
							Name     string `json:"name"`
							DataType string `json:"data_type"`
						} `json:"field"`
					} `json:"field_values"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}

	graphqlRequest(t, ts.URL, "user:alice", query, map[string]any{
		"filter": map[string]any{
			"field_uri": "field:task-status",
			"value":     "Done",
		},
	}, &result)

	// Every returned element should have a "Status" field with value "Done"
	for _, edge := range result.Elements.Edges {
		found := false
		for _, fv := range edge.Node.FieldValues {
			if fv.Field.URI == "field:task-status" {
				if fmt.Sprintf("%v", fv.Value) == "Done" {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("element %s was returned but doesn't have task-status=Done", edge.Node.URI)
		}
	}

	t.Logf("Filter test: found %d elements with status=Done", len(result.Elements.Edges))
}

// =============================================================================
// TEST 6: Mutation — updateElement
// =============================================================================
// Updates an element's title and verifies the returned value.
func TestMutationUpdateElement(t *testing.T) {
	db := openTestDB(t)
	ts := testServer(t, db)

	// First, get a real element URI that alice can access
	var listResult struct {
		Elements struct {
			Edges []struct {
				Node struct {
					URI   string `json:"uri"`
					Title string `json:"title"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}
	graphqlRequest(t, ts.URL, "user:alice",
		`query { elements(first: 1) { edges { node { uri title } } } }`,
		nil, &listResult)

	if len(listResult.Elements.Edges) == 0 {
		t.Skip("no elements available to update")
	}

	targetURI := listResult.Elements.Edges[0].Node.URI
	originalTitle := listResult.Elements.Edges[0].Node.Title
	newTitle := fmt.Sprintf("Updated at %d", time.Now().UnixMilli())

	t.Logf("Updating element %s from %q to %q", targetURI, originalTitle, newTitle)

	// GraphQL mutations follow the same HTTP protocol as queries.
	// The only difference is the `mutation` keyword in the query string.
	mutationQuery := `
		mutation($uri: String!, $title: String!) {
			updateElement(uri: $uri, title: $title) {
				uri
				title
				author
				creation_date
			}
		}
	`
	var mutResult struct {
		UpdateElement struct {
			URI          string `json:"uri"`
			Title        string `json:"title"`
			Author       string `json:"author"`
			CreationDate string `json:"creation_date"`
		} `json:"updateElement"`
	}
	graphqlRequest(t, ts.URL, "user:alice", mutationQuery, map[string]any{
		"uri":   targetURI,
		"title": newTitle,
	}, &mutResult)

	if mutResult.UpdateElement.URI != targetURI {
		t.Errorf("expected URI %s, got %s", targetURI, mutResult.UpdateElement.URI)
	}
	if mutResult.UpdateElement.Title != newTitle {
		t.Errorf("expected title %q, got %q", newTitle, mutResult.UpdateElement.Title)
	}

	// Restore original title (cleanup — good test hygiene)
	graphqlRequest(t, ts.URL, "user:alice", mutationQuery, map[string]any{
		"uri":   targetURI,
		"title": originalTitle,
	}, nil)

	t.Log("Mutation test passed")
}

// =============================================================================
// TEST 7: User isolation — bob cannot see alice's private spaces
// =============================================================================
// Verifies the permission system: bob has access to space:acme-projects only,
// NOT to space:acme-hr (which only alice and charlie can see).
func TestUserIsolation(t *testing.T) {
	db := openTestDB(t)
	ts := testServer(t, db)

	// Query as bob
	var bobResult struct {
		Elements struct {
			Edges []struct {
				Node struct {
					SpaceURI string `json:"space_uri"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}
	graphqlRequest(t, ts.URL, "user:bob",
		`query { elements(first: 100) { edges { node { space_uri } } } }`,
		nil, &bobResult)

	// Bob should only see elements from space:acme-projects
	for _, edge := range bobResult.Elements.Edges {
		if edge.Node.SpaceURI != "space:acme-projects" {
			t.Errorf("bob got an element from %s which he should not access", edge.Node.SpaceURI)
		}
	}

	t.Logf("Isolation test: bob sees %d elements, all from allowed spaces", len(bobResult.Elements.Edges))
}

// func TestSubscription(t *testing.T) {
// 	db := openTestDB(t)
// 	ts := testServer(t, db)

// 	// Convert http://127.0.0.1:PORT to ws://127.0.0.1:PORT/graphql
// 	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/graphql"

// 	// Connect via WebSocket
// 	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
// 	if err != nil {
// 		t.Fatalf("websocket dial: %v", err)
// 	}
// 	defer ws.Close()

// 	// Step 1: send connection_init with auth payload
// 	// This is the graphql-transport-ws protocol — the server's InitFunc
// 	// reads "X-User-ID" from this payload
// 	err = ws.WriteJSON(map[string]any{
// 		"type": "connection_init",
// 		"payload": map[string]any{
// 			"X-User-ID": "user:alice",
// 		},
// 	})
// 	if err != nil {
// 		t.Fatalf("sending connection_init: %v", err)
// 	}

// 	// Step 2: expect connection_ack back from the server
// 	var ack map[string]any
// 	if err := ws.ReadJSON(&ack); err != nil {
// 		t.Fatalf("reading connection_ack: %v", err)
// 	}
// 	if ack["type"] != "connection_ack" {
// 		t.Fatalf("expected connection_ack, got %v", ack["type"])
// 	}

// 	// Step 3: send the subscribe message
// 	err = ws.WriteJSON(map[string]any{
// 		"id":   "1",
// 		"type": "subscribe",
// 		"payload": map[string]any{
// 			"query": `subscription { elementUpdated { uri title } }`,
// 		},
// 	})
// 	if err != nil {
// 		t.Fatalf("sending subscribe: %v", err)
// 	}

// 	// Step 4: trigger a mutation in a goroutine after a short delay
// 	// The delay ensures the subscription is fully registered before the
// 	// mutation fires, otherwise we might miss the event
// 	go func() {
// 		time.Sleep(200 * time.Millisecond)
// 		graphqlRequest(t, ts.URL, "user:alice",
// 			`mutation { updateElement(uri: "element:project-1", title: "subscription test") { uri } }`,
// 			nil, nil,
// 		)
// 	}()

// 	// Step 5: wait for the next message from the subscription
// 	// Set a deadline so the test doesn't hang forever if something goes wrong
// 	ws.SetReadDeadline(time.Now().Add(5 * time.Second))

// 	var msg map[string]any
// 	if err := ws.ReadJSON(&msg); err != nil {
// 		t.Fatalf("reading subscription message: %v", err)
// 	}

// 	if msg["type"] != "next" {
// 		t.Fatalf("expected next, got %v", msg["type"])
// 	}

// 	t.Logf("subscription received: %v", msg)
// }

// openTestDB and testServer are defined in e2e_test.go
// graphqlRequest is defined in e2e_test.go
