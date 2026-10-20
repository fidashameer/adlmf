package lockservice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"adlmf/internal/types"
)

// EtcdBackend implements Backend against a real etcd cluster.
//
// It speaks etcd's v3 HTTP/JSON gateway rather than the clientv3 gRPC library.
// The semantics are identical — the gateway is a thin translation layer in
// front of the same gRPC service — and it keeps the project free of external
// Go modules. Swapping in clientv3 later means writing another Backend; the
// monitor, the engine and the harness do not change.
//
// The locking protocol is the standard etcd one:
//
//   - A lease is granted with a TTL. If the holder stops renewing, etcd expires
//     the lease and deletes the key automatically. That is what makes a crashed
//     service's lock reclaimable without anybody having to notice the crash.
//   - The lock key is written with a transaction whose comparison is
//     "create_revision == 0", i.e. "only if this key does not exist". Two
//     services racing both issue this txn; etcd serialises them through Raft and
//     exactly one sees succeeded=true. That is the mutual exclusion guarantee,
//     and it comes from consensus rather than from anything we wrote.
//   - The revision etcd assigns to the successful put is used as the fencing
//     token. etcd revisions increase monotonically across the whole keyspace, so
//     a later acquisition always carries a strictly higher token than an earlier
//     one. This is precisely the fencing token Kleppmann's critique asks for,
//     and it comes for free from the store rather than being a counter we
//     maintain and could get wrong.
type EtcdBackend struct {
	endpoint string
	http     *http.Client
	prefix   string

	mu  sync.Mutex
	seq map[types.ServiceID]uint64
	// leases is keyed by fencing token, not by lock.
	//
	// Keying by lock is racy: between a departing holder's delete transaction
	// succeeding and it looking up which lease to revoke, the next holder can
	// already have acquired and overwritten the entry. The departing holder
	// then revokes the NEW holder's lease, etcd deletes their key, and that
	// holder's own release fails. The token is unique per acquisition, so this
	// cannot happen.
	leases map[uint64]leaseRef
	subs   []chan types.LockEvent
	closed bool
}

// NewEtcdBackend connects to etcd at endpoint (e.g. "http://127.0.0.1:2379").
// All keys are written under prefix so a test run cannot collide with anything
// else in the store.
func NewEtcdBackend(endpoint, prefix string) *EtcdBackend {
	return &EtcdBackend{
		endpoint: endpoint,
		prefix:   prefix,
		http:     &http.Client{Timeout: 5 * time.Second},
		seq:      make(map[types.ServiceID]uint64),
		leases:   make(map[uint64]leaseRef),
	}
}

// leaseRef ties one acquisition's lease to the lock it was taken for.
type leaseRef struct {
	id   int64
	lock types.LockID
}

func (b *EtcdBackend) key(lock types.LockID) string { return b.prefix + "/" + string(lock) }

// call posts a JSON body to one of etcd's gateway endpoints.
func (b *EtcdBackend) call(path string, req any, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := b.http.Post(b.endpoint+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("etcd %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("etcd %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func enc(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// Ping checks the cluster is reachable. Used by tests to skip cleanly when no
// etcd is running rather than failing with a confusing error.
func (b *EtcdBackend) Ping() error {
	var out map[string]any
	return b.call("/v3/maintenance/status", map[string]any{}, &out)
}

// grantLease asks etcd for a lease with the given TTL, in seconds.
func (b *EtcdBackend) grantLease(ttl time.Duration) (int64, error) {
	secs := int64(ttl.Seconds())
	if secs < 1 {
		secs = 1 // etcd's minimum granularity
	}
	var out struct {
		ID string `json:"ID"`
	}
	if err := b.call("/v3/lease/grant", map[string]any{"TTL": secs}, &out); err != nil {
		return 0, err
	}
	return strconv.ParseInt(out.ID, 10, 64)
}

// emit publishes an event to subscribers. Caller must hold b.mu.
func (b *EtcdBackend) emit(kind types.EventKind, service types.ServiceID, lock types.LockID, token uint64) {
	b.seq[service]++
	ev := types.LockEvent{Kind: kind, Service: service, Lock: lock, Seq: b.seq[service], Token: token}
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// TryAcquire implements Backend using a create-if-absent transaction.
func (b *EtcdBackend) TryAcquire(service types.ServiceID, lock types.LockID, ttl time.Duration) (Handle, error) {
	leaseID, err := b.grantLease(ttl)
	if err != nil {
		return Handle{}, err
	}

	k := enc(b.key(lock))
	req := map[string]any{
		// "create_revision == 0" is etcd's idiom for "this key does not exist".
		"compare": []map[string]any{{
			"key": k, "target": "CREATE", "result": "EQUAL", "create_revision": "0",
		}},
		"success": []map[string]any{{
			"requestPut": map[string]any{"key": k, "value": enc(string(service)), "lease": strconv.FormatInt(leaseID, 10)},
		}},
		// On failure, read the key back so we can report the current holder.
		"failure": []map[string]any{{
			"requestRange": map[string]any{"key": k},
		}},
	}

	var out struct {
		Header struct {
			Revision string `json:"revision"`
		} `json:"header"`
		Succeeded bool `json:"succeeded"`
	}
	if err := b.call("/v3/kv/txn", req, &out); err != nil {
		return Handle{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if !out.Succeeded {
		// Someone else holds it. Drop the lease we speculatively created so it
		// does not linger until TTL.
		_ = b.call("/v3/lease/revoke", map[string]any{"ID": strconv.FormatInt(leaseID, 10)}, nil)
		b.emit(types.Waiting, service, lock, 0)
		return Handle{}, ErrHeld
	}

	// etcd's revision is the fencing token.
	rev, _ := strconv.ParseUint(out.Header.Revision, 10, 64)
	b.leases[rev] = leaseRef{id: leaseID, lock: lock}
	b.emit(types.Acquired, service, lock, rev)

	return Handle{Service: service, Lock: lock, Token: rev, Expiry: time.Now().Add(ttl)}, nil
}

// Release deletes the lock key, but only if the caller is still the holder.
//
// The guard is a transaction comparing the key's mod_revision against the
// token the caller was given. A holder whose lease lapsed and whose lock was
// taken by someone else will fail this comparison, which is the whole point of
// the fencing token: a stale holder cannot delete the new holder's lock.
func (b *EtcdBackend) Release(h Handle) error {
	k := enc(b.key(h.Lock))
	req := map[string]any{
		"compare": []map[string]any{{
			"key": k, "target": "MOD", "result": "EQUAL", "mod_revision": strconv.FormatUint(h.Token, 10),
		}},
		"success": []map[string]any{{
			"requestDeleteRange": map[string]any{"key": k},
		}},
	}
	var out struct {
		Succeeded bool `json:"succeeded"`
	}
	if err := b.call("/v3/kv/txn", req, &out); err != nil {
		return err
	}
	if !out.Succeeded {
		return ErrNotHolder
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if ref, ok := b.leases[h.Token]; ok {
		_ = b.call("/v3/lease/revoke", map[string]any{"ID": strconv.FormatInt(ref.id, 10)}, nil)
		delete(b.leases, h.Token)
	}
	b.emit(types.Released, h.Service, h.Lock, h.Token)
	return nil
}

// Renew sends a lease keepalive. etcd refuses to keep alive a lease that has
// already expired, which surfaces here as ErrLeaseExpired.
func (b *EtcdBackend) Renew(h Handle, ttl time.Duration) (Handle, error) {
	b.mu.Lock()
	ref, ok := b.leases[h.Token]
	b.mu.Unlock()
	if !ok {
		return Handle{}, ErrNotHolder
	}
	leaseID := ref.id

	var out struct {
		Result struct {
			TTL string `json:"TTL"`
		} `json:"result"`
	}
	if err := b.call("/v3/lease/keepalive", map[string]any{"ID": strconv.FormatInt(leaseID, 10)}, &out); err != nil {
		return Handle{}, err
	}
	// etcd answers a keepalive for a dead lease with TTL 0.
	if out.Result.TTL == "" || out.Result.TTL == "0" {
		return Handle{}, ErrLeaseExpired
	}
	secs, _ := strconv.Atoi(out.Result.TTL)
	return Handle{Service: h.Service, Lock: h.Lock, Token: h.Token, Expiry: time.Now().Add(time.Duration(secs) * time.Second)}, nil
}

// ReapExpired is a no-op against etcd: lease expiry is the server's job, and it
// deletes the key itself when the TTL lapses. The method exists so the fault
// manager can drive both backends through the same interface. It reports how
// many locks it observed as having been reclaimed by etcd since the last call.
func (b *EtcdBackend) ReapExpired() int {
	b.mu.Lock()
	type entry struct {
		token uint64
		lock  types.LockID
	}
	tracked := make([]entry, 0, len(b.leases))
	for tok, ref := range b.leases {
		tracked = append(tracked, entry{tok, ref.lock})
	}
	b.mu.Unlock()

	n := 0
	for _, e := range tracked {
		if _, held := b.Holder(e.lock); !held {
			b.mu.Lock()
			delete(b.leases, e.token)
			b.mu.Unlock()
			n++
		}
	}
	return n
}

// Holder reads the lock key back from etcd.
func (b *EtcdBackend) Holder(lock types.LockID) (types.ServiceID, bool) {
	var out struct {
		Kvs []struct {
			Value string `json:"value"`
		} `json:"kvs"`
	}
	if err := b.call("/v3/kv/range", map[string]any{"key": enc(b.key(lock))}, &out); err != nil {
		return "", false
	}
	if len(out.Kvs) == 0 {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(out.Kvs[0].Value)
	if err != nil {
		return "", false
	}
	return types.ServiceID(raw), true
}

// Subscribe implements Backend.
func (b *EtcdBackend) Subscribe() <-chan types.LockEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan types.LockEvent, 1024)
	b.subs = append(b.subs, ch)
	return ch
}

// Close implements Backend.
func (b *EtcdBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, ch := range b.subs {
		close(ch)
	}
	b.subs = nil
}

var _ Backend = (*EtcdBackend)(nil)
