package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"cronbeat.dev/cronbeat-agent/internal/api"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	labelJobName       = "batch.kubernetes.io/job-name"
	labelJobNameLegacy = "job-name"
)

// Watcher runs the CronJob, Job and Pod informers.
//
// It is built to stay small on large clusters: Pods are listed and watched
// with a label selector, so the API server only sends the pods of Jobs, and
// every object is cut down to the fields the agent reads before it is cached.
type Watcher struct {
	factories []informers.SharedInformerFactory
	cronJobs  []batchlisters.CronJobLister
	cjInfs    []cache.SharedIndexInformer
	jobInfs   []cache.SharedIndexInformer
	podInfs   []cache.SharedIndexInformer
	cacheSync []cache.InformerSynced
	proc      *JobProcessor
	tracker   *Tracker
	log       *slog.Logger
	changes   chan struct{}
	initErr   error
}

// NewWatcher builds informers for the namespaces (empty means all namespaces).
// No resync period is used: events are driven by watch notifications only.
func NewWatcher(client kubernetes.Interface, namespaces []string, proc *JobProcessor, tracker *Tracker, log *slog.Logger) *Watcher {
	w := &Watcher{proc: proc, tracker: tracker, log: log, changes: make(chan struct{}, 1)}
	scopes := namespaces
	if len(scopes) == 0 {
		scopes = []string{corev1.NamespaceAll}
	}
	jobPodsOnly := informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = labelJobName })
	for _, ns := range scopes {
		batchFactory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(ns))
		podFactory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(ns), jobPodsOnly)
		cjInf := batchFactory.Batch().V1().CronJobs()
		cjInformer := cjInf.Informer()
		jobInf := batchFactory.Batch().V1().Jobs().Informer()
		podInf := podFactory.Core().V1().Pods().Informer()
		w.setTransform(cjInformer, TrimCronJob)
		w.setTransform(jobInf, TrimJob)
		w.setTransform(podInf, TrimPod)

		w.factories = append(w.factories, batchFactory, podFactory)
		w.cronJobs = append(w.cronJobs, cjInf.Lister())
		w.cjInfs = append(w.cjInfs, cjInformer)
		w.jobInfs = append(w.jobInfs, jobInf)
		w.podInfs = append(w.podInfs, podInf)
		w.cacheSync = append(w.cacheSync, cjInformer.HasSynced, jobInf.HasSynced, podInf.HasSynced)
	}
	return w
}

func (w *Watcher) setTransform(inf cache.SharedIndexInformer, trim func(any) any) {
	if err := inf.SetTransform(asTransform(trim)); err != nil && w.initErr == nil {
		w.initErr = fmt.Errorf("informer transform: %w", err)
	}
}

// Start starts the informers (they run until ctx is done), waits until all
// caches are synced (at most syncTimeout, 0 means no limit) and only then
// attaches the Job handler, so the initial Job list is processed with a fully
// populated pod tracker. On timeout it returns context.DeadlineExceeded.
func (w *Watcher) Start(ctx context.Context, syncTimeout time.Duration) error {
	if w.initErr != nil {
		return w.initErr
	}
	for _, p := range w.podInfs {
		if _, err := p.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { w.onPod(o) },
			UpdateFunc: func(_, o any) { w.onPod(o) },
		}); err != nil {
			return fmt.Errorf("pod handler: %w", err)
		}
	}
	for _, f := range w.factories {
		f.Start(ctx.Done())
	}
	waitCtx := ctx
	if syncTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, syncTimeout)
		defer cancel()
	}
	if !cache.WaitForCacheSync(waitCtx.Done(), w.cacheSync...) {
		if err := waitCtx.Err(); err != nil {
			return err
		}
		return errors.New("informer caches did not sync")
	}
	for _, j := range w.jobInfs {
		if _, err := j.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(o any) { w.onJob(o) },
			UpdateFunc: func(_, o any) { w.onJob(o) },
			DeleteFunc: w.onJobDelete,
		}); err != nil {
			return fmt.Errorf("job handler: %w", err)
		}
	}
	for _, c := range w.cjInfs {
		if _, err := c.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
			// the first sync sends the initial list itself
			AddFunc:    func(_ any, initial bool) { w.onCronJobAdded(initial) },
			UpdateFunc: w.onCronJobUpdated,
			DeleteFunc: func(any) { w.notifyCronJobs() },
		}); err != nil {
			return fmt.Errorf("cronjob handler: %w", err)
		}
	}
	return nil
}

// Stop waits for informer goroutines to end (ctx passed to Start must be done).
func (w *Watcher) Stop() {
	for _, f := range w.factories {
		f.Shutdown()
	}
}

// CronJobChanges signals that a CronJob was created, deleted or changed in a
// way that matters for the sync (schedule, time zone, suspend). Signals are
// coalesced: a burst of changes leaves at most one pending signal.
func (w *Watcher) CronJobChanges() <-chan struct{} { return w.changes }

// CronJobs lists all known CronJobs as sync payload entries, sorted.
func (w *Watcher) CronJobs() []api.CronJob {
	out := []api.CronJob{}
	for _, l := range w.cronJobs {
		items, err := l.List(labels.Everything())
		if err != nil {
			w.log.Error("listing cronjobs from cache failed", "error", err.Error())
			continue
		}
		for _, cj := range items {
			if entry := ToAPICronJob(cj); !entry.Ignored {
				out = append(out, entry)
			}
		}
	}
	sortCronJobs(out)
	return out
}

// CronJobCount returns the number of CronJobs that are monitored (the ones
// marked cronbeat.dev/ignore are not). It builds no payload, so it is cheap
// enough for the heartbeat.
func (w *Watcher) CronJobCount() int {
	n := 0
	for _, inf := range w.cjInfs {
		for _, o := range inf.GetStore().List() {
			if cj, ok := o.(*batchv1.CronJob); ok && !ignoredByAnnotation(cj) {
				n++
			}
		}
	}
	return n
}

// ReplayJobs runs every cached Job through the processor again. The sink
// de-duplicates, so only what was held back (an unknown CronJob, a full queue)
// is sent now. Call it after a sync made new CronJobs known.
func (w *Watcher) ReplayJobs() {
	for _, inf := range w.jobInfs {
		for _, o := range inf.GetStore().List() {
			w.onJob(o)
		}
	}
}

func (w *Watcher) onCronJobAdded(initial bool) {
	if !initial {
		w.notifyCronJobs()
	}
}

func (w *Watcher) onCronJobUpdated(oldObj, newObj any) {
	oldCJ, okOld := oldObj.(*batchv1.CronJob)
	newCJ, okNew := newObj.(*batchv1.CronJob)
	if !okOld || !okNew || ToAPICronJob(oldCJ).Stable() != ToAPICronJob(newCJ).Stable() {
		w.notifyCronJobs()
	}
}

func (w *Watcher) notifyCronJobs() {
	select {
	case w.changes <- struct{}{}:
	default:
	}
}

func (w *Watcher) onJob(o any) {
	if job, ok := o.(*batchv1.Job); ok {
		w.log.Debug("job event", "namespace", job.Namespace, "job", job.Name)
		w.proc.OnJob(job)
	}
}

func (w *Watcher) onJobDelete(o any) {
	if tomb, ok := o.(cache.DeletedFinalStateUnknown); ok {
		o = tomb.Obj
	}
	if job, ok := o.(*batchv1.Job); ok {
		w.proc.OnJobDeleted(job)
	}
}

func (w *Watcher) onPod(o any) {
	pod, ok := o.(*corev1.Pod)
	if !ok {
		return
	}
	// A terminating pod reports kill signals (exit 137/143) that would mask
	// the real cause; keep what was known before.
	if pod.DeletionTimestamp != nil {
		return
	}
	job := PodJobName(pod)
	if job == "" {
		return
	}
	w.tracker.Update(pod.Namespace, job, pod.Name, ExtractStates(pod))
}

// PodJobName returns the name of the Job a pod belongs to ("" if none).
func PodJobName(pod *corev1.Pod) string {
	if v := pod.Labels[labelJobName]; v != "" {
		return v
	}
	if v := pod.Labels[labelJobNameLegacy]; v != "" {
		return v
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" {
			return ref.Name
		}
	}
	return ""
}
