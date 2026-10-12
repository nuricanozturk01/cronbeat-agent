package k8s

import (
	"sort"
	"sync"
)

const (
	defaultMaxJobs        = 5000
	defaultMaxPodsPerJob  = 20
	trackerKeyPartsJoiner = "/"
)

// Tracker keeps the last known abnormal container states per Job so that the
// reason of a failure can still be reported after the pods were deleted
// (ttlSecondsAfterFinished). Memory is bounded: at most maxJobs jobs with
// maxPods pods each; entries are evicted when the Job is deleted and, when the
// cap is hit, least recently updated jobs go first.
type Tracker struct {
	mu      sync.Mutex
	jobs    map[string]*jobEntry
	seq     uint64
	maxJobs int
	maxPods int
}

type jobEntry struct {
	lastSeq uint64
	pods    map[string]*podEntry
}

type podEntry struct {
	seq    uint64
	states []ContainerState
}

// NewTracker creates a tracker with the default caps.
func NewTracker() *Tracker {
	return newTracker(defaultMaxJobs, defaultMaxPodsPerJob)
}

func newTracker(maxJobs, maxPods int) *Tracker {
	return &Tracker{jobs: map[string]*jobEntry{}, maxJobs: maxJobs, maxPods: maxPods}
}

func jobKey(namespace, job string) string {
	return namespace + trackerKeyPartsJoiner + job
}

// Update records the current abnormal states of a pod of the given job. An
// empty states slice clears what was known for that pod.
func (t *Tracker) Update(namespace, job, pod string, states []ContainerState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := jobKey(namespace, job)
	entry := t.jobs[key]
	if entry == nil {
		if len(states) == 0 {
			return
		}
		entry = &jobEntry{pods: map[string]*podEntry{}}
		t.jobs[key] = entry
	}
	t.seq++
	entry.lastSeq = t.seq
	if len(states) == 0 {
		delete(entry.pods, pod)
	} else {
		entry.pods[pod] = &podEntry{seq: t.seq, states: append([]ContainerState(nil), states...)}
	}
	for len(entry.pods) > t.maxPods {
		t.evictOldestPod(entry)
	}
	for len(t.jobs) > t.maxJobs {
		t.evictOldestJob()
	}
}

func (t *Tracker) evictOldestPod(e *jobEntry) {
	var oldest string
	var oldestSeq uint64
	first := true
	for name, p := range e.pods {
		if first || p.seq < oldestSeq {
			oldest, oldestSeq, first = name, p.seq, false
		}
	}
	delete(e.pods, oldest)
}

func (t *Tracker) evictOldestJob() {
	var oldest string
	var oldestSeq uint64
	first := true
	for k, e := range t.jobs {
		if first || e.lastSeq < oldestSeq {
			oldest, oldestSeq, first = k, e.lastSeq, false
		}
	}
	delete(t.jobs, oldest)
}

// States returns the known states of the job, most recently updated pod first.
func (t *Tracker) States(namespace, job string) []ContainerState {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.jobs[jobKey(namespace, job)]
	if entry == nil {
		return nil
	}
	pods := make([]*podEntry, 0, len(entry.pods))
	for _, p := range entry.pods {
		pods = append(pods, p)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].seq > pods[j].seq })
	var out []ContainerState
	for _, p := range pods {
		out = append(out, p.states...)
	}
	return out
}

// Forget drops everything known about the job (call when the Job is deleted).
func (t *Tracker) Forget(namespace, job string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.jobs, jobKey(namespace, job))
}

// Len returns the number of tracked jobs.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.jobs)
}
