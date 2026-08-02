//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// cdpReadTimeout bounds every wait for a CDP response. A service-worker
// target can be destroyed between attach and evaluate — the helper's own
// reload (setEnabled off/on) does exactly that mid-poll, and after it the
// target list can still name the dead worker — and Chrome never answers the
// in-flight command of a dead session. Without a deadline the suite hangs
// to the 20-minute panic; with a long one, a poll window is spent on a few
// dead sessions instead of many fresh attempts, so keep it short.
// A variable only so the framing test can shrink it.
var cdpReadTimeoutForTest = 5 * time.Second

// cdpPipe is a minimal Chrome DevTools Protocol client over
// --remote-debugging-pipe (fd 3 = commands to Chrome, fd 4 = responses,
// null-byte-delimited JSON). The pipe transport is required for the
// experimental Extensions.loadUnpacked command, which the E2E test uses to
// install extensions exactly like a human clicking "Load unpacked" —
// --load-extension would not do: command-line extensions cannot be disabled,
// which would defeat the setEnabled-based reload under test.
type cdpPipe struct {
	w  *os.File
	r  *bufio.Reader
	rf *os.File
	// partial holds the bytes of a frame that a read deadline interrupted.
	// Without it a timeout would drop them, and every later read would
	// start mid-frame — one timeout would corrupt the stream for good.
	partial []byte
	seq     int
}

// newCDPPipe returns the two files to pass as ExtraFiles (fd 3 and 4) and
// the client using the parent-side ends.
func newCDPPipe() (extraFiles []*os.File, client *cdpPipe, err error) {
	chromeRead, ourWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	ourRead, chromeWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	return []*os.File{chromeRead, chromeWrite}, &cdpPipe{
		w:  ourWrite,
		r:  bufio.NewReader(ourRead),
		rf: ourRead,
	}, nil
}

// call sends one browser-level CDP command and waits for its response,
// skipping interleaved events.
func (c *cdpPipe) call(method string, params any) (json.RawMessage, error) {
	return c.callSession("", method, params)
}

// callSession is call scoped to an attached target session (flat mode).
func (c *cdpPipe) callSession(sessionID, method string, params any) (json.RawMessage, error) {
	c.seq++
	req := map[string]any{"id": c.seq, "method": method}
	if params != nil {
		req["params"] = params
	}
	if sessionID != "" {
		req["sessionId"] = sessionID
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.w.Write(append(payload, 0)); err != nil {
		return nil, fmt.Errorf("write CDP command: %w", err)
	}
	// The deadline is cleared on return: a response that arrives after a
	// timeout is skipped by the id check on the next call, not misread.
	if err := c.rf.SetReadDeadline(time.Now().Add(cdpReadTimeoutForTest)); err == nil {
		defer c.rf.SetReadDeadline(time.Time{})
	}
	for {
		raw, err := c.readFrame()
		if err != nil {
			return nil, fmt.Errorf("read CDP response to %s: %w", method, err)
		}
		var resp struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw[:len(raw)-1], &resp); err != nil {
			continue
		}
		if resp.ID != c.seq {
			continue // event or stale response
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// readFrame returns one null-terminated frame, carrying any bytes a read
// deadline interrupted over to the next call: bufio hands back what it read
// before the error, and dropping that would leave every later read starting
// mid-frame.
func (c *cdpPipe) readFrame() ([]byte, error) {
	chunk, err := c.r.ReadBytes(0)
	c.partial = append(c.partial, chunk...)
	if err != nil {
		return nil, err
	}
	frame := c.partial
	c.partial = nil
	return frame, nil
}

// loadUnpacked installs an unpacked extension from dir and returns its ID.
func (c *cdpPipe) loadUnpacked(dir string) (string, error) {
	res, err := c.call("Extensions.loadUnpacked", map[string]string{"path": dir})
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// serviceWorkerTarget returns the target id of the running service worker of
// the given extension, or "" when it is not currently running.
func (c *cdpPipe) serviceWorkerTarget(extID string) (string, error) {
	if _, err := c.call("Target.setDiscoverTargets", map[string]bool{"discover": true}); err != nil {
		return "", err
	}
	res, err := c.call("Target.getTargets", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	prefix := "chrome-extension://" + extID + "/"
	for _, ti := range out.TargetInfos {
		if ti.Type == "service_worker" && len(ti.URL) > len(prefix) && ti.URL[:len(prefix)] == prefix {
			return ti.TargetID, nil
		}
	}
	return "", nil
}

// evalInServiceWorker evaluates expr inside the extension's service worker
// and returns the result as a string.
func (c *cdpPipe) evalInServiceWorker(extID, expr string) (string, error) {
	target, err := c.serviceWorkerTarget(extID)
	if err != nil {
		return "", err
	}
	if target == "" {
		return "", fmt.Errorf("service worker of %s is not running", extID)
	}
	res, err := c.call("Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})
	if err != nil {
		return "", err
	}
	var attach struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &attach); err != nil {
		return "", err
	}
	defer c.call("Target.detachFromTarget", map[string]string{"sessionId": attach.SessionID})

	res, err = c.callSession(attach.SessionID, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	if out.ExceptionDetails != nil {
		return "", fmt.Errorf("evaluate: %s", out.ExceptionDetails.Text)
	}
	return fmt.Sprint(out.Result.Value), nil
}

func (c *cdpPipe) close() {
	c.w.Close()
	c.rf.Close()
}
