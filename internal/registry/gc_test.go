package registry

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenLinker-ai/openlinker-registry/internal/blobstore"
	"github.com/OpenLinker-ai/openlinker-registry/internal/model"
	"github.com/OpenLinker-ai/openlinker-registry/internal/skill"
	"github.com/OpenLinker-ai/openlinker-registry/internal/store"
)

var gcMD = []byte("---\nname: gc\ndescription: GC race\nlicense: MIT\n---\nBody.\n")

func newGCService(t *testing.T) *Service {
	t.Helper()
	st := store.NewMemory()
	if _, err := st.CreateNamespace(context.Background(), model.Namespace{Name: "com.example", Kind: "domain"}, model.AuditEvent{}); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, blobs, nil, Config{Mode: "public", AllowPublic: true, PublicURL: "https://r.example"})
	// Make the blob an existing orphan so GC lists it.
	if _, err := blobs.Put(context.Background(), blobstore.Artifacts, gcMD); err != nil {
		t.Fatal(err)
	}
	return svc
}

func publishGC(svc *Service, version string) error {
	_, _, err := svc.PublishUpload(context.Background(), PublishOptions{Actor: "t", Namespace: "com.example", Name: "gc", Version: version},
		UploadSkillMD, gcMD)
	return err
}

// assertServable fails unless the published artifact is still readable.
func assertServable(t *testing.T, svc *Service) {
	t.Helper()
	sk, err := svc.Store.GetSkill(context.Background(), "com.example", "gc")
	if err != nil {
		t.Fatal(err)
	}
	v, err := svc.Store.GetVersion(context.Background(), sk.ID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Materialize(context.Background(), sk, v, false); err != nil {
		t.Fatalf("referenced artifact is gone: %v", err)
	}
}

// A publish has written (refreshed) the blob but not yet committed its
// version when GC, using a cutoff that still treats the blob as old, reaches
// the same digest. GC must wait for the publish and then see the reference.
func TestGCWaitsForInFlightPublish(t *testing.T) {
	svc := newGCService(t)
	digest := skill.Digest(gcMD)
	putDone, releasePublish, gcAtLock := make(chan struct{}), make(chan struct{}), make(chan struct{})
	svc.afterArtifactPut = func(d string) {
		if d == digest {
			close(putDone)
			<-releasePublish
		}
	}
	svc.gcBeforeLock = func(d string) {
		if d == digest {
			close(gcAtLock)
		}
	}
	pubErr := make(chan error, 1)
	go func() { pubErr <- publishGC(svc, "1") }()
	<-putDone

	type gcResult struct {
		n   int
		err error
	}
	gcRes := make(chan gcResult, 1)
	go func() {
		n, err := svc.GCArtifacts(context.Background(), time.Now().Add(time.Hour))
		gcRes <- gcResult{n, err}
	}()
	<-gcAtLock
	// GC has listed the digest as an unreferenced old blob and is about to
	// lock it while the publish still holds its lease. Let the publish commit.
	close(releasePublish)
	if err := <-pubErr; err != nil {
		t.Fatal(err)
	}
	res := <-gcRes
	if res.err != nil || res.n != 0 {
		t.Fatalf("gc removed %d (err %v); the in-flight artifact must be kept", res.n, res.err)
	}
	assertServable(t, svc)
}

// GC has verified the blob is unreferenced and is about to delete it when a
// publish of the same content starts. The publish must not write before the
// deletion, so it rewrites the blob afterwards.
func TestPublishWaitsForInFlightGC(t *testing.T) {
	svc := newGCService(t)
	digest := skill.Digest(gcMD)
	gcHolding, releaseGC := make(chan struct{}), make(chan struct{})
	svc.gcBeforeDelete = func(d string) {
		if d == digest {
			close(gcHolding)
			<-releaseGC
		}
	}
	rec := &deleteRecorder{Store: svc.Blobs}
	svc.Blobs = rec
	putAfterGC := make(chan bool, 1)
	svc.afterArtifactPut = func(string) { putAfterGC <- rec.deleted.Load() }
	gcRes := make(chan int, 1)
	go func() {
		n, _ := svc.GCArtifacts(context.Background(), time.Now().Add(time.Hour))
		gcRes <- n
	}()
	<-gcHolding
	pubErr := make(chan error, 1)
	go func() { pubErr <- publishGC(svc, "1") }()
	close(releaseGC)
	if n := <-gcRes; n != 1 {
		t.Fatalf("gc removed %d, want the orphan removed", n)
	}
	if err := <-pubErr; err != nil {
		t.Fatal(err)
	}
	if !<-putAfterGC {
		t.Fatal("publish wrote the blob while GC held the digest")
	}
	assertServable(t, svc)
}

// deleteRecorder notes when GC has actually removed a blob.
type deleteRecorder struct {
	blobstore.Store
	deleted atomic.Bool
}

func (r *deleteRecorder) DeleteIfOlder(ctx context.Context, a blobstore.Area, d string, t time.Time) (bool, error) {
	ok, err := r.Store.DeleteIfOlder(ctx, a, d, t)
	if ok {
		r.deleted.Store(true)
	}
	return ok, err
}

func TestDigestLocksAreReleased(t *testing.T) {
	var d digestLocks
	d.shared("a")()
	d.exclusive("a")()
	if len(d.m) != 0 {
		t.Fatalf("lock entries leaked: %d", len(d.m))
	}
}
