package reapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	registry "github.com/bazel-contrib/rules_img/img_tool/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	combined "github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry"
)

type fakeCAS struct {
	writes map[string][]byte // hex hash -> bytes
	sizes  map[string]int64  // hex hash -> digest size the write was made with
}

func newFakeCAS() *fakeCAS {
	return &fakeCAS{writes: map[string][]byte{}, sizes: map[string]int64{}}
}

func (f *fakeCAS) ReaderForBlob(context.Context, cas.Digest) (io.ReadCloser, error) {
	return nil, registry.ErrNotFound
}

func (f *fakeCAS) FindMissingBlobs(_ context.Context, digests []cas.Digest) ([]cas.Digest, error) {
	return digests, nil
}

func (f *fakeCAS) WriteBlob(_ context.Context, digest cas.Digest, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	key := hex.EncodeToString(digest.Hash)
	f.writes[key] = data
	f.sizes[key] = digest.SizeBytes
	return nil
}

// notFound is the reapi upstream for a blob nothing has seen: the cas_registry push's case.
type notFound struct{}

func (notFound) Stat(context.Context, string, v1.Hash) (int64, error) {
	return 0, registry.ErrNotFound
}

func hashOf(t *testing.T, data []byte) v1.Hash {
	t.Helper()
	sum := sha256.Sum256(data)
	h, err := v1.NewHash("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func newTestHandler(t *testing.T, store casStore) (*REAPIBlobHandler, *combined.BlobSizeCache) {
	sizes := combined.NewBlobSizeCache()
	return &REAPIBlobHandler{upstream: notFound{}, casReader: store, blobSizeCache: sizes, spoolDir: t.TempDir()}, sizes
}

func TestPutUnknownSizeSpoolsAndWrites(t *testing.T) {
	store := newFakeCAS()
	h, sizes := newTestHandler(t, store)
	data := []byte("a base layer the CAS has never seen")
	hash := hashOf(t, data)

	if err := h.Put(context.Background(), "repo", hash, io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := store.writes[hash.Hex]; !bytes.Equal(got, data) {
		t.Fatalf("CAS got %q, want %q", got, data)
	}
	if got := store.sizes[hash.Hex]; got != int64(len(data)) {
		t.Fatalf("written with size %d, want %d", got, len(data))
	}
	if got, ok := sizes.Get(hash); !ok || got != int64(len(data)) {
		t.Fatalf("size cache = %d, %v; want %d, true", got, ok, len(data))
	}
}

func TestPutUnknownSizeRejectsDigestMismatch(t *testing.T) {
	store := newFakeCAS()
	h, sizes := newTestHandler(t, store)
	hash := hashOf(t, []byte("what the client claims"))

	err := h.Put(context.Background(), "repo", hash, io.NopCloser(strings.NewReader("what it sends")))
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Put err = %v, want a digest mismatch", err)
	}
	if len(store.writes) != 0 {
		t.Fatalf("CAS was written despite the mismatch: %v", store.writes)
	}
	if _, ok := sizes.Get(hash); ok {
		t.Fatal("size cached for a rejected blob")
	}
}

func TestPutKnownSizeStreamsWithCachedSize(t *testing.T) {
	store := newFakeCAS()
	h, sizes := newTestHandler(t, store)
	data := []byte("committed by the push")
	hash := hashOf(t, data)
	sizes.Set(hash, int64(len(data)))

	if err := h.Put(context.Background(), "repo", hash, io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := store.sizes[hash.Hex]; got != int64(len(data)) {
		t.Fatalf("written with size %d, want %d", got, len(data))
	}
}
