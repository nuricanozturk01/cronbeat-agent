package k8s

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func annotated(annotations map[string]string) *batchv1.CronJob {
	cj := cronJob("backup", "*/5 * * * *")
	cj.Annotations = annotations
	return cj
}

func TestAnnotationsSetGraceAndMaxRuntime(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		grace       int
		maxRuntime  int
	}{
		{"none", nil, 0, 0},
		{"minutes", map[string]string{"cronbeat.dev/grace": "10m"}, 600, 0},
		{"plain seconds", map[string]string{"cronbeat.dev/grace": "90"}, 90, 0},
		{"mixed units", map[string]string{"cronbeat.dev/grace": "1h30m"}, 5400, 0},
		{"days", map[string]string{"cronbeat.dev/grace": "2d"}, 172800, 0},
		{"both", map[string]string{"cronbeat.dev/grace": " 5m ", "cronbeat.dev/max-runtime": "30m"}, 300, 1800},
		{"upper bound", map[string]string{"cronbeat.dev/grace": "7d", "cronbeat.dev/max-runtime": "24h"}, 604800, 86400},
		{"foreign annotations are ignored", map[string]string{"other.io/grace": "10m"}, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ToAPICronJob(annotated(tc.annotations))
			if got.GraceSeconds != tc.grace || got.MaxRuntimeSeconds != tc.maxRuntime {
				t.Fatalf("grace=%d maxRuntime=%d, want %d and %d", got.GraceSeconds, got.MaxRuntimeSeconds, tc.grace, tc.maxRuntime)
			}
			if got.Problem != "" || got.Ignored {
				t.Fatalf("unexpected problem %q ignored=%v", got.Problem, got.Ignored)
			}
		})
	}
}

func TestInvalidAnnotationsAreIgnoredAndReported(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantInProb  string
	}{
		{"not a duration", map[string]string{"cronbeat.dev/grace": "soon"}, "cronbeat.dev/grace"},
		{"zero grace", map[string]string{"cronbeat.dev/grace": "0s"}, "cronbeat.dev/grace"},
		{"grace above 7 days", map[string]string{"cronbeat.dev/grace": "8d"}, "7 days"},
		{"negative", map[string]string{"cronbeat.dev/grace": "-5m"}, "cronbeat.dev/grace"},
		{"max runtime above 24 hours", map[string]string{"cronbeat.dev/max-runtime": "25h"}, "24 hours"},
		{"ignore must be a boolean", map[string]string{"cronbeat.dev/ignore": "yes"}, "cronbeat.dev/ignore"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ToAPICronJob(annotated(tc.annotations))
			if got.GraceSeconds != 0 || got.MaxRuntimeSeconds != 0 || got.Ignored {
				t.Fatalf("an invalid annotation must have no effect: %+v", got)
			}
			if got.Problem == "" || !contains(got.Problem, tc.wantInProb) {
				t.Fatalf("problem = %q, want it to mention %q", got.Problem, tc.wantInProb)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestIgnoreAnnotationIsCaseInsensitive(t *testing.T) {
	for _, v := range []string{"true", "True", "TRUE", " true "} {
		if !ToAPICronJob(annotated(map[string]string{"cronbeat.dev/ignore": v})).Ignored {
			t.Fatalf("%q must ignore the CronJob", v)
		}
	}
	if ToAPICronJob(annotated(map[string]string{"cronbeat.dev/ignore": "false"})).Ignored {
		t.Fatal("false must not ignore")
	}
}

func TestIgnoredCronJobsAreNeitherSyncedNorCounted(t *testing.T) {
	client := fake.NewSimpleClientset(
		cronJob("kept", "* * * * *"),
		annotated(map[string]string{"cronbeat.dev/ignore": "true"}),
	)
	w, _ := startWatcher(t, client, newSink(), nil, NewTracker())

	list := w.CronJobs()
	if len(list) != 1 || list[0].Name != "kept" {
		t.Fatalf("only the monitored CronJob may be synced, got %+v", list)
	}
	if w.CronJobCount() != 1 {
		t.Fatalf("count = %d, ignored CronJobs must not be counted", w.CronJobCount())
	}
}

func TestChangingAnAnnotationNotifiesTheSyncLoop(t *testing.T) {
	cj := annotated(map[string]string{"cronbeat.dev/grace": "5m"})
	client := fake.NewSimpleClientset(cj)
	w, _ := startWatcher(t, client, newSink(), nil, NewTracker())
	drained(w.CronJobChanges())

	changed := cj.DeepCopy()
	changed.Annotations = map[string]string{"cronbeat.dev/grace": "15m"}
	if _, err := client.BatchV1().CronJobs("default").Update(t.Context(), changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := drained(w.CronJobChanges()); n != 1 {
		t.Fatalf("a changed annotation must notify once, got %d", n)
	}
	if got := w.CronJobs()[0].GraceSeconds; got != 900 {
		t.Fatalf("grace = %d", got)
	}
}

func TestUnchangedAnnotationsDoNotCauseSyncs(t *testing.T) {
	a := ToAPICronJob(annotated(map[string]string{"cronbeat.dev/grace": "5m"}))
	b := ToAPICronJob(annotated(map[string]string{"cronbeat.dev/grace": "300"}))
	if a != b {
		t.Fatal("5m and 300 are the same setting")
	}
}
