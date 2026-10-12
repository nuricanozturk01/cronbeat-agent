package syncer

import (
	"sync"

	"cronbeat.dev/cronbeat-agent/internal/api"
)

// KnownSet is the set of CronJobs the API has a monitor for, as reported by
// the last successful sync. The API ignores events of other CronJobs, so the
// processor holds those back until a sync reports them.
type KnownSet struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

// NewKnownSet creates an empty set: nothing is known before the first sync.
func NewKnownSet() *KnownSet {
	return &KnownSet{set: map[string]struct{}{}}
}

func knownKey(namespace, name string) string { return namespace + "/" + name }

// Replace swaps the whole set for what the API reported.
func (k *KnownSet) Replace(refs []api.MonitorRef) {
	next := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		next[knownKey(r.Namespace, r.Name)] = struct{}{}
	}
	k.mu.Lock()
	k.set = next
	k.mu.Unlock()
}

// Has reports whether the API knows the CronJob.
func (k *KnownSet) Has(namespace, name string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, ok := k.set[knownKey(namespace, name)]
	return ok
}

// Len returns the number of known CronJobs.
func (k *KnownSet) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.set)
}
