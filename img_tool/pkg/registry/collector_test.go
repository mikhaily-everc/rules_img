package registry

import (
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// testClock is a clock the test moves by hand.
type testClock struct {
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestCollectorKeepsWhatATagReaches(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	config := descriptorFor(types.OCIConfigJSON, "config")
	layer := descriptorFor(types.OCILayer, "layer")
	image := imageManifest(t, "image", config, layer)
	imageDigest := digestOf(t, image)
	index := imageIndex(t, "index", manifestDescriptor(image, types.OCIManifestSchema1))
	indexDigest := digestOf(t, index)

	store.PutManifest("app", imageDigest, Manifest{Kind: KindManifest, Blob: image})
	store.PutManifest("app", indexDigest, Manifest{Kind: KindIndex, Blob: index})
	store.PutTag("app", "v1", indexDigest)
	collector.TouchManifest("app", imageDigest)
	collector.TouchManifest("app", indexDigest)
	collector.TouchTag("app", "v1")
	collector.TouchBlob("app", config.Digest, config.Size)
	collector.TouchBlob("app", layer.Digest, layer.Size)

	// Tags never expire by default, so nothing below one does either -- however
	// long ago the manifests and blobs themselves were last touched.
	clock.advance(100 * time.Minute)
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting a fully tagged graph removed %+v, want nothing", stats)
	}
	assertHeld(t, store, "app", imageDigest, indexDigest)
	assertLiveBlobs(t, collector, config.Digest, layer.Digest)

	// Untag it and the whole graph goes, blobs included.
	store.DeleteTag("app", "v1")
	collector.ForgetTag("app", "v1")
	stats := collector.Collect()
	if stats.Manifests != 2 {
		t.Fatalf("collecting an untagged graph removed %d manifests, want 2", stats.Manifests)
	}
	if stats.Blobs != 2 {
		t.Fatalf("collecting an untagged graph removed %d blobs, want 2", stats.Blobs)
	}
	assertNotHeld(t, store, "app", imageDigest, indexDigest)
	assertLiveBlobs(t, collector)
}

func TestCollectorKeepsAChildAnIndexStillNames(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	imageDigest := digestOf(t, image)
	index := imageIndex(t, "index", manifestDescriptor(image, types.OCIManifestSchema1))
	indexDigest := digestOf(t, index)

	store.PutManifest("app", imageDigest, Manifest{Kind: KindManifest, Blob: image})
	collector.TouchManifest("app", imageDigest)

	// The index lands half a TTL later, so its child is past its own expiry
	// while the index is not. Reachability, not age, decides.
	clock.advance(30 * time.Second)
	store.PutManifest("app", indexDigest, Manifest{Kind: KindIndex, Blob: index})
	collector.TouchManifest("app", indexDigest)

	clock.advance(31 * time.Second)
	if stats := collector.Collect(); stats.Manifests != 0 {
		t.Fatalf("collecting removed %d manifests, want 0: the index still names its child", stats.Manifests)
	}
	assertHeld(t, store, "app", imageDigest, indexDigest)

	// Once the index expires too, both go.
	clock.advance(30 * time.Second)
	if stats := collector.Collect(); stats.Manifests != 2 {
		t.Fatalf("collecting an expired index and child removed %d manifests, want 2", stats.Manifests)
	}
}

func TestCollectorKeepsAChildSharedByTwoIndexes(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	imageDigest := digestOf(t, image)
	child := manifestDescriptor(image, types.OCIManifestSchema1)
	first := imageIndex(t, "first", child)
	second := imageIndex(t, "second", child)

	store.PutManifest("app", imageDigest, Manifest{Kind: KindManifest, Blob: image})
	store.PutManifest("app", digestOf(t, first), Manifest{Kind: KindIndex, Blob: first})
	collector.TouchManifest("app", imageDigest)
	collector.TouchManifest("app", digestOf(t, first))

	clock.advance(30 * time.Second)
	store.PutManifest("app", digestOf(t, second), Manifest{Kind: KindIndex, Blob: second})
	collector.TouchManifest("app", digestOf(t, second))

	// The first index and the child have both expired; the second index has not,
	// and it names the same child.
	clock.advance(31 * time.Second)
	if stats := collector.Collect(); stats.Manifests != 1 {
		t.Fatalf("collecting removed %d manifests, want 1: only the expired index", stats.Manifests)
	}
	assertHeld(t, store, "app", imageDigest, digestOf(t, second))
	assertNotHeld(t, store, "app", digestOf(t, first))
}

func TestCollectorFollowsNestedIndexes(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	inner := imageIndex(t, "inner", manifestDescriptor(image, types.OCIManifestSchema1))
	outer := imageIndex(t, "outer", manifestDescriptor(inner, types.OCIImageIndex))

	store.PutManifest("app", digestOf(t, image), Manifest{Kind: KindManifest, Blob: image})
	store.PutManifest("app", digestOf(t, inner), Manifest{Kind: KindIndex, Blob: inner})
	store.PutManifest("app", digestOf(t, outer), Manifest{Kind: KindIndex, Blob: outer})
	store.PutTag("app", "v1", digestOf(t, outer))
	collector.TouchManifest("app", digestOf(t, image))
	collector.TouchManifest("app", digestOf(t, inner))
	collector.TouchManifest("app", digestOf(t, outer))
	collector.TouchTag("app", "v1")

	clock.advance(100 * time.Minute)
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting a tagged nested index removed %+v, want nothing", stats)
	}
	assertHeld(t, store, "app", digestOf(t, image), digestOf(t, inner), digestOf(t, outer))
}

func TestCollectorTerminatesOnCycles(t *testing.T) {
	// Content addressing makes cycles unreachable over HTTP -- a manifest
	// cannot name a digest that depends on its own bytes -- so build them
	// against the store directly. A future store that keys manifests by
	// something other than their content would not have that guarantee.
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	selfDigest := descriptorFor(types.OCIImageIndex, "self").Digest
	otherDigest := descriptorFor(types.OCIImageIndex, "other").Digest
	selfLoop := imageIndex(t, "self", v1.Descriptor{MediaType: types.OCIImageIndex, Digest: selfDigest, Size: 1})
	pointsAtSelf := imageIndex(t, "pair-a", v1.Descriptor{MediaType: types.OCIImageIndex, Digest: otherDigest, Size: 1})
	pointsBack := imageIndex(t, "pair-b", v1.Descriptor{MediaType: types.OCIImageIndex, Digest: selfDigest, Size: 1})

	store.PutManifest("app", selfDigest, Manifest{Kind: KindIndex, Blob: selfLoop})
	store.PutTag("app", "loop", selfDigest)
	collector.TouchManifest("app", selfDigest)
	collector.TouchTag("app", "loop")

	clock.advance(100 * time.Minute)
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting a self-referencing index removed %+v, want nothing", stats)
	}

	// A mutually referencing pair, rooted at one of them.
	store.PutManifest("app", selfDigest, Manifest{Kind: KindIndex, Blob: pointsAtSelf})
	store.PutManifest("app", otherDigest, Manifest{Kind: KindIndex, Blob: pointsBack})
	collector.TouchManifest("app", selfDigest)
	collector.TouchManifest("app", otherDigest)

	clock.advance(100 * time.Minute)
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting a mutually referencing pair removed %+v, want nothing", stats)
	}

	// Unroot the pair and both go, without looping forever getting there.
	store.DeleteTag("app", "loop")
	collector.ForgetTag("app", "loop")
	if stats := collector.Collect(); stats.Manifests != 2 {
		t.Fatalf("collecting an unrooted cycle removed %d manifests, want 2", stats.Manifests)
	}
}

func TestCollectorKeepsReferrersWithTheirSubject(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	config := descriptorFor(types.OCIConfigJSON, "config")
	signatureConfig := descriptorFor(types.OCIConfigJSON, "signature-config")
	image := imageManifest(t, "image", config)
	imageDigest := digestOf(t, image)
	signature := referrerManifest(t, "signature", imageDigest, signatureConfig)
	signatureDigest := digestOf(t, signature)

	store.PutManifest("app", imageDigest, Manifest{Kind: KindManifest, Blob: image})
	store.PutManifest("app", signatureDigest, Manifest{Kind: KindManifest, Blob: signature})
	store.PutTag("app", "v1", imageDigest)
	collector.TouchManifest("app", imageDigest)
	collector.TouchManifest("app", signatureDigest)
	collector.TouchTag("app", "v1")
	collector.TouchBlob("app", config.Digest, config.Size)
	collector.TouchBlob("app", signatureConfig.Digest, signatureConfig.Size)

	// The signature is nobody's child, but its subject is tagged, so it stays --
	// and so does the blob it names.
	clock.advance(100 * time.Minute)
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting removed %+v, want nothing: the signature's subject is tagged", stats)
	}
	assertHeld(t, store, "app", imageDigest, signatureDigest)
	assertLiveBlobs(t, collector, config.Digest, signatureConfig.Digest)

	// Once the subject goes, the signature has nothing to describe.
	store.DeleteTag("app", "v1")
	collector.ForgetTag("app", "v1")
	if stats := collector.Collect(); stats.Manifests != 2 {
		t.Fatalf("collecting removed %d manifests, want 2: subject and referrer", stats.Manifests)
	}
}

func TestCollectorDoesNotLetAReferrerRootItsSubject(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	imageDigest := digestOf(t, image)
	signature := referrerManifest(t, "signature", imageDigest, descriptorFor(types.OCIConfigJSON, "signature-config"))

	store.PutManifest("app", imageDigest, Manifest{Kind: KindManifest, Blob: image})
	collector.TouchManifest("app", imageDigest)

	// The signature is fresh, the image it points at is not. The subject edge
	// only runs the other way: a referrer does not keep its subject alive.
	clock.advance(61 * time.Second)
	store.PutManifest("app", digestOf(t, signature), Manifest{Kind: KindManifest, Blob: signature})
	collector.TouchManifest("app", digestOf(t, signature))

	if stats := collector.Collect(); stats.Manifests != 1 {
		t.Fatalf("collecting removed %d manifests, want 1: the expired subject", stats.Manifests)
	}
	assertNotHeld(t, store, "app", imageDigest)
	assertHeld(t, store, "app", digestOf(t, signature))
}

func TestCollectorExpiresTagsOnlyWithATagTTL(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, TagTTL: 10 * time.Minute, Clock: clock.Now})

	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	imageDigest := digestOf(t, image)
	store.PutManifest("app", imageDigest, Manifest{Kind: KindManifest, Blob: image})
	store.PutTag("app", "v1", imageDigest)
	collector.TouchManifest("app", imageDigest)
	collector.TouchTag("app", "v1")

	// The manifest is well past its own TTL but the tag is not.
	clock.advance(5 * time.Minute)
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting removed %+v while the tag was fresh, want nothing", stats)
	}

	clock.advance(6 * time.Minute)
	stats := collector.Collect()
	if stats.Tags != 1 || stats.Manifests != 1 {
		t.Fatalf("collecting an expired tag removed %+v, want 1 tag and 1 manifest", stats)
	}
	if _, ok := store.ResolveTag("app", "v1"); ok {
		t.Fatal("an expired tag still resolves")
	}
}

func TestCollectorRefreshesATagThatIsRead(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, TagTTL: time.Minute, Clock: clock.Now})

	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	store.PutManifest("app", digestOf(t, image), Manifest{Kind: KindManifest, Blob: image})
	store.PutTag("app", "v1", digestOf(t, image))
	collector.TouchManifest("app", digestOf(t, image))
	collector.TouchTag("app", "v1")

	for range 5 {
		clock.advance(50 * time.Second)
		collector.TouchTag("app", "v1")
		if stats := collector.Collect(); stats != (CollectStats{}) {
			t.Fatalf("collecting removed %+v from a tag that keeps being read, want nothing", stats)
		}
	}
}

func TestCollectorDropsDanglingTags(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	missing := descriptorFor(types.OCIManifestSchema1, "never pushed").Digest
	store.PutTag("app", "v1", missing)
	collector.TouchTag("app", "v1")

	clock.advance(time.Minute)
	if stats := collector.Collect(); stats.Tags != 1 {
		t.Fatalf("collecting removed %d tags, want 1: the tag points at nothing", stats.Tags)
	}
}

func TestCollectorKeepsARecentlyUsedBlobWithNoManifest(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	// A layer is uploaded before the manifest that will name it. Sweeping in
	// between must not take it.
	layer := descriptorFor(types.OCILayer, "layer")
	collector.TouchBlob("app", layer.Digest, layer.Size)

	clock.advance(30 * time.Second)
	if stats := collector.Collect(); stats.Blobs != 0 {
		t.Fatalf("collecting took %d freshly uploaded blobs, want 0", stats.Blobs)
	}

	clock.advance(31 * time.Second)
	if stats := collector.Collect(); stats.Blobs != 1 {
		t.Fatalf("collecting took %d expired unreferenced blobs, want 1", stats.Blobs)
	}
}

func TestCollectorReportsCollectedBlobsOnce(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	var collected []v1.Hash
	collector.OnBlobCollected(func(repo string, digest v1.Hash) {
		if repo != "app" {
			t.Errorf("collected blob reported for repository %q, want app", repo)
		}
		collected = append(collected, digest)
	})

	config := descriptorFor(types.OCIConfigJSON, "config")
	layer := descriptorFor(types.OCILayer, "layer")
	image := imageManifest(t, "image", config, layer)
	store.PutManifest("app", digestOf(t, image), Manifest{Kind: KindManifest, Blob: image})
	store.PutTag("app", "v1", digestOf(t, image))
	collector.TouchManifest("app", digestOf(t, image))
	collector.TouchTag("app", "v1")
	collector.TouchBlob("app", config.Digest, config.Size)
	collector.TouchBlob("app", layer.Digest, layer.Size)

	clock.advance(100 * time.Minute)
	collector.Collect()
	if len(collected) != 0 {
		t.Fatalf("blobs %v were reported collected while their manifest was tagged", collected)
	}

	store.DeleteTag("app", "v1")
	collector.ForgetTag("app", "v1")
	collector.Collect()
	if len(collected) != 2 {
		t.Fatalf("collecting reported %v, want the config and layer blobs", collected)
	}

	collected = nil
	collector.Collect()
	if len(collected) != 0 {
		t.Fatalf("a second sweep reported %v again, want nothing", collected)
	}
}

func TestCollectorAdoptsObjectsItHasNotSeen(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, Clock: clock.Now})

	// A push that landed while a sweep was already running has no node yet.
	// Adopting it means nothing is ever swept on the sweep that first sees it.
	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	store.PutManifest("app", digestOf(t, image), Manifest{Kind: KindManifest, Blob: image})

	clock.advance(100 * time.Minute)
	if stats := collector.Collect(); stats.Manifests != 0 {
		t.Fatalf("the first sweep to see a manifest removed %d, want 0", stats.Manifests)
	}
	assertHeld(t, store, "app", digestOf(t, image))

	clock.advance(61 * time.Second)
	if stats := collector.Collect(); stats.Manifests != 1 {
		t.Fatalf("the next sweep removed %d manifests, want 1", stats.Manifests)
	}
}

func TestCollectorWithoutATTLTracksButNeverCollects(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{Clock: clock.Now})

	config := descriptorFor(types.OCIConfigJSON, "config")
	image := imageManifest(t, "image", config)
	store.PutManifest("app", digestOf(t, image), Manifest{Kind: KindManifest, Blob: image})
	collector.TouchManifest("app", digestOf(t, image))
	collector.TouchBlob("app", config.Digest, config.Size)

	clock.advance(365 * 24 * time.Hour)
	collector.MaybeCollect()
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("a collector with no TTL removed %+v, want nothing", stats)
	}
	// It still knows the live set, which is why the keepalive can use one.
	assertLiveBlobs(t, collector, config.Digest)
}

func TestNilCollectorIsInert(t *testing.T) {
	var collector *Collector
	digest := descriptorFor(types.OCILayer, "layer").Digest

	collector.TouchManifest("app", digest)
	collector.TouchTag("app", "v1")
	collector.TouchBlob("app", digest, 1)
	collector.ForgetManifest("app", digest)
	collector.ForgetTag("app", "v1")
	collector.ForgetBlob(digest)
	collector.OnBlobCollected(func(string, v1.Hash) { t.Fatal("a nil collector collected something") })
	collector.MaybeCollect()
	collector.RangeLiveBlobs(func(LiveBlob) bool { t.Fatal("a nil collector reported a live blob"); return false })
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("a nil collector removed %+v, want nothing", stats)
	}
	if collector.Store() != nil {
		t.Fatal("a nil collector reported a store")
	}
}

func TestCollectorMaybeCollectRespectsItsInterval(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: 10 * time.Minute, Clock: clock.Now})

	image := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	store.PutManifest("app", digestOf(t, image), Manifest{Kind: KindManifest, Blob: image})
	collector.TouchManifest("app", digestOf(t, image))

	// Expired, but the interval -- a tenth of the TTL -- has not elapsed since
	// the collector was created, so no sweep happens yet.
	clock.advance(30 * time.Second)
	collector.MaybeCollect()
	assertHeld(t, store, "app", digestOf(t, image))

	clock.advance(11 * time.Minute)
	collector.MaybeCollect()
	assertNotHeld(t, store, "app", digestOf(t, image))
}

func TestCollectorLiveBlobsCarrySizes(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{Clock: clock.Now})

	layer := descriptorFor(types.OCILayer, "a layer with some content")
	config := descriptorFor(types.OCIConfigJSON, "config")
	image := imageManifest(t, "image", config, layer)
	store.PutManifest("app", digestOf(t, image), Manifest{Kind: KindManifest, Blob: image})
	collector.TouchManifest("app", digestOf(t, image))

	// Sizes come from the manifest's descriptors even for blobs no client has
	// touched, which is what lets them be looked up in a content-addressed
	// store.
	sizes := map[v1.Hash]int64{}
	collector.RangeLiveBlobs(func(blob LiveBlob) bool {
		sizes[blob.Digest] = blob.Size
		return true
	})
	if sizes[layer.Digest] != layer.Size {
		t.Fatalf("live layer size got %d, want %d", sizes[layer.Digest], layer.Size)
	}
	if sizes[config.Digest] != config.Size {
		t.Fatalf("live config size got %d, want %d", sizes[config.Digest], config.Size)
	}
}

func assertHeld(t *testing.T, store Store, repo string, digests ...v1.Hash) {
	t.Helper()
	for _, digest := range digests {
		if _, ok := store.GetManifest(repo, digest); !ok {
			t.Fatalf("manifest %s was collected, want it kept", digest)
		}
	}
}

func assertNotHeld(t *testing.T, store Store, repo string, digests ...v1.Hash) {
	t.Helper()
	for _, digest := range digests {
		if _, ok := store.GetManifest(repo, digest); ok {
			t.Fatalf("manifest %s was kept, want it collected", digest)
		}
	}
}

func assertLiveBlobs(t *testing.T, collector *Collector, want ...v1.Hash) {
	t.Helper()
	live := map[v1.Hash]struct{}{}
	collector.RangeLiveBlobs(func(blob LiveBlob) bool {
		live[blob.Digest] = struct{}{}
		return true
	})
	if len(live) != len(want) {
		t.Fatalf("%d blobs are live, want %d", len(live), len(want))
	}
	for _, digest := range want {
		if _, ok := live[digest]; !ok {
			t.Fatalf("blob %s is not live, want it live", digest)
		}
	}
}

// pushImage stores a distinct image in repo under tags, as a push would.
func pushImage(t *testing.T, store Store, collector *Collector, repo, seed string, tags ...string) v1.Hash {
	t.Helper()
	image := imageManifest(t, seed, descriptorFor(types.OCIConfigJSON, "config-"+seed))
	digest := digestOf(t, image)
	store.PutManifest(repo, digest, Manifest{Kind: KindManifest, Blob: image})
	collector.TouchManifest(repo, digest)
	for _, tag := range tags {
		store.PutTag(repo, tag, digest)
		collector.PushTag(repo, tag)
	}
	return digest
}

func TestCollectorKeepsTheNewestImagesPerRepo(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, TagTTL: time.Hour, TagKeepLast: 2, Clock: clock.Now})

	first := pushImage(t, store, collector, "app", "1", "20261001.000000-aaaaaaa")
	clock.advance(time.Second)
	second := pushImage(t, store, collector, "app", "2", "20261002.000000-bbbbbbb")
	clock.advance(time.Second)
	// latest and a dated tag on the same build are one image, not two slots.
	third := pushImage(t, store, collector, "app", "3", "20261003.000000-ccccccc", "latest")
	other := pushImage(t, store, collector, "other", "4", "v1")

	clock.advance(2 * time.Minute)
	stats := collector.Collect()
	if stats.Tags != 1 || stats.Manifests != 1 {
		t.Fatalf("collecting removed %+v, want the oldest image's 1 tag and 1 manifest", stats)
	}
	assertNotHeld(t, store, "app", first)
	assertHeld(t, store, "app", second, third)
	assertHeld(t, store, "other", other)

	// The newest images stay whatever their age: TagTTL only bounds older ones.
	clock.advance(48 * time.Hour)
	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting removed %+v from the newest images, want nothing", stats)
	}
}

func TestCollectorKeepsAnOlderImageWhileItIsRead(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, TagTTL: time.Hour, TagKeepLast: 1, Clock: clock.Now})

	pinned := pushImage(t, store, collector, "app", "pinned", "v1")
	clock.advance(time.Second)
	pushImage(t, store, collector, "app", "newer", "v2")

	// A deployment pinned to v1 keeps reading it, so it outlives its slot.
	for range 3 {
		clock.advance(50 * time.Minute)
		collector.TouchTag("app", "v1")
		if stats := collector.Collect(); stats != (CollectStats{}) {
			t.Fatalf("collecting removed %+v from a tag still being read, want nothing", stats)
		}
	}
	assertHeld(t, store, "app", pinned)

	clock.advance(61 * time.Minute)
	if stats := collector.Collect(); stats.Tags != 1 || stats.Manifests != 1 {
		t.Fatalf("collecting removed %+v once v1 went unread, want 1 tag and 1 manifest", stats)
	}
	assertNotHeld(t, store, "app", pinned)
}

// pushTimesStore is a Store that remembers push times, as s3store does after Open.
type pushTimesStore struct {
	Store
	pushed map[string]time.Time
}

func (s pushTimesStore) TagPushedAt(repo, tag string) (time.Time, bool) {
	pushed, ok := s.pushed[repo+":"+tag]
	return pushed, ok
}

func TestCollectorRanksAdoptedTagsByTheStoresPushTimes(t *testing.T) {
	clock := newTestClock()
	mem := NewMemStore()
	base := clock.Now()
	store := pushTimesStore{Store: mem, pushed: map[string]time.Time{
		// Name order says b is newer; the store knows a was pushed last.
		"app:a": base.Add(-time.Hour),
		"app:b": base.Add(-2 * time.Hour),
	}}
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, TagTTL: time.Hour, TagKeepLast: 1, Clock: clock.Now})

	// Loaded at startup: present in the store, never seen by this Collector.
	var digests []v1.Hash
	for _, tag := range []string{"a", "b"} {
		image := imageManifest(t, tag, descriptorFor(types.OCIConfigJSON, "config-"+tag))
		digest := digestOf(t, image)
		mem.PutManifest("app", digest, Manifest{Kind: KindManifest, Blob: image})
		mem.PutTag("app", tag, digest)
		digests = append(digests, digest)
	}

	if stats := collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("the sweep that adopts the tags removed %+v, want nothing", stats)
	}
	clock.advance(61 * time.Minute)
	if stats := collector.Collect(); stats.Tags != 1 {
		t.Fatalf("collecting removed %d tags, want 1", stats.Tags)
	}
	if _, ok := mem.ResolveTag("app", "a"); !ok {
		t.Fatal("the most recently pushed tag was collected")
	}
	assertNotHeld(t, mem, "app", digests[1])
}

func TestCollectorBreaksPushTiesByTagName(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, TagTTL: time.Hour, TagKeepLast: 1, Clock: clock.Now})

	older := pushImage(t, store, collector, "app", "1", "20261001.000000-aaaaaaa")
	newer := pushImage(t, store, collector, "app", "2", "20261002.000000-bbbbbbb")

	clock.advance(2 * time.Hour)
	collector.Collect()
	assertHeld(t, store, "app", newer)
	assertNotHeld(t, store, "app", older)
}

func TestCollectorDoesNotGiveADanglingTagASlot(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute, TagTTL: time.Hour, TagKeepLast: 1, Clock: clock.Now})

	kept := pushImage(t, store, collector, "app", "1", "v1")
	clock.advance(time.Second)
	store.PutTag("app", "v2", descriptorFor(types.OCIManifestSchema1, "never pushed").Digest)
	collector.PushTag("app", "v2")

	clock.advance(2 * time.Hour)
	if stats := collector.Collect(); stats.Tags != 1 {
		t.Fatalf("collecting removed %d tags, want only the dangling one", stats.Tags)
	}
	assertHeld(t, store, "app", kept)
}

func TestCollectorMaybeCollectRunsWithOnlyTagKeepLast(t *testing.T) {
	clock := newTestClock()
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TagKeepLast: 1, Interval: time.Minute, Clock: clock.Now})

	older := pushImage(t, store, collector, "app", "1", "v1")
	clock.advance(time.Second)
	pushImage(t, store, collector, "app", "2", "v2")

	clock.advance(time.Minute)
	collector.MaybeCollect()
	if _, ok := store.ResolveTag("app", "v1"); ok {
		t.Fatal("MaybeCollect never swept: the unread older tag still resolves")
	}
	// TTL 0 keeps untagged manifests, so only the tag goes.
	assertHeld(t, store, "app", older)
}
