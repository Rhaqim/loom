module github.com/rhaqim/loom-dnd

go 1.26.0

require (
	github.com/google/uuid v1.6.0
	github.com/lib/pq v1.12.3
	github.com/rhaqim/loom v0.0.2
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/anthropics/anthropic-sdk-go/v2 v2.9.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.6.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/pb33f/go-yaml v0.1.1 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.2 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.20.2 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.3.0 // indirect
	github.com/tidwall/sjson v1.2.9 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/rhaqim/loom/generator/anthropic v0.0.0
	github.com/spf13/pflag v1.1.0 // indirect
)

replace github.com/rhaqim/loom => ../..

replace github.com/rhaqim/loom/generator/anthropic => ../../generator/anthropic
