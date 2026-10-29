// Package service — Node cache with singleflight to prevent thundering herd.
//
// When the cache expires, concurrent requests that miss the cache would all
// simultaneously fetch and parse the upstream YAML. The singleflight pattern
// collapses these into one in-flight call; others wait for its result.
// Reference: golang.org/x/sync/singleflight — bundled in-process here to
// avoid an external dependency.

package service

import (
	"sync"
	"time"

	"substore-ui-go/internal/model"
)

// nodeCacheEntry holds cached nodes with their fetch timestamp.
type nodeCacheEntry struct {
	nodes     []model.Node
	fetchedAt time.Time
}

var (
	nodeCacheMu   sync.RWMutex
	nodeCacheData *nodeCacheEntry

	// singleflight: only one goroutine fetches nodes at a time.
	sfMu      sync.Mutex
	sfCallPtr *sfCall
)

// sfCall represents an in-flight node fetch.
type sfCall struct {
	done   chan struct{}
	nodes  []model.Node
	err    error
}

const nodeCacheTTL = 30 * time.Second

// GetCachedNodes returns a copy of cached nodes if not expired, nil otherwise.
func GetCachedNodes() []model.Node {
	nodeCacheMu.RLock()
	defer nodeCacheMu.RUnlock()
	if nodeCacheData == nil || time.Since(nodeCacheData.fetchedAt) > nodeCacheTTL {
		return nil
	}
	// Return a shallow copy to prevent callers from mutating the slice header.
	nodes := make([]model.Node, len(nodeCacheData.nodes))
	copy(nodes, nodeCacheData.nodes)
	return nodes
}

// SetCachedNodes stores nodes in the cache with current timestamp.
func SetCachedNodes(nodes []model.Node) {
	nodeCacheMu.Lock()
	defer nodeCacheMu.Unlock()
	nodeCacheData = &nodeCacheEntry{
		nodes:     nodes,
		fetchedAt: time.Now(),
	}
}

// InvalidateNodeCache clears the cache (call after config changes).
func InvalidateNodeCache() {
	nodeCacheMu.Lock()
	defer nodeCacheMu.Unlock()
	nodeCacheData = nil
}

// FetchNodesWithSingleflight fetches nodes from upstream using singleflight.
// Only one in-flight call happens at a time; concurrent callers wait for the
// first one's result. This eliminates the thundering herd on cache expiry.
//
// fetchFn is the function that performs the actual upstream fetch + parse.
func FetchNodesWithSingleflight(fetchFn func() ([]model.Node, error)) ([]model.Node, error) {
	// Try cache first (read path, no lock needed beyond RLock).
	if nodes := GetCachedNodes(); nodes != nil {
		return nodes, nil
	}

	sfMu.Lock()
	if sfCallPtr != nil {
		// Another goroutine is already fetching; wait for its result.
		call := sfCallPtr
		sfMu.Unlock()
		<-call.done
		return call.nodes, call.err
	}
	// We're the leader; start the fetch.
	call := &sfCall{done: make(chan struct{})}
	sfCallPtr = call
	sfMu.Unlock()

	// Execute the fetch.
	nodes, err := fetchFn()

	// Store result and wake up waiters.
	call.nodes = nodes
	call.err = err
	close(call.done)

	sfMu.Lock()
	sfCallPtr = nil
	sfMu.Unlock()

	if err == nil && nodes != nil {
		SetCachedNodes(nodes)
	}
	return nodes, err
}
