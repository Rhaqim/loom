module github.com/rhaqim/story-api

go 1.26.0

require (
	github.com/google/uuid v1.6.0
	github.com/lib/pq v1.12.3
	github.com/rhaqim/loom v0.0.0
	golang.org/x/crypto v0.24.0
)

replace github.com/rhaqim/loom => ../..
