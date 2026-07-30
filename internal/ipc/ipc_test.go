package ipc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func startTestServer(t *testing.T, h Handler) {
	t.Helper()
	// Unix socket paths are length-limited (~104 bytes on macOS); use a short
	// tmp dir instead of t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "cepm-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}

	l, err := Listen(filepath.Join(dir, "run", "cepm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Serve(ctx, l, h)
}

func TestPingRoundTrip(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		if req.Cmd != CmdPing {
			return Response{Error: "unexpected cmd"}
		}
		return Response{OK: true, Host: &HostInfo{Version: "test", PID: 42, Leader: true}}
	})
	info, err := Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.PID != 42 || !info.Leader {
		t.Errorf("unexpected host info: %+v", info)
	}
}

func TestReloadRoundTrip(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		results := make([]ReloadResult, len(req.IDs))
		for i, id := range req.IDs {
			results[i] = ReloadResult{ID: id, Status: StatusReloaded}
		}
		return Response{OK: true, Results: results}
	})
	results, err := Reload(context.Background(), []string{"aaa", "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ID != "aaa" || results[1].Status != StatusReloaded {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestHostErrorSurfaces(t *testing.T) {
	startTestServer(t, func(ctx context.Context, req Request) Response {
		return Response{Error: "helper not connected"}
	})
	if _, err := Reload(context.Background(), []string{"aaa"}); err == nil {
		t.Error("expected error from host")
	}
}

func TestDialFailsFastWhenNoHost(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled context → no retry loop
	if _, err := Ping(ctx); err == nil {
		t.Error("expected ErrHostNotRunning")
	}
}
