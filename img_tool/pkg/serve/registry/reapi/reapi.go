package reapi

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	registry "github.com/bazel-contrib/rules_img/img_tool/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"google.golang.org/grpc"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	combined "github.com/bazel-contrib/rules_img/img_tool/pkg/serve/registry"
)

// casStore is the part of *cas.CAS the handler uses; tests substitute a fake.
type casStore interface {
	ReaderForBlob(ctx context.Context, digest cas.Digest) (io.ReadCloser, error)
	FindMissingBlobs(ctx context.Context, digests []cas.Digest) ([]cas.Digest, error)
	WriteBlob(ctx context.Context, digest cas.Digest, r io.Reader) error
}

type REAPIBlobHandler struct {
	upstream      registry.BlobStatHandler
	casReader     casStore
	blobSizeCache *combined.BlobSizeCache
	spoolDir      string // "" means os.TempDir()
}

func New(upstream registry.BlobStatHandler, clientConn *grpc.ClientConn, blobSizeCache *combined.BlobSizeCache) (*REAPIBlobHandler, error) {
	casReader, err := cas.New(clientConn, cas.WithLearnCapabilities(true))
	if err != nil {
		return nil, err
	}

	return &REAPIBlobHandler{
		upstream:      upstream,
		casReader:     casReader,
		blobSizeCache: blobSizeCache,
	}, nil
}

func (h *REAPIBlobHandler) Get(ctx context.Context, repo string, hash registryv1.Hash) (io.ReadCloser, error) {
	// since we need to know the size of the blob for any REAPI operations,
	// we ask the cache or upstream registry to find out if the blob exists.
	var upstreamSize int64
	if cachedSize, ok := h.blobSizeCache.Get(hash); ok {
		upstreamSize = cachedSize
	} else {
		var upstreamErr error
		upstreamSize, upstreamErr = h.upstream.Stat(ctx, repo, hash)
		if upstreamErr != nil {
			return nil, upstreamErr
		}
	}

	if upstreamSize < 0 {
		return nil, errors.New("unexpected negative blob size")
	}

	digest, err := digestFromDescriptor(hash, upstreamSize)
	if err != nil {
		return nil, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
	}
	return h.casReader.ReaderForBlob(ctx, digest)
}

func (h *REAPIBlobHandler) Stat(ctx context.Context, repo string, hash registryv1.Hash) (int64, error) {
	// since we need to know the size of the blob for any REAPI operations,
	// we ask the cache or upstream registry to find out if the blob exists.
	var upstreamSize int64
	if cachedSize, ok := h.blobSizeCache.Get(hash); ok {
		upstreamSize = cachedSize
	} else {
		var upstreamErr error
		upstreamSize, upstreamErr = h.upstream.Stat(ctx, repo, hash)
		if upstreamErr != nil {
			return 0, upstreamErr
		}
	}
	if upstreamSize == 0 {
		return 0, nil
	}

	digest, err := digestFromDescriptor(hash, upstreamSize)
	if err != nil {
		return 0, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
	}
	missing, err := h.casReader.FindMissingBlobs(ctx, []cas.Digest{digest})
	if err != nil {
		return 0, err
	}
	if len(missing) == 0 {
		return upstreamSize, nil // Blob is present.
	}
	return 0, registry.ErrNotFound // Blob is missing.
}

// FindMissingBlobs reports which of the given blobs the CAS does not have.
//
// Asking is also what keeps a blob resident: a CAS that evicts by least recent
// use counts this as a use, so a caller can keep blobs it still needs from
// aging out without transferring any of their data.
func (h *REAPIBlobHandler) FindMissingBlobs(ctx context.Context, digests []cas.Digest) ([]cas.Digest, error) {
	return h.casReader.FindMissingBlobs(ctx, digests)
}

// Put writes a blob to the CAS. A REAPI write needs the size up front: a size the cache or
// upstream already knows streams straight through; an unknown one (a base layer a push
// uploads for the first time) is spooled to disk, counted and hashed first.
func (h *REAPIBlobHandler) Put(ctx context.Context, repo string, hash v1.Hash, rc io.ReadCloser) error {
	defer rc.Close() // Ensure the reader is closed after use.
	size, known, err := h.knownSize(ctx, repo, hash)
	if err != nil {
		return err
	}
	if !known {
		return h.putSpooled(ctx, hash, rc)
	}
	digest, err := digestFromDescriptor(hash, size)
	if err != nil {
		return err
	}
	return h.casReader.WriteBlob(ctx, digest, rc)
}

// knownSize reports the size the cache or upstream has for hash; not found is (0, false, nil).
func (h *REAPIBlobHandler) knownSize(ctx context.Context, repo string, hash v1.Hash) (int64, bool, error) {
	if cachedSize, ok := h.blobSizeCache.Get(hash); ok {
		return cachedSize, true, nil
	}
	size, err := h.upstream.Stat(ctx, repo, hash)
	if errors.Is(err, registry.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return size, true, nil
}

// putSpooled learns an unknown blob's size by spooling it to disk, never memory: base layers
// run to hundreds of MB. The digest is checked here too, so nothing unverified reaches the CAS.
func (h *REAPIBlobHandler) putSpooled(ctx context.Context, want v1.Hash, rc io.Reader) error {
	hasher, err := newHasher(want.Algorithm)
	if err != nil {
		return err
	}
	spool, err := os.CreateTemp(h.spoolDir, "reapi-put-*")
	if err != nil {
		return fmt.Errorf("spooling blob %s: %w", want, err)
	}
	defer os.Remove(spool.Name())
	defer spool.Close()

	size, err := io.Copy(io.MultiWriter(spool, hasher), rc)
	if err != nil {
		return fmt.Errorf("spooling blob %s: %w", want, err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != want.Hex {
		return fmt.Errorf("blob digest mismatch: want %s, got %s:%s", want, want.Algorithm, got)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding spooled blob %s: %w", want, err)
	}
	digest, err := digestFromDescriptor(want, size)
	if err != nil {
		return err
	}
	if err := h.casReader.WriteBlob(ctx, digest, spool); err != nil {
		return err
	}
	h.blobSizeCache.Set(want, size)
	return nil
}

func newHasher(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("unsupported digest algorithm: %s", algorithm)
}

func digestFromDescriptor(hash registryv1.Hash, size int64) (cas.Digest, error) {
	rawHash, err := hex.DecodeString(hash.Hex)
	if err != nil {
		return cas.Digest{}, fmt.Errorf("failed to decode digest hash: %w", err)
	}

	switch hash.Algorithm {
	case "sha256":
		return cas.SHA256(rawHash, size), nil
	case "sha512":
		return cas.SHA512(rawHash, size), nil
	}
	return cas.Digest{}, fmt.Errorf("unsupported digest algorithm: %s", hash.Algorithm)
}
