package e2e

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// procUsage reads a process's resident memory (kB) and CPU time from /proc.
func procUsage(t *testing.T, pid int) (rssKB int, cpu time.Duration) {
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "VmRSS:" {
			rssKB, _ = strconv.Atoi(f[1])
		}
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+2:])
	utime, _ := strconv.Atoi(f[11])
	stime, _ := strconv.Atoi(f[12])
	return rssKB, time.Duration(utime+stime) * 10 * time.Millisecond // USER_HZ is 100 on Linux
}

// TestIdleUsage measures the bridge signed in to one account with one encrypted DM, syncing and idle.
//
//	E2E_MEASURE=5m TUWUNEL=... go test ./tests/e2e -run TestIdleUsage -v
func TestIdleUsage(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("E2E_MEASURE"))
	if err != nil {
		t.Skip("E2E_MEASURE is not set")
	}
	e := setup(t)
	if _, done, err := e.login(); err != nil || done.Type != "complete" {
		t.Fatalf("login: %v", err)
	}
	if err := e.provision("POST", "/v3/create_dm/"+url.PathEscape(string(e.bea.client.UserID)), map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second) // let start-up work settle before the baseline
	_, cpu0 := procUsage(t, e.br.Process.Pid)
	time.Sleep(d)
	rss, cpu1 := procUsage(t, e.br.Process.Pid)
	perHour := time.Duration(float64(cpu1-cpu0) * float64(time.Hour) / float64(d))
	t.Logf("RSS %d MB, CPU %.2f s per hour while syncing", rss/1024, perHour.Seconds())
}
