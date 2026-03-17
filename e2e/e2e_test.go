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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/example/ds-technical-assessment/src/server"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
)


// testServer starts the real HTTP server using Go's httptest.NewServer.
func testServer(t *testing.T, database *sql.DB) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/graphql", server.BuildHandler(database))
	mux.Handle("/health", server.BuildHealthHandler(database))
	testServer := httptest.NewServer(mux)
	t.Cleanup(testServer.Close)
	return testServer
}

// openTestDB opens a connection to the test database.
// It uses the same default connection string as the main app.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	connStr := os.Getenv("DATABASE_URL")
	if (connStr == ""){
		connStr = "postgres://postgres:postgres@localhost:5432/technical_assessment?sslmode=disable"
	}
	database, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("opening test DB: %v", err)
		// t.Fatalf logs the message and immediately stops this test.
		// It's like t.Errorf (marks the test as failed) + return.
	}

	// Give the DB 5 seconds to become available (useful in CI environments).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("pinging test DB: %v", err)
	}

	t.Cleanup(func() { database.Close() })
	return database
}

// graphqlRequest sends a GraphQL query/mutation over HTTP POST to the server.
func graphqlRequest(t *testing.T, baseURL string, userID string, query string, variables map[string]any, result any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", userID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("executing request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, respBody)
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		t.Fatalf("decoding response envelope: %v\nBody: %s", err, respBody)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		t.Fatalf("GraphQL errors: %v\nFull response: %s", msgs, respBody)
	}
	if result != nil && envelope.Data != nil {
		if err := json.Unmarshal(envelope.Data, result); err != nil {
			t.Fatalf("decoding response data: %v\nData: %s", err, envelope.Data)
		}
	}
}

// =============================================================================
// TEST 1: Authentication middleware test - Missing "X-User-ID" header
// =============================================================================
// Tests that the auth middleware rejects requests without the header.
func TestAuthMiddleware_MissingHeader(t *testing.T) {
	database := openTestDB(t)
	testServer := testServer(t, database)
	query := `
		{ elements(first: 1) 
			{ edges 
				{ node 
					{ uri 
					} 
				} 
			} 
		}
	`
	body, err := json.Marshal(map[string]any{
		"query": query,
	})
	if err != nil {
	t.Fatalf("failed to marshal request: %v", err)
	}
	resp, err := http.Post(testServer.URL+"/graphql", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}
}

// =============================================================================
// TEST 2: Query test - Basic
// =============================================================================
// This tests the elements query with a valid user and ensures the elements returned aren't in 
// spaces the user shouldn't have access to.
func TestQueryElements_BasicPagination(t *testing.T) {
	database := openTestDB(t)
	testServer := testServer(t, database)
	userID := "user:alice"
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
	graphqlRequest(t, testServer.URL, userID, query, nil, &result)
	if len(result.Elements.Edges) <= 0 || len(result.Elements.Edges) > 5 {
		t.Errorf("expected between 1 and 5 elements, got %d", len(result.Elements.Edges))
	}
	aliceSpaces := map[string]bool{
		"space:acme-projects": true,
		"space:acme-hr":       true,
	}
	for _, edge := range result.Elements.Edges {
		if !aliceSpaces[edge.Node.SpaceURI] {
			t.Errorf("element %s is in space %s which %s should not access", edge.Node.URI, edge.Node.SpaceURI, userID)
		}
	}
	t.Logf("Got %d elements, hasNextPage=%v", len(result.Elements.Edges), result.Elements.PageInfo.HasNextPage)
}

// =============================================================================
// TEST 3: Query test — Cursor-based pagination
// =============================================================================
// Tests fetching the first page, getting its endCursor then getting the next page with it
func TestQueryElements_CursorPagination(t *testing.T) {
	database := openTestDB(t)
	testServer := testServer(t, database)
	userID := "user:alice"
	firstPageQuery := `
		query {
			elements(first: 3) {
				pageInfo { 
					hasNextPage 
					endCursor 
				}
				edges { 
					node { 
						uri 
					} 
				}
			}
		}
	`
	secondPageQuery := `
		query($after: String) {
			elements(first: 3, after: $after) {
				pageInfo { 
					hasNextPage endCursor 
					}
				edges { 
					node { 
						uri 
					} 
				}
			}
		}
	`
	var firstPage struct {
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
	var nextPage struct {
		Elements struct {
			Edges []struct {
				Node struct {
					URI string `json:"uri"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}
	graphqlRequest(t, testServer.URL, userID, firstPageQuery, nil, &firstPage)

	if !firstPage.Elements.PageInfo.HasNextPage {
		t.Skip("not enough elements to test pagination (need > 3)")
	}
	if firstPage.Elements.PageInfo.EndCursor == nil {
		t.Fatal("endCursor should not be nil when hasNextPage is true")
	}

	variables := map[string]any {
		"after": *firstPage.Elements.PageInfo.EndCursor,
	}
	graphqlRequest(t, testServer.URL, userID, secondPageQuery, variables, &nextPage)

	if len(nextPage.Elements.Edges) == 0 {
		t.Error("second page should have elements")
	}
	page1URIs := make(map[string]bool)
	for _, e := range firstPage.Elements.Edges {
		page1URIs[e.Node.URI] = true
	}
	for _, e := range nextPage.Elements.Edges {
		if page1URIs[e.Node.URI] {
			t.Errorf("element %s appeared on both page 1 and page 2", e.Node.URI)
		}
	}
	t.Logf("Page 1: %d elements, Page 2: %d elements — no overlap",
		len(firstPage.Elements.Edges), len(nextPage.Elements.Edges))
}

// =============================================================================
// TEST 4: Query test - Filtering elements by a field value
// =============================================================================
// Tests filtering elements by a field value.
func TestQueryElements_WithFilter(t *testing.T) {
	database := openTestDB(t)
	testServer := testServer(t, database)
	userID := "user:alice"
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
	variables := map[string]any {
		"filter": map[string]any {
			"field_uri":	"field:task-status",
			"value":		"Done",
		},
	}
	graphqlRequest(t, testServer.URL, userID, query, variables, &result)
	for _, edge := range result.Elements.Edges {
		found := false
		for _, fv := range edge.Node.FieldValues {
			if fv.Field.URI == "field:task-status" {
				if value, ok := fv.Value.(string); ok && value == "Done" {
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
// TEST 5: Mutation test - Basic
// =============================================================================
// Tests updating an element's title and verifies the returned value.
func TestMutation_UpdateElement(t *testing.T) {
	database := openTestDB(t)
	testServer := testServer(t, database)
	userID := "user:alice"
	query := `	
		query { 
			elements(first: 1) { 
				edges { 
					node { 
						uri title 
					} 
				} 
			} 
		}
	`
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
	graphqlRequest(t, testServer.URL, userID, query, nil, &listResult)

	if len(listResult.Elements.Edges) == 0 {
		t.Fatalf("no elements available to update for userID : %s", userID)
	}

	targetURI := listResult.Elements.Edges[0].Node.URI
	originalTitle := listResult.Elements.Edges[0].Node.Title
	newTitle := "For testing purposes"
	t.Logf("Updating element %q from %q to %q", targetURI, originalTitle, newTitle)
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

	variables := map[string]any {
		"uri": targetURI,
		"title" : newTitle,
	}

	graphqlRequest(t, testServer.URL, "user:alice", mutationQuery, variables, &mutResult)

	if mutResult.UpdateElement.URI != targetURI {
		t.Errorf("expected URI %q, got %q", targetURI, mutResult.UpdateElement.URI)
	}
	if mutResult.UpdateElement.Title != newTitle {
		t.Errorf("expected title %q, got %q", newTitle, mutResult.UpdateElement.Title)
	}
	variables["title"] = originalTitle;
	graphqlRequest(t, testServer.URL, "user:alice", mutationQuery, variables, nil)
}

// =============================================================================
// TEST 6: Query test - Ensure isolation
// =============================================================================
// Verifies that a user has only access to his spaces and not others'.
func TestQuery_UserIsolation(t *testing.T) {
	database := openTestDB(t)
	testServer := testServer(t, database)
	userID := "user:bob"
	query := `
		query { 
			elements(first: 100) { 
				edges { 
					node { 
						space_uri 
						} 
					} 
				} 
		}
	`
	var bobResult struct {
		Elements struct {
			Edges []struct {
				Node struct {
					SpaceURI string `json:"space_uri"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"elements"`
	}
	graphqlRequest(t, testServer.URL, userID, query, nil, &bobResult)
	for _, edge := range bobResult.Elements.Edges {
		if edge.Node.SpaceURI != "space:acme-projects" {
			t.Errorf("bob got an element from %s which he should not access", edge.Node.SpaceURI)
		}
	}
	t.Logf("Isolation test: bob sees %d elements, all from allowed spaces", len(bobResult.Elements.Edges))
}


// =============================================================================
// TEST 7: Subscription test
// =============================================================================
// This tests the subscription to updates.
// It establishes a WebSocket connection to the server, initializes it with an authentication payload (connection_init), and confirms the server acknowledges the connection. 
// It then starts a subscription for elementUpdated events, triggers a mutation in the background to produce a change and checks that the subscription receives the expected next message within a timeout.
func TestSubscription(t *testing.T) {
	database := openTestDB(t)
	testServer := testServer(t, database)
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http") + "/graphql"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Sec-WebSocket-Protocol": []string{"graphql-transport-ws"},
	})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer ws.Close()
	err = ws.WriteJSON(map[string]any{
		"type": "connection_init",
		"payload": map[string]any{
			"X-User-ID": "user:alice",
		},
	})
	if err != nil {
		t.Fatalf("sending connection_init: %v", err)
	}
	var ack map[string]any
	if err := ws.ReadJSON(&ack); err != nil {
		t.Fatalf("reading connection_ack: %v", err)
	}
	if ack["type"] != "connection_ack" {
		t.Fatalf("expected connection_ack, got %v", ack["type"])
	}
	err = ws.WriteJSON(map[string]any{
		"id":   "1",
		"type": "subscribe",
		"payload": map[string]any{
			"query": `subscription { elementUpdated { uri title } }`,
		},
	})
	if err != nil {
		t.Fatalf("sending subscribe: %v", err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		graphqlRequest(t, testServer.URL, "user:alice",
			`mutation { updateElement(uri: "element:project-1", title: "subscription test") { uri } }`,
			nil, nil,
		)
	}()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var msg map[string]any
	if err := ws.ReadJSON(&msg); err != nil {
		t.Fatalf("reading subscription message: %v", err)
	}
	if msg["type"] != "next" {
		t.Fatalf("expected next, got %v", msg["type"])
	}
	t.Logf("subscription received: %v", msg)
}
