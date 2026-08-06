package adapter

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func testResp(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}
}

func TestReadAndCheckStreamInitialError_NoError(t *testing.T) {
	body := "data: {\"type\":\"message.start\",\"id\":\"m1\"}\n\ndata: {\"type\":\"text.delta\",\"text\":\"hello\"}\n\ndata: {\"type\":\"message.stop\"}\n\n"
	resp := testResp(body)
	b, hit, err := ReadAndCheckStreamInitialError(resp)
	if err != nil || hit || b != nil {
		t.Fatalf("want no error/hit, got err=%v hit=%v", err, hit)
	}
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != body {
		t.Fatalf("body corrupted: %q", rest)
	}
}

func TestReadAndCheckStreamInitialError_FreeLimit(t *testing.T) {
	body := "data: {\"type\":\"error\",\"error\":{\"type\":\"FreeUsageLimitError\",\"message\":\"free limit reached\"}}\n\n"
	resp := testResp(body)
	b, hit, err := ReadAndCheckStreamInitialError(resp)
	if err != nil || !hit {
		t.Fatalf("want hit, got err=%v hit=%v", err, hit)
	}
	if !strings.Contains(string(b), "FreeUsageLimitError") {
		t.Fatalf("returned body missing error: %q", b)
	}
}

func TestReadAndCheckStreamInitialError_LongStream(t *testing.T) {
	lines := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		lines = append(lines, "data: {\"type\":\"text.delta\",\"text\":\"chunk"+string(rune('a'+i))+"\"}\n")
	}
	body := strings.Join(lines, "\n") + "\n"
	resp := testResp(body)
	b, hit, err := ReadAndCheckStreamInitialError(resp)
	if err != nil || hit || b != nil {
		t.Fatalf("want no error/hit on long stream, got err=%v hit=%v", err, hit)
	}
	rest, _ := io.ReadAll(resp.Body)
	if len(rest) != len(body) {
		t.Fatalf("long stream truncated: got %d want %d", len(rest), len(body))
	}
}

func TestHandleStreamForwarding_Correctness(t *testing.T) {
	body := "data: {\"type\":\"text.delta\",\"text\":\"hi\"}\n\n"
	resp := testResp(body)
	rec := &recordingWriter{}
	if err := HandleStreamForwarding(rec, resp); err != nil {
		t.Fatalf("forward err: %v", err)
	}
	if rec.s != body {
		t.Fatalf("forwarded mismatch: %q", rec.s)
	}
}

type recordingWriter struct {
	s   string
	hdr http.Header
}

func (r *recordingWriter) Header() http.Header {
	if r.hdr == nil {
		r.hdr = http.Header{}
	}
	return r.hdr
}
func (r *recordingWriter) Write(p []byte) (int, error) {
	r.s += string(p)
	return len(p), nil
}
func (r *recordingWriter) WriteHeader(int) {}
func (r *recordingWriter) Flush()          {}
