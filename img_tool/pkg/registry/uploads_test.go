package registry

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// spoolRegistry serves an in-memory blob handler with uploads spooled under a test dir.
func spoolRegistry(t *testing.T, ttl time.Duration) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	return New(WithUploadSpool(dir, ttl), Logger(log.New(io.Discard, "", 0))), dir
}

func serve(t *testing.T, h http.Handler, method, target string, body []byte, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// patchAll uploads blob in chunks of chunk bytes and returns the session's upload path.
func patchAll(t *testing.T, h http.Handler, blob []byte, chunk int) string {
	t.Helper()
	post := serve(t, h, http.MethodPost, "/v2/app/blobs/uploads/", nil, nil)
	if post.Code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", post.Code, post.Body)
	}
	loc := post.Header().Get("Location")
	for off := 0; off < len(blob); off += chunk {
		end := min(off+chunk, len(blob))
		hdr := http.Header{"Content-Range": {fmt.Sprintf("%d-%d", off, end-1)}}
		rec := serve(t, h, http.MethodPatch, loc, blob[off:end], hdr)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("PATCH %d-%d = %d %s", off, end-1, rec.Code, rec.Body)
		}
		if got, want := rec.Header().Get("Range"), fmt.Sprintf("0-%d", end-1); got != want {
			t.Fatalf("Range = %q, want %q", got, want)
		}
	}
	return loc
}

func TestChunkedUploadSpoolsAndCompletes(t *testing.T) {
	h, dir := spoolRegistry(t, 0)
	blob := make([]byte, 3<<20+17) // several chunks, none aligned to the end
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	loc := patchAll(t, h, blob, 1<<20)
	if files := spoolFiles(t, dir); len(files) != 1 {
		t.Fatalf("spool files mid-upload = %v, want one", files)
	}
	put := serve(t, h, http.MethodPut, loc+"?digest="+digest, nil, nil)
	if put.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", put.Code, put.Body)
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Fatalf("spool files after PUT = %v, want none", files)
	}
	get := serve(t, h, http.MethodGet, "/v2/app/blobs/"+digest, nil, nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), blob) {
		t.Fatalf("GET = %d, %d bytes; want 200 and the uploaded %d", get.Code, get.Body.Len(), len(blob))
	}
}

func TestChunkedUploadDigestMismatchRemovesSpool(t *testing.T) {
	h, dir := spoolRegistry(t, 0)
	loc := patchAll(t, h, []byte(strings.Repeat("x", 4096)), 1000)
	wrong := sha256.Sum256([]byte("something else"))

	put := serve(t, h, http.MethodPut, loc+"?digest=sha256:"+hex.EncodeToString(wrong[:]), nil, nil)
	if put.Code != http.StatusBadRequest || !strings.Contains(put.Body.String(), "DIGEST_INVALID") {
		t.Fatalf("PUT = %d %s, want 400 DIGEST_INVALID", put.Code, put.Body)
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Fatalf("spool files after a rejected PUT = %v, want none", files)
	}
}

func TestChunkedUploadOffsetMismatchRejected(t *testing.T) {
	h, _ := spoolRegistry(t, 0)
	loc := patchAll(t, h, []byte("0123456789"), 10)
	rec := serve(t, h, http.MethodPatch, loc, []byte("abc"), http.Header{"Content-Range": {"5-7"}})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("PATCH at a stale offset = %d, want 416", rec.Code)
	}
}

func TestAbandonedUploadIsSwept(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	clock := func(r *registry) { r.blobs.uploads.now = func() time.Time { return now } }
	h := New(WithUploadSpool(dir, time.Minute), clock, Logger(log.New(io.Discard, "", 0)))

	patchAll(t, h, []byte("never finished"), 5)
	if files := spoolFiles(t, dir); len(files) != 1 {
		t.Fatalf("spool files = %v, want one", files)
	}

	now = now.Add(2 * time.Minute)
	// A new upload's POST reclaims sessions idle past the TTL.
	if post := serve(t, h, http.MethodPost, "/v2/app/blobs/uploads/", nil, nil); post.Code != http.StatusAccepted {
		t.Fatalf("POST = %d", post.Code)
	}
	if files := spoolFiles(t, dir); len(files) != 0 {
		t.Fatalf("spool files after the TTL = %v, want none", files)
	}
}
