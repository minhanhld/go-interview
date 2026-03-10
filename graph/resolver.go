package graph

import (
	"database/sql"
	"sync"

	"github.com/example/ds-technical-assessment/graph/model"
)

//go:generate go tool gqlgen generate

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct{
	database	*sql.DB
	subscribers	map[chan *model.Element]struct{}
	mu 			sync.Mutex
}

func NewResolver(db *sql.DB) *Resolver {
	return &Resolver{
		database: db,
		subscribers: make(map[chan *model.Element]struct{}), 
	}
}
