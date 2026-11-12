package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	c := New()
	c.Add(RunnersStarted, "srg-vps", 2)
	c.Add(RunnersStarted, "srg-vps", 1)
	c.Set(RunnersActive, "srg-vps", 1)
	c.Set(HostSlotsMax, "", 4)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE scalerunner_runners_started_total counter",
		`scalerunner_runners_started_total{scale_set="srg-vps"} 3`,
		"# TYPE scalerunner_runners_active gauge",
		`scalerunner_runners_active{scale_set="srg-vps"} 1`,
		"scalerunner_host_slots_max 4",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if c.Get(RunnersStarted, "srg-vps") != 3 || c.Get(RunnersFailed, "x") != 0 {
		t.Error("Get wrong")
	}
}
