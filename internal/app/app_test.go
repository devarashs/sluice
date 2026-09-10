package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devarashs/sluice/internal/admin"
)

func commonWithAdmin(t *testing.T, addr string) Common {
	t.Helper()
	common := Common{Admin: admin.Config{ListenAddr: addr}}
	if err := common.Validate(); err != nil {
		t.Fatal(err)
	}
	return common
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}

func TestRunContextServesAdminReflectsReadinessAndExitsCleanly(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	adminAddr := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	go func() {
		exit <- RunContext(ctx, commonWithAdmin(t, "127.0.0.1:0"), &logs, func(ctx context.Context, deps Deps) error {
			adminAddr <- deps.AdminAddr
			if deps.Ready.Check() == nil {
				t.Error("mode should start not ready")
			}
			deps.Ready.Set(nil)
			<-ctx.Done()
			deps.Ready.Set(errors.New("stopping"))
			return nil
		})
	}()

	addr := <-adminAddr
	if addr == nil {
		t.Fatal("AdminAddr is nil with the admin listener enabled")
	}
	base := "http://" + addr.String()
	deadline := time.Now().Add(2 * time.Second)
	for getStatus(t, base+"/readyz") != 200 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if getStatus(t, base+"/readyz") != 200 {
		t.Fatal("/readyz never became 200 after the mode set ready")
	}
	if getStatus(t, base+"/healthz") != 200 {
		t.Fatal("/healthz not 200")
	}

	cancel()
	select {
	case code := <-exit:
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0; logs:\n%s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunContext did not return after cancel")
	}
	for _, want := range []string{"starting", "build=", "stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

func TestRunContextReportsMainError(t *testing.T) {
	var logs bytes.Buffer
	common := Common{Admin: admin.Config{Disabled: true}}
	if err := common.Validate(); err != nil {
		t.Fatal(err)
	}
	code := RunContext(context.Background(), common, &logs, func(context.Context, Deps) error {
		return errors.New("listen: address in use")
	})
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(logs.String(), "address in use") {
		t.Fatalf("logs do not name the error:\n%s", logs.String())
	}
}

func TestRunContextTreatsCancellationAsClean(t *testing.T) {
	common := Common{Admin: admin.Config{Disabled: true}}
	if err := common.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code := RunContext(ctx, common, io.Discard, func(ctx context.Context, deps Deps) error {
		if deps.AdminAddr != nil {
			t.Error("AdminAddr should be nil when admin is disabled")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 for a cancelled main", code)
	}
}

func TestRunContextFailsFastWhenAdminCannotBind(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	var logs bytes.Buffer
	ran := false
	code := RunContext(context.Background(), commonWithAdmin(t, occupied.Addr().String()), &logs, func(context.Context, Deps) error {
		ran = true
		return nil
	})
	if code != ExitError || ran {
		t.Fatalf("exit = %d, main ran = %v; want exit 1 without running main", code, ran)
	}
	if !strings.Contains(logs.String(), "admin listener failed") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestReadinessStartsNotReady(t *testing.T) {
	r := NewReadiness()
	if r.Check() == nil {
		t.Fatal("new readiness should not be ready")
	}
	r.Set(nil)
	if r.Check() != nil {
		t.Fatal("Set(nil) should be ready")
	}
}
