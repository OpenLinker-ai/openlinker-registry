package registry

import "sync"

// digestLocks serializes artifact garbage collection against publishes of
// the same digest. A publish holds a shared lease on its artifact digest from
// before the blob is written until the version row is committed (or the
// publish fails). GC takes the exclusive lock across the reference re-check
// and the deletion, so a blob is never removed while a publish that will
// reference it is in flight, and a publish that starts after a deletion
// rewrites the blob.
//
// The locks are in-process: the local disk blob store is owned by a single
// registry process. A shared backend (S3) needs a store-level lease instead.
type digestLocks struct {
	mu sync.Mutex
	m  map[string]*digestLock
}

type digestLock struct {
	rw   sync.RWMutex
	refs int
}

func (d *digestLocks) acquire(digest string) *digestLock {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[string]*digestLock{}
	}
	l := d.m[digest]
	if l == nil {
		l = &digestLock{}
		d.m[digest] = l
	}
	l.refs++
	return l
}

func (d *digestLocks) release(digest string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if l := d.m[digest]; l != nil {
		if l.refs--; l.refs == 0 {
			delete(d.m, digest)
		}
	}
}

// shared takes a publish lease; call the returned function to release it.
func (d *digestLocks) shared(digest string) func() {
	l := d.acquire(digest)
	l.rw.RLock()
	return func() { l.rw.RUnlock(); d.release(digest) }
}

// exclusive takes the GC lock; call the returned function to release it.
func (d *digestLocks) exclusive(digest string) func() {
	l := d.acquire(digest)
	l.rw.Lock()
	return func() { l.rw.Unlock(); d.release(digest) }
}
